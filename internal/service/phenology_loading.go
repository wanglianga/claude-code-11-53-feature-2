package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agrisync/internal/model"
)

// seasonRule loads the season rule (and its parameters) for a rule id.
func loadSeasonRule(ctx context.Context, pool *pgxpool.Pool, ruleID string) (model.SeasonRule, error) {
	var r model.SeasonRule
	var notesJSON []byte
	err := pool.QueryRow(ctx, `
		select rule_id, season_id, rule_version, crop, hemisphere, das_tolerance_days,
		       weather_gap_pending_days, min_weather_coverage, station_max_distance_km,
		       station_max_elevation_delta_m, missing_day_upper_fraction, published_at, notes
		from season_rules where rule_id=$1`, ruleID).
		Scan(&r.RuleID, &r.SeasonID, &r.RuleVersion, &r.Crop, &r.Hemisphere, &r.DASToleranceDays,
			&r.WeatherGapPendingDays, &r.MinWeatherCoverage, &r.StationMaxDistanceKm,
			&r.StationMaxElevationDeltaM, &r.MissingDayUpperFraction, &r.PublishedAt, &notesJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return r, fmt.Errorf("%w: season rule %s has no evidence in this region", ErrNotFound, ruleID)
	}
	if err != nil {
		return r, err
	}
	_ = json.Unmarshal(notesJSON, &r.Notes)
	return r, nil
}

// loadActiveSowingFacts returns the highest active revision of each sowing
// fact for the rule. Superseded and cancelled revisions are ignored, so a
// later correction never silently rewrites a run that already used the old
// revision (the old run is immutable; this only affects new runs).
func loadActiveSowingFacts(ctx context.Context, pool *pgxpool.Pool, ruleID string) ([]sowingSegment, error) {
	rows, err := pool.Query(ctx, `
		select distinct on (fact_id)
		       fact_id, revision, plot_id, segment_no, crop, variety_code, sown_on, area_ha,
		       event_type, recorded_at
		from sowing_facts
		where rule_id=$1 and status='active'
		order by fact_id, revision desc`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []sowingSegment{}
	for rows.Next() {
		var f sowingSegment
		if err := rows.Scan(&f.FactID, &f.Revision, &f.PlotID, &f.SegmentNo, &f.Crop, &f.VarietyCode,
			&f.SownOn, &f.AreaHa, &f.EventType, &f.RecordedAt); err != nil {
			return nil, err
		}
		f.SownOn = time.Date(f.SownOn.Year(), f.SownOn.Month(), f.SownOn.Day(), 0, 0, 0, 0, time.UTC)
		out = append(out, f)
	}
	return out, rows.Err()
}

func loadVarieties(ctx context.Context, pool *pgxpool.Pool, ruleID string) (map[string]model.Variety, error) {
	rows, err := pool.Query(ctx, `
		select variety_code, crop, maturity_group, das_thresholds, gdd
		from phenology_varieties where rule_id=$1`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.Variety{}
	for rows.Next() {
		var v model.Variety
		var dasJSON, gddJSON []byte
		if err := rows.Scan(&v.VarietyCode, &v.Crop, &v.MaturityGroup, &dasJSON, &gddJSON); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(dasJSON, &v.DASThresholds)
		if len(gddJSON) > 0 && string(gddJSON) != "null" {
			var p model.GDDParams
			if err := json.Unmarshal(gddJSON, &p); err == nil {
				v.GDD = &p
			}
		}
		out[v.VarietyCode] = v
	}
	return out, rows.Err()
}

// loadStageCalendars returns two indexes: variety-specific and crop-wide.
func loadStageCalendars(ctx context.Context, pool *pgxpool.Pool, ruleID string) (map[string]map[string]model.StageCalendar, error) {
	rows, err := pool.Query(ctx, `
		select calendar_id, crop, variety_code, stage, start_day, end_day, source, published_at
		from stage_calendars where rule_id=$1`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]map[string]model.StageCalendar{}
	for rows.Next() {
		var c model.StageCalendar
		if err := rows.Scan(&c.CalendarID, &c.Crop, &c.VarietyCode, &c.Stage, &c.StartDay, &c.EndDay,
			&c.Source, &c.PublishedAt); err != nil {
			return nil, err
		}
		if model.PhenologyStageRank(c.Stage) == 0 {
			continue
		}
		key := c.VarietyCode + "|" + c.Crop
		if out[key] == nil {
			out[key] = map[string]model.StageCalendar{}
		}
		out[key][c.Stage] = c
	}
	return out, rows.Err()
}

type plotInfo struct {
	PlotID string
	Crop   string
	AreaHa float64
}

func loadPlots(ctx context.Context, pool *pgxpool.Pool) (map[string]plotInfo, error) {
	rows, err := pool.Query(ctx, `select plot_id, crop, declared_area_ha from phenology_plots`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]plotInfo{}
	for rows.Next() {
		var p plotInfo
		if err := rows.Scan(&p.PlotID, &p.Crop, &p.AreaHa); err != nil {
			return nil, err
		}
		out[p.PlotID] = p
	}
	return out, rows.Err()
}

func loadStationLinks(ctx context.Context, pool *pgxpool.Pool, ruleID string) (map[string]model.StationLink, error) {
	rows, err := pool.Query(ctx, `
		select plot_id, station_id, distance_km, elevation_delta_m
		from station_links where rule_id=$1`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]model.StationLink{}
	for rows.Next() {
		var l model.StationLink
		if err := rows.Scan(&l.PlotID, &l.StationID, &l.DistanceKm, &l.ElevationDeltaM); err != nil {
			return nil, err
		}
		out[l.PlotID] = l
	}
	return out, rows.Err()
}

type stageObservationKey struct {
	PlotID    string
	SegmentNo int
}

func loadStageObservations(ctx context.Context, pool *pgxpool.Pool, ruleID string) (map[stageObservationKey]model.StageObservation, error) {
	rows, err := pool.Query(ctx, `
		select observation_id, plot_id, segment_no, stage, observed_on, source, received_at
		from stage_observations where rule_id=$1`, ruleID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[stageObservationKey]model.StageObservation{}
	for rows.Next() {
		var o model.StageObservation
		var observedOn time.Time
		if err := rows.Scan(&o.ObservationID, &o.PlotID, &o.SegmentNo, &o.Stage, &observedOn,
			&o.Source, &o.ReceivedAt); err != nil {
			return nil, err
		}
		o.ObservedOn = dateString(observedOn)
		key := stageObservationKey{o.PlotID, o.SegmentNo}
		// Keep the most recently received observation for the segment.
		if prev, ok := out[key]; !ok || o.ReceivedAt.After(prev.ReceivedAt) {
			out[key] = o
		}
	}
	return out, rows.Err()
}

// observationFor returns the scouted observation applicable to a segment:
// an exact segment match wins over a plot-wide (segment_no=0) observation.
func observationFor(obs map[stageObservationKey]model.StageObservation, plotID string, segmentNo int) (model.StageObservation, bool) {
	if o, ok := obs[stageObservationKey{plotID, segmentNo}]; ok {
		return o, true
	}
	if o, ok := obs[stageObservationKey{plotID, 0}]; ok {
		return o, true
	}
	return model.StageObservation{}, false
}

// PhenologyEvidenceSummary is a region-local accounting of what evidence the
// region holds for a rule. It is useful for an operator to see whether a
// weather gap or missing variety is why an area is pending.
type PhenologyEvidenceSummary struct {
	Region            string   `json:"region_code"`
	RuleID            string   `json:"rule_id"`
	Rules             []string `json:"rule_ids"`
	Plots             int      `json:"plots"`
	SowingFacts       int      `json:"active_sowing_facts"`
	Varieties         int      `json:"varieties"`
	StageCalendars    int      `json:"stage_calendars"`
	WeatherStations   int      `json:"weather_stations"`
	StationLinks      int      `json:"station_links"`
	Observations      int      `json:"temperature_observations"`
	StageObservations int      `json:"stage_observations"`
	RunCount          int      `json:"phenology_runs"`
}

func (s *Service) PhenologyEvidenceSummary(ctx context.Context, region, ruleID string) (PhenologyEvidenceSummary, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return PhenologyEvidenceSummary{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	out := PhenologyEvidenceSummary{Region: region, RuleID: ruleID, Rules: []string{}}
	rows, err := pool.Query(ctx, `select rule_id from season_rules order by rule_id`)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var r string
		if err := rows.Scan(&r); err != nil {
			rows.Close()
			return out, err
		}
		out.Rules = append(out.Rules, r)
	}
	rows.Close()
	if out.RuleID == "" && len(out.Rules) == 1 {
		out.RuleID = out.Rules[0]
	}
	if out.RuleID == "" {
		return out, nil
	}
	queries := []struct {
		q    string
		dest *int
	}{
		{`select count(*) from phenology_plots`, &out.Plots},
		{`select count(*) from sowing_facts where rule_id=$1 and status='active'`, &out.SowingFacts},
		{`select count(*) from phenology_varieties where rule_id=$1`, &out.Varieties},
		{`select count(*) from stage_calendars where rule_id=$1`, &out.StageCalendars},
		{`select count(*) from weather_stations where rule_id=$1`, &out.WeatherStations},
		{`select count(*) from station_links where rule_id=$1`, &out.StationLinks},
		{`select count(*) from temperature_observations where rule_id=$1 and status='active'`, &out.Observations},
		{`select count(*) from stage_observations where rule_id=$1`, &out.StageObservations},
		{`select count(*) from phenology_runs where rule_id=$1`, &out.RunCount},
	}
	for _, q := range queries {
		if err := pool.QueryRow(ctx, q.q, out.RuleID).Scan(q.dest); err != nil {
			return out, err
		}
	}
	return out, nil
}
