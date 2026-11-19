package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"agrisync/internal/model"
)

// ---------------------------------------------------------------------------
// Phenology evidence ingest
// ---------------------------------------------------------------------------

// IngestPhenologyEvidence stores the season rule, variety catalog, sowing
// facts, stage calendars, weather stations/links and temperature/stage
// observations for one rule in a region's own database. All of it is scoped by
// rule_id, which is what binds derivation and aggregation to a single season
// rule version.
//
// Sowing facts and temperature observations are revisioned: uploading a higher
// revision marks prior revisions superseded. Existing phenology runs are
// never touched, so a correction can only ever affect a future run.
func (s *Service) IngestPhenologyEvidence(ctx context.Context, region string, ev model.PhenologyEvidence, actor string) (model.PhenologyIngestResult, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.PhenologyIngestResult{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if strings.TrimSpace(ev.BatchID) == "" {
		return model.PhenologyIngestResult{}, fmt.Errorf("%w: batch_id is required", ErrInvalid)
	}
	if err := validateSeasonRule(ev.Rule); err != nil {
		return model.PhenologyIngestResult{}, err
	}
	ruleID := ev.Rule.RuleID
	contentHash := sha256Hex(jsonBytes(ev))
	res := model.PhenologyIngestResult{
		Region: region, BatchID: ev.BatchID, RuleID: ruleID,
		Counts: map[string]int{},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return res, err
	}
	defer tx.Rollback(ctx)

	var prevHash string
	var prevCounts []byte
	err = tx.QueryRow(ctx, `select content_sha256, counts from phenology_ingest_receipts where batch_id=$1`, ev.BatchID).
		Scan(&prevHash, &prevCounts)
	if err == nil {
		if prevHash != contentHash {
			return res, fmt.Errorf("%w: batch_id %s was already used with different content", ErrConflict, ev.BatchID)
		}
		res.Replayed = true
		_ = json.Unmarshal(prevCounts, &res.Counts)
		return res, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return res, err
	}

	if err := upsertSeasonRule(ctx, tx, ev.Rule); err != nil {
		return res, err
	}
	for _, v := range ev.Varieties {
		if v.VarietyCode == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into phenology_varieties (rule_id, variety_code, crop, maturity_group, das_thresholds, gdd, updated_at)
			values ($1,$2,$3,$4,$5,$6,now())
			on conflict (rule_id, variety_code) do update
			  set crop=excluded.crop, maturity_group=excluded.maturity_group,
			      das_thresholds=excluded.das_thresholds, gdd=excluded.gdd, updated_at=now()`,
			ruleID, v.VarietyCode, v.Crop, v.MaturityGroup, jsonBytes(v.DASThresholds), nullableJSON(v.GDD)); err != nil {
			return res, err
		}
		res.Counts["varieties"]++
	}
	for _, p := range ev.Plots {
		if p.PlotID == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into phenology_plots (plot_id, crop, declared_area_ha, note)
			values ($1,$2,$3,$4)
			on conflict (plot_id) do update
			  set crop=excluded.crop, declared_area_ha=excluded.declared_area_ha, note=excluded.note`,
			p.PlotID, p.Crop, p.DeclaredAreaHa, p.Note); err != nil {
			return res, err
		}
		res.Counts["plots"]++
	}
	for _, c := range ev.StageCalendars {
		if c.CalendarID == "" || c.Stage == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into stage_calendars (rule_id, calendar_id, crop, variety_code, stage, start_day, end_day, source, published_at)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			on conflict (rule_id, calendar_id) do update
			  set crop=excluded.crop, variety_code=excluded.variety_code, stage=excluded.stage,
			      start_day=excluded.start_day, end_day=excluded.end_day, source=excluded.source,
			      published_at=excluded.published_at`,
			ruleID, c.CalendarID, c.Crop, c.VarietyCode, c.Stage, c.StartDay, c.EndDay, c.Source,
			c.PublishedAt.UTC()); err != nil {
			return res, err
		}
		res.Counts["stage_calendars"]++
	}
	for _, st := range ev.Stations {
		if st.StationID == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into weather_stations (rule_id, station_id, label, elevation_m)
			values ($1,$2,$3,$4)
			on conflict (rule_id, station_id) do update
			  set label=excluded.label, elevation_m=excluded.elevation_m`,
			ruleID, st.StationID, st.Label, st.ElevationM); err != nil {
			return res, err
		}
		res.Counts["stations"]++
	}
	for _, l := range ev.StationLinks {
		if l.PlotID == "" || l.StationID == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into station_links (rule_id, plot_id, station_id, distance_km, elevation_delta_m)
			values ($1,$2,$3,$4,$5)
			on conflict (rule_id, plot_id) do update
			  set station_id=excluded.station_id, distance_km=excluded.distance_km,
			      elevation_delta_m=excluded.elevation_delta_m`,
			ruleID, l.PlotID, l.StationID, l.DistanceKm, l.ElevationDeltaM); err != nil {
			return res, err
		}
		res.Counts["station_links"]++
	}

	acceptedFacts := 0
	for _, f := range ev.SowingFacts {
		if err := validateSowingFact(f); err != nil {
			return res, err
		}
		// A new revision of a fact supersedes every lower active revision.
		if _, err := tx.Exec(ctx, `
			update sowing_facts set status='superseded'
			where rule_id=$1 and fact_id=$2 and revision<$3 and status='active'`,
			ruleID, f.FactID, f.Revision); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `
			insert into sowing_facts (fact_id, revision, rule_id, plot_id, segment_no, crop, variety_code,
				sown_on, area_ha, event_type, evidence_ref, status, recorded_at)
			values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
			on conflict (fact_id, revision) do nothing`,
			f.FactID, f.Revision, ruleID, f.PlotID, f.SegmentNo, f.Crop, f.VarietyCode,
			f.SownOn, f.AreaHa, f.EventType, f.EvidenceRef, statusOrActive(f.Status), f.RecordedAt.UTC()); err != nil {
			return res, err
		}
		acceptedFacts++
	}
	res.Counts["sowing_facts"] = acceptedFacts

	acceptedObs := 0
	for _, o := range ev.TemperatureObservations {
		if err := validateTemperatureObservation(o); err != nil {
			return res, err
		}
		// A higher revision supersedes lower revisions for the same station-day.
		if _, err := tx.Exec(ctx, `
			update temperature_observations set status='superseded'
			where rule_id=$1 and station_id=$2 and obs_date=$3 and revision<$4 and status='active'`,
			ruleID, o.StationID, o.ObsDate, o.Revision); err != nil {
			return res, err
		}
		if _, err := tx.Exec(ctx, `
			insert into temperature_observations (rule_id, station_id, obs_date, revision, tmin_c, tmax_c,
				source, status, recorded_at)
			values ($1,$2,$3,$4,$5,$6,$7,'active',$8)
			on conflict (rule_id, station_id, obs_date, revision) do nothing`,
			ruleID, o.StationID, o.ObsDate, o.Revision, o.TMinC, o.TMaxC, o.Source, o.RecordedAt.UTC()); err != nil {
			return res, err
		}
		acceptedObs++
	}
	res.Counts["temperature_observations"] = acceptedObs

	acceptedStageObs := 0
	for _, so := range ev.StageObservations {
		if so.ObservationID == "" || so.PlotID == "" || model.PhenologyStageRank(so.Stage) == 0 {
			continue
		}
		if _, err := tx.Exec(ctx, `
			insert into stage_observations (rule_id, observation_id, plot_id, segment_no, stage, observed_on, source)
			values ($1,$2,$3,$4,$5,$6,$7)
			on conflict (rule_id, observation_id) do update
			  set plot_id=excluded.plot_id, segment_no=excluded.segment_no, stage=excluded.stage,
			      observed_on=excluded.observed_on, source=excluded.source`,
			ruleID, so.ObservationID, so.PlotID, so.SegmentNo, so.Stage, so.ObservedOn, so.Source); err != nil {
			return res, err
		}
		acceptedStageObs++
	}
	res.Counts["stage_observations"] = acceptedStageObs

	if _, err := tx.Exec(ctx, `
		insert into phenology_ingest_receipts (batch_id, content_sha256, rule_id, counts)
		values ($1,$2,$3,$4)`,
		ev.BatchID, contentHash, ruleID, jsonBytes(res.Counts)); err != nil {
		return res, err
	}
	if err := tx.Commit(ctx); err != nil {
		return res, err
	}
	return res, nil
}

func validateSeasonRule(r model.SeasonRule) error {
	if strings.TrimSpace(r.RuleID) == "" {
		return fmt.Errorf("%w: rule.rule_id is required", ErrInvalid)
	}
	if strings.TrimSpace(r.SeasonID) == "" || strings.TrimSpace(r.RuleVersion) == "" {
		return fmt.Errorf("%w: rule.season_id and rule.rule_version are required", ErrInvalid)
	}
	if r.Crop == "" {
		return fmt.Errorf("%w: rule.crop is required", ErrInvalid)
	}
	if r.Hemisphere != "north" && r.Hemisphere != "south" {
		return fmt.Errorf("%w: rule.hemisphere must be north or south", ErrInvalid)
	}
	return nil
}

func validateSowingFact(f model.SowingFact) error {
	if f.FactID == "" || f.PlotID == "" || f.Crop == "" {
		return fmt.Errorf("%w: sowing fact needs fact_id, plot_id and crop", ErrInvalid)
	}
	if f.Revision <= 0 {
		return fmt.Errorf("%w: sowing fact %s needs a positive revision", ErrInvalid, f.FactID)
	}
	if _, err := parseDate(f.SownOn); err != nil {
		return fmt.Errorf("%w: sowing fact %s sown_on: %v", ErrInvalid, f.FactID, err)
	}
	if f.AreaHa <= 0 {
		return fmt.Errorf("%w: sowing fact %s area must be positive", ErrInvalid, f.FactID)
	}
	if f.EventType != "initial" && f.EventType != "reseed" {
		return fmt.Errorf("%w: sowing fact %s event_type must be initial or reseed", ErrInvalid, f.FactID)
	}
	switch statusOrActive(f.Status) {
	case "active", "cancelled":
	default:
		return fmt.Errorf("%w: sowing fact %s status must be active or cancelled", ErrInvalid, f.FactID)
	}
	return nil
}

func validateTemperatureObservation(o model.TemperatureObservation) error {
	if o.StationID == "" {
		return fmt.Errorf("%w: temperature observation needs station_id", ErrInvalid)
	}
	if _, err := parseDate(o.ObsDate); err != nil {
		return fmt.Errorf("%w: observation %s date: %v", ErrInvalid, o.StationID, err)
	}
	if o.Revision <= 0 {
		return fmt.Errorf("%w: observation %s/%s needs a positive revision", ErrInvalid, o.StationID, o.ObsDate)
	}
	if o.TMinC > o.TMaxC {
		return fmt.Errorf("%w: observation %s/%s tmin > tmax", ErrInvalid, o.StationID, o.ObsDate)
	}
	return nil
}

func statusOrActive(s string) string {
	if strings.TrimSpace(s) == "" {
		return "active"
	}
	return s
}

func nullableJSON(v any) any {
	if v == nil {
		return nil
	}
	return jsonBytes(v)
}

func upsertSeasonRule(ctx context.Context, tx pgx.Tx, r model.SeasonRule) error {
	published := r.PublishedAt
	if published.IsZero() {
		published = time.Now().UTC()
	}
	_, err := tx.Exec(ctx, `
		insert into season_rules (rule_id, season_id, rule_version, crop, hemisphere,
			das_tolerance_days, weather_gap_pending_days, min_weather_coverage,
			station_max_distance_km, station_max_elevation_delta_m, missing_day_upper_fraction,
			published_at, notes)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		on conflict (rule_id) do update
		  set season_id=excluded.season_id, rule_version=excluded.rule_version, crop=excluded.crop,
		      hemisphere=excluded.hemisphere, das_tolerance_days=excluded.das_tolerance_days,
		      weather_gap_pending_days=excluded.weather_gap_pending_days,
		      min_weather_coverage=excluded.min_weather_coverage,
		      station_max_distance_km=excluded.station_max_distance_km,
		      station_max_elevation_delta_m=excluded.station_max_elevation_delta_m,
		      missing_day_upper_fraction=excluded.missing_day_upper_fraction,
		      published_at=excluded.published_at, notes=excluded.notes`,
		r.RuleID, r.SeasonID, r.RuleVersion, r.Crop, r.Hemisphere,
		defaultInt(r.DASToleranceDays, 3), defaultInt(r.WeatherGapPendingDays, 10),
		defaultFloat(r.MinWeatherCoverage, 0.9), defaultFloat(r.StationMaxDistanceKm, 40),
		defaultFloat(r.StationMaxElevationDeltaM, 120), defaultFloat(r.MissingDayUpperFraction, 0.5),
		published.UTC(), jsonBytes(notesOrEmpty(r.Notes)))
	return err
}

func notesOrEmpty(n []string) []string {
	if n == nil {
		return []string{}
	}
	return n
}

func defaultInt(v, def int) int {
	if v <= 0 {
		return def
	}
	return v
}

func defaultFloat(v, def float64) float64 {
	if v <= 0 {
		return def
	}
	return v
}

func parseDate(s string) (time.Time, error) {
	return time.Parse("2006-01-02", s)
}

func dateString(t time.Time) string { return t.UTC().Format("2006-01-02") }
