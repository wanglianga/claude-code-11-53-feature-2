package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"agrisync/internal/model"
)

// ---------------------------------------------------------------------------
// Stage derivation (day-thresholds and growing-degree-days)
// ---------------------------------------------------------------------------

// Pending reasons. Each one is a reason a segment's area is NOT attributed to
// a stage; that area is reported as pending instead of being forced into the
// stage map to "complete" a global picture.
const (
	PendingVarietyUnknown        = "variety_unknown"
	PendingNoThresholds          = "no_variety_or_calendar_thresholds"
	PendingMultiCropPlot         = "multi_crop_plot"
	PendingNoStationLink         = "no_station_link"
	PendingStationNotApplicable  = "station_distance_or_height_not_applicable"
	PendingWeatherMissing        = "weather_observations_missing"
	PendingWeatherGapWidens      = "weather_gap_widens_stage"
	PendingStageEvidenceConflict = "stage_evidence_conflict"
	PendingGDDParamsMissing      = "gdd_params_not_declared"
)

type sowingSegment struct {
	FactID      string
	Revision    int
	PlotID      string
	SegmentNo   int
	Crop        string
	VarietyCode string
	SownOn      time.Time
	AreaHa      float64
	EventType   string
	RecordedAt  time.Time
}

type weatherDay struct {
	Date     time.Time
	TMin     float64
	TMax     float64
	Revision int
	Present  bool
}

// DerivePhenologyRun computes one immutable stage run for a region. It reads
// only the region's own evidence tables, classifies every active sowing
// segment under the requested method, and records the exact evidence
// revisions it used. It never updates a prior run: a late sowing fact or a
// weather revision produces a NEW run and leaves old progress reports intact.
func (s *Service) DerivePhenologyRun(ctx context.Context, region string, req model.PhenologyRunRequest, actor string) (model.PhenologyRun, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.PhenologyRun{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if req.Method != "das" && req.Method != "gdd" {
		return model.PhenologyRun{}, fmt.Errorf("%w: method must be das or gdd", ErrInvalid)
	}
	asOf, err := parseDate(req.AsOfDate)
	if err != nil {
		return model.PhenologyRun{}, fmt.Errorf("%w: as_of_date must be YYYY-MM-DD", ErrInvalid)
	}
	rule, err := loadSeasonRule(ctx, pool, req.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	facts, err := loadActiveSowingFacts(ctx, pool, rule.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	varieties, err := loadVarieties(ctx, pool, rule.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	calendars, err := loadStageCalendars(ctx, pool, rule.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	plots, err := loadPlots(ctx, pool)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	links, err := loadStationLinks(ctx, pool, rule.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	stageObservations, err := loadStageObservations(ctx, pool, rule.RuleID)
	if err != nil {
		return model.PhenologyRun{}, err
	}

	factsByPlot := map[string][]sowingSegment{}
	for _, f := range facts {
		factsByPlot[f.PlotID] = append(factsByPlot[f.PlotID], f)
	}

	d := &derivation{
		ctx: ctx, pool: pool, rule: rule, method: req.Method, asOf: asOf,
		varieties: varieties, calendars: calendars, plots: plots,
		links: links, stageObservations: stageObservations,
		stageAreas:      map[string]float64{},
		pendingByReason: map[string]*model.PendingArea{},
		evidenceFrom:    map[string]time.Time{},
		evidenceTo:      map[string]time.Time{},
		pendingFrom:     map[string]time.Time{},
		pendingTo:       map[string]time.Time{},
		weatherUsed:     map[string]bool{},
		weatherDays:     0,
		weatherPresent:  0,
	}

	for _, plotID := range sortedKeys(factsByPlot) {
		d.classifyPlot(plotID, factsByPlot[plotID])
	}

	pendingList := make([]model.PendingArea, 0, len(d.pendingByReason))
	var pendingTotal float64
	for _, p := range d.pendingByReason {
		pendingList = append(pendingList, *p)
		pendingTotal += p.AreaHa
	}
	sort.Slice(pendingList, func(i, j int) bool { return pendingList[i].Reason < pendingList[j].Reason })
	pendingTotal = round2(pendingTotal)

	coverage := 1.0
	if d.method == "gdd" {
		if d.weatherDays > 0 {
			coverage = float64(d.weatherPresent) / float64(d.weatherDays)
		} else {
			coverage = 0
		}
	}

	summary := model.PhenologyRunSummary{
		SownAreaHa:         round2(d.sownArea),
		StageAreas:         roundMap(d.stageAreas),
		PreEmergenceAreaHa: round2(d.preEmergenceArea),
		PendingAreaHa:      pendingTotal,
		Pending:            pendingList,
		EvidenceRanges:     buildRanges(d.evidenceFrom, d.evidenceTo),
		PendingEvidence:    buildRanges(d.pendingFrom, d.pendingTo),
		WeatherCoverage:    round4(coverage),
		SegmentedPlots:     d.segmentedPlots,
		ReseededAreaHa:     round2(d.reseededArea),
		CalendarUsed:       d.calendarUsed,
		Notes:              summaryNotes(req.Method, d.calendarUsed, pendingList),
	}
	run := model.PhenologyRun{
		RunID:               randID("phen"),
		Region:              region,
		RuleID:              rule.RuleID,
		SeasonID:            rule.SeasonID,
		RuleVersion:         rule.RuleVersion,
		Crop:                rule.Crop,
		Method:              req.Method,
		AsOfDate:            req.AsOfDate,
		Status:              runStatus(pendingTotal, summary.SownAreaHa),
		EvidenceFingerprint: buildEvidenceFingerprint(rule, req.Method, facts, d.weatherUsed),
		Summary:             summary,
		Segments:            d.segments,
		CreatedAt:           s.now().UTC(),
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return model.PhenologyRun{}, err
	}
	defer tx.Rollback(ctx)
	var node string
	var epoch, nextOutbox int64
	if err := tx.QueryRow(ctx, `
		select active_node, local_epoch, next_outbox_seq from region_state where id for update`).
		Scan(&node, &epoch, &nextOutbox); err != nil {
		return model.PhenologyRun{}, err
	}
	if _, err := tx.Exec(ctx, `
		insert into phenology_runs (run_id, rule_id, season_id, rule_version, crop, method, as_of_date,
			status, evidence_fingerprint, summary, segments, outbox_seq, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)`,
		run.RunID, run.RuleID, run.SeasonID, run.RuleVersion, run.Crop, run.Method, run.AsOfDate,
		run.Status, run.EvidenceFingerprint, jsonBytes(summary), jsonBytes(d.segments), nextOutbox,
		run.CreatedAt); err != nil {
		return model.PhenologyRun{}, err
	}
	product := s.buildPhenologyProduct(run)
	if _, err := tx.Exec(ctx, `
		insert into outbox_events (seq, entity_type, entity_id, category, payload, source_node, source_epoch)
		values ($1,'phenology_run',$2,'phenology_stage',$3,$4,$5)`,
		nextOutbox, run.RunID, jsonBytes(product), node, epoch); err != nil {
		return model.PhenologyRun{}, err
	}
	if _, err := tx.Exec(ctx, `update region_state set next_outbox_seq=$1, updated_at=now() where id`,
		nextOutbox+1); err != nil {
		return model.PhenologyRun{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.PhenologyRun{}, err
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: "region_operator", Action: "phenology.run", SubjectType: "phenology_run",
		SubjectID: run.RunID, RegionCode: region, Decision: "allow",
		Detail: map[string]any{
			"rule_id": run.RuleID, "method": run.Method, "as_of_date": run.AsOfDate,
			"stage_areas": summary.StageAreas, "pending_area_ha": summary.PendingAreaHa,
			"pre_emergence_area_ha": summary.PreEmergenceAreaHa,
			"evidence_fingerprint":  run.EvidenceFingerprint,
		},
	}); err != nil {
		return model.PhenologyRun{}, err
	}
	return run, nil
}

type derivation struct {
	ctx    context.Context
	pool   *pgxpool.Pool
	rule   model.SeasonRule
	method string
	asOf   time.Time

	varieties         map[string]model.Variety
	calendars         map[string]map[string]model.StageCalendar
	plots             map[string]plotInfo
	links             map[string]model.StationLink
	stageObservations map[stageObservationKey]model.StageObservation

	segments        []model.PlotSegment
	stageAreas      map[string]float64
	pendingByReason map[string]*model.PendingArea
	evidenceFrom    map[string]time.Time
	evidenceTo      map[string]time.Time
	pendingFrom     map[string]time.Time
	pendingTo       map[string]time.Time
	weatherUsed     map[string]bool
	weatherDays     int
	weatherPresent  int

	sownArea         float64
	preEmergenceArea float64
	reseededArea     float64
	segmentedPlots   int
	calendarUsed     bool
}

func (d *derivation) classifyPlot(plotID string, plotFacts []sowingSegment) {
	sort.Slice(plotFacts, func(i, j int) bool {
		if plotFacts[i].SownOn.Equal(plotFacts[j].SownOn) {
			return plotFacts[i].SegmentNo < plotFacts[j].SegmentNo
		}
		return plotFacts[i].SownOn.Before(plotFacts[j].SownOn)
	})
	if len(plotFacts) > 1 {
		d.segmentedPlots++
	}
	multiCrop := distinctCrops(plotFacts) > 1
	declaredCrop := ""
	if p, ok := d.plots[plotID]; ok {
		declaredCrop = p.Crop
	}

	for _, f := range plotFacts {
		d.sownArea += f.AreaHa
		if f.EventType == "reseed" {
			for _, other := range plotFacts {
				if other.FactID != f.FactID && other.SownOn.Before(f.SownOn) {
					d.reseededArea += f.AreaHa
					break
				}
			}
		}
		seg := model.PlotSegment{
			PlotID: plotID, SegmentNo: f.SegmentNo, FactID: f.FactID, FactRevision: f.Revision,
			Crop: f.Crop, VarietyCode: f.VarietyCode, AreaHa: f.AreaHa, EventType: f.EventType,
			SownOn: dateString(f.SownOn), Status: "classified",
			DASDays: int(d.asOf.Sub(f.SownOn).Hours() / 24),
		}
		d.classifySegment(f, seg, multiCrop, declaredCrop)
	}
}

func (d *derivation) classifySegment(f sowingSegment, seg model.PlotSegment, multiCrop bool, declaredCrop string) {
	if multiCrop || (declaredCrop != "" && declaredCrop != f.Crop) {
		// A plot carrying more than one crop cannot be attributed to a single
		// stage track; keep its segments identifiable but do not pool them.
		d.markPending(&seg, PendingMultiCropPlot, f.AreaHa)
		return
	}
	variety, ok := d.varieties[f.VarietyCode]
	if !ok || strings.TrimSpace(f.VarietyCode) == "" {
		d.markPending(&seg, PendingVarietyUnknown, f.AreaHa)
		return
	}

	if d.method == "das" {
		d.classifyDAS(f, seg, variety)
		return
	}
	d.classifyGDD(f, seg, variety)
}

// classifyDAS derives the stage from confirmed sowing date plus variety day
// thresholds (falling back to the published stage calendar). This route needs
// no continuous weather: transparent and usable anywhere.
func (d *derivation) classifyDAS(f sowingSegment, seg model.PlotSegment, variety model.Variety) {
	thresholds := map[string]time.Time{}
	if len(variety.DASThresholds) > 0 {
		for _, stage := range model.PhenologyStageOrder {
			if day, ok := variety.DASThresholds[stage]; ok {
				thresholds[stage] = f.SownOn.AddDate(0, 0, day)
			}
		}
	} else if cal := pickCalendar(d.calendars, variety.VarietyCode, f.Crop); cal != nil {
		d.calendarUsed = true
		for _, stage := range model.PhenologyStageOrder {
			if day, ok := cal[stage]; ok {
				thresholds[stage] = f.SownOn.AddDate(0, 0, day)
			}
		}
	}
	if len(thresholds) == 0 {
		d.markPending(&seg, PendingNoThresholds, f.AreaHa)
		return
	}

	conflict, hasObs := observationFor(d.stageObservations, f.PlotID, f.SegmentNo)

	stage := ""
	for _, s := range model.PhenologyStageOrder {
		if onset, ok := thresholds[s]; ok && !onset.After(d.asOf) {
			stage = s
		}
	}
	if stage == "" {
		// Sown but not yet at the first threshold: not emerged yet.
		seg.Stage = ""
		seg.Status = "classified"
		d.preEmergenceArea += f.AreaHa
		d.segments = append(d.segments, seg)
		return
	}
	if hasObs && model.PhenologyStageRank(conflict.Stage) != model.PhenologyStageRank(stage) {
		d.markPending(&seg, PendingStageEvidenceConflict, f.AreaHa)
		return
	}
	onset := thresholds[stage]
	seg.Stage = stage
	seg.OnsetFrom = dateString(onset)
	seg.OnsetTo = dateString(onset)
	d.stageAreas[stage] += f.AreaHa
	for _, s := range model.PhenologyStageOrder {
		if o, ok := thresholds[s]; ok && !o.After(d.asOf) && model.PhenologyStageRank(s) <= model.PhenologyStageRank(stage) {
			addRange(d.evidenceFrom, d.evidenceTo, s, o, o)
		}
	}
	d.segments = append(d.segments, seg)
}

// classifyGDD derives the stage from cumulative growing degree days at the
// plot's applicable weather station. Missing observation days widen the
// evidence range; a widening past the rule's tolerance becomes pending.
func (d *derivation) classifyGDD(f sowingSegment, seg model.PlotSegment, variety model.Variety) {
	link, ok := d.links[f.PlotID]
	if !ok {
		d.markPending(&seg, PendingNoStationLink, f.AreaHa)
		return
	}
	if link.DistanceKm > d.rule.StationMaxDistanceKm || link.ElevationDeltaM > d.rule.StationMaxElevationDeltaM {
		d.markPending(&seg, PendingStationNotApplicable, f.AreaHa)
		return
	}
	if variety.GDD == nil || len(variety.GDD.Thresholds) == 0 {
		d.markPending(&seg, PendingGDDParamsMissing, f.AreaHa)
		return
	}
	days, err := loadWeatherWindow(d.ctx, d.pool, d.rule.RuleID, link.StationID, f.SownOn, d.asOf)
	if err != nil {
		d.markPending(&seg, PendingWeatherMissing, f.AreaHa)
		return
	}
	acc := accumulateGDD(days, variety.GDD.BaseTempC)
	d.weatherDays += acc.TotalDays
	d.weatherPresent += acc.ObservedDays
	if acc.ObservedDays == 0 {
		d.markPending(&seg, PendingWeatherMissing, f.AreaHa)
		return
	}
	seg.StationID = link.StationID
	seg.WeatherCoverage = round4(acc.Coverage)
	seg.GDDLower = round1(acc.Lower)
	seg.GDDUpper = round1(acc.Upper)
	for _, wd := range days {
		if wd.Present {
			d.weatherUsed[fmt.Sprintf("%s/%s@%d", link.StationID, dateString(wd.Date), wd.Revision)] = true
		}
	}

	lowerOnset, upperOnset := gddCrossings(days, variety.GDD.BaseTempC, variety.GDD.Thresholds, d.rule.MissingDayUpperFraction)
	lowerStage := furthestStage(variety.GDD.Thresholds, acc.Lower)
	upperStage := furthestStage(variety.GDD.Thresholds, acc.Upper)

	// Conflicting scouted observation: trust it over the derivation.
	if ob, hasObs := observationFor(d.stageObservations, f.PlotID, f.SegmentNo); hasObs &&
		model.PhenologyStageRank(ob.Stage) != model.PhenologyStageRank(lowerStage) {
		d.markPending(&seg, PendingStageEvidenceConflict, f.AreaHa)
		return
	}

	// Missing observation days make the stage genuinely uncertain when the
	// lower and upper accumulation bounds disagree about which stage was
	// reached. That segment is pending, with the wide range kept as evidence.
	if lowerStage != upperStage {
		d.markPending(&seg, PendingWeatherGapWidens, f.AreaHa)
		if lowerStage != "" {
			start, end := ordered(lowerOnset[lowerStage], lowerOnset[lowerStage], upperOnset[upperStage], upperOnset[upperStage])
			if start.IsZero() {
				start, end = lowerOnset[lowerStage], lowerOnset[lowerStage]
			}
			addRange(d.pendingFrom, d.pendingTo, lowerStage, start, end)
		}
		return
	}

	if lowerStage == "" {
		// Sown but not emerged under either bound.
		seg.Stage = ""
		seg.Status = "classified"
		d.preEmergenceArea += f.AreaHa
		d.segments = append(d.segments, seg)
		return
	}

	// Same stage on both bounds; the onset date may still shift because missing
	// days add plausible accumulation and therefore move the crossing *earlier*
	// in date. The onset window is the ordered [earliest, latest] pair.
	lo := lowerOnset[lowerStage]
	up := upperOnset[lowerStage]
	if lo.IsZero() {
		d.markPending(&seg, PendingWeatherMissing, f.AreaHa)
		return
	}
	if up.IsZero() || up.After(lo) {
		up = lo
	}
	earliest, latest := up, lo // up is the gap-widened (earlier) crossing
	if int(latest.Sub(earliest).Hours()/24) > d.rule.WeatherGapPendingDays {
		d.markPending(&seg, PendingWeatherGapWidens, f.AreaHa)
		addRange(d.pendingFrom, d.pendingTo, lowerStage, earliest, latest)
		return
	}
	seg.Stage = lowerStage
	seg.OnsetFrom = dateString(earliest)
	seg.OnsetTo = dateString(latest)
	d.stageAreas[lowerStage] += f.AreaHa
	addRange(d.evidenceFrom, d.evidenceTo, lowerStage, earliest, latest)
	for stage, o := range lowerOnset {
		if !o.IsZero() && model.PhenologyStageRank(stage) <= model.PhenologyStageRank(lowerStage) {
			astart, aend := ordered(earliest, latest, o, upperOnset[stage])
			addRange(d.evidenceFrom, d.evidenceTo, stage, astart, aend)
		}
	}
	d.segments = append(d.segments, seg)
}

// ordered returns the min and max of up to two onset pairs, ignoring zero
// values, so an evidence window is always a well-formed [from, to] interval.
func ordered(pairs ...time.Time) (time.Time, time.Time) {
	var start, end time.Time
	for i := 0; i+1 < len(pairs); i += 2 {
		lo, hi := pairs[i], pairs[i+1]
		if lo.IsZero() {
			lo = hi
		}
		if hi.IsZero() {
			hi = lo
		}
		if lo.IsZero() && hi.IsZero() {
			continue
		}
		if lo.After(hi) {
			lo, hi = hi, lo
		}
		if start.IsZero() || lo.Before(start) {
			start = lo
		}
		if end.IsZero() || hi.After(end) {
			end = hi
		}
	}
	return start, end
}

func (d *derivation) markPending(seg *model.PlotSegment, reason string, area float64) {
	seg.Status = "pending"
	seg.PendingReason = reason
	p, ok := d.pendingByReason[reason]
	if !ok {
		p = &model.PendingArea{Reason: reason}
		d.pendingByReason[reason] = p
	}
	p.AreaHa = round2(p.AreaHa + area)
	p.Segments++
	d.segments = append(d.segments, *seg)
}

// furthestStage reports the highest declared stage whose threshold the
// cumulative GDD has reached.
func furthestStage(thresholds map[string]float64, acc float64) string {
	stage := ""
	for _, s := range model.PhenologyStageOrder {
		if th, ok := thresholds[s]; ok && acc >= th {
			stage = s
		}
	}
	return stage
}

type gddAccumulation struct {
	Lower, Upper float64
	ObservedDays int
	MissingDays  int
	Coverage     float64
	TotalDays    int
}

func accumulateGDD(days []weatherDay, base float64) gddAccumulation {
	var acc gddAccumulation
	maxDaily := 0.0
	for _, wd := range days {
		acc.TotalDays++
		if !wd.Present {
			acc.MissingDays++
			continue
		}
		acc.ObservedDays++
		g := (wd.TMin+wd.TMax)/2 - base
		if g < 0 {
			g = 0
		}
		acc.Lower += g
		if g > maxDaily {
			maxDaily = g
		}
	}
	if acc.TotalDays > 0 {
		acc.Coverage = float64(acc.ObservedDays) / float64(acc.TotalDays)
	}
	acc.Upper = acc.Lower + float64(acc.MissingDays)*maxDaily
	return acc
}

// gddCrossings returns, per stage, the lower-series (point estimate) onset
// date and the upper-series (gap-widened) onset date.
func gddCrossings(days []weatherDay, base float64, thresholds map[string]float64, upperFraction float64) (map[string]time.Time, map[string]time.Time) {
	lower := map[string]time.Time{}
	upper := map[string]time.Time{}
	maxDaily := 0.0
	for _, wd := range days {
		if wd.Present {
			if g := (wd.TMin+wd.TMax)/2 - base; g > maxDaily {
				maxDaily = g
			}
		}
	}
	var lowerAcc, upperAcc float64
	for _, wd := range days {
		if wd.Present {
			g := (wd.TMin+wd.TMax)/2 - base
			if g < 0 {
				g = 0
			}
			lowerAcc += g
			upperAcc += g
		} else {
			upperAcc += maxDaily * upperFraction
		}
		for _, s := range model.PhenologyStageOrder {
			th, ok := thresholds[s]
			if !ok {
				continue
			}
			if _, hit := lower[s]; !hit && lowerAcc >= th {
				lower[s] = wd.Date
			}
			if _, hit := upper[s]; !hit && upperAcc >= th {
				upper[s] = wd.Date
			}
		}
	}
	return lower, upper
}

func loadWeatherWindow(ctx context.Context, pool *pgxpool.Pool, ruleID, stationID string, from, to time.Time) ([]weatherDay, error) {
	rows, err := pool.Query(ctx, `
		select obs_date, tmin_c, tmax_c, revision from (
		  select distinct on (obs_date) obs_date, tmin_c, tmax_c, revision, status
		  from temperature_observations
		  where rule_id=$1 and station_id=$2
		  order by obs_date, revision desc
		) x where status='active'`, ruleID, stationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDate := map[string]weatherDay{}
	for rows.Next() {
		var wd weatherDay
		if err := rows.Scan(&wd.Date, &wd.TMin, &wd.TMax, &wd.Revision); err != nil {
			return nil, err
		}
		wd.Date = time.Date(wd.Date.Year(), wd.Date.Month(), wd.Date.Day(), 0, 0, 0, 0, time.UTC)
		wd.Present = true
		byDate[dateString(wd.Date)] = wd
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := []weatherDay{}
	for day := from; !day.After(to); day = day.AddDate(0, 0, 1) {
		if wd, ok := byDate[dateString(day)]; ok {
			out = append(out, wd)
		} else {
			out = append(out, weatherDay{Date: day})
		}
	}
	return out, nil
}

func pickCalendar(calendars map[string]map[string]model.StageCalendar, varietyCode, crop string) map[string]int {
	if m, ok := calendars[varietyCode+"|"+crop]; ok && len(m) > 0 {
		out := map[string]int{}
		for stage, cal := range m {
			out[stage] = cal.StartDay
		}
		return out
	}
	if m, ok := calendars["|"+crop]; ok && len(m) > 0 {
		out := map[string]int{}
		for stage, cal := range m {
			out[stage] = cal.StartDay
		}
		return out
	}
	return nil
}

func addRange(from, to map[string]time.Time, stage string, start, end time.Time) {
	if start.IsZero() {
		return
	}
	if end.IsZero() {
		end = start
	}
	if cur, ok := from[stage]; !ok || start.Before(cur) {
		from[stage] = start
	}
	if cur, ok := to[stage]; !ok || end.After(cur) {
		to[stage] = end
	}
}

func buildRanges(from, to map[string]time.Time) map[string]model.EvidenceRange {
	out := map[string]model.EvidenceRange{}
	for stage, start := range from {
		out[stage] = model.EvidenceRange{Stage: stage, From: dateString(start), To: dateString(to[stage])}
	}
	return out
}

func buildEvidenceFingerprint(rule model.SeasonRule, method string, facts []sowingSegment, weather map[string]bool) string {
	refs := make([]string, 0, len(facts))
	for _, f := range facts {
		refs = append(refs, fmt.Sprintf("%s@%d", f.FactID, f.Revision))
	}
	sort.Strings(refs)
	wkeys := make([]string, 0, len(weather))
	for k := range weather {
		wkeys = append(wkeys, k)
	}
	sort.Strings(wkeys)
	return sha256Hex([]byte(strings.Join([]string{
		"rule=" + rule.RuleID + "|version=" + rule.RuleVersion + "|method=" + method,
		"facts=" + strings.Join(refs, ","),
		"weather=" + strings.Join(wkeys, ","),
	}, "\n")))
}

func distinctCrops(facts []sowingSegment) int {
	set := map[string]bool{}
	for _, f := range facts {
		set[f.Crop] = true
	}
	return len(set)
}

func runStatus(pendingArea, sownArea float64) string {
	switch {
	case sownArea == 0:
		return "empty"
	case pendingArea <= 0:
		return "complete"
	case pendingArea >= sownArea:
		return "pending_all"
	default:
		return "partial"
	}
}

func summaryNotes(method string, calendarUsed bool, pending []model.PendingArea) []string {
	notes := []string{
		"Stage alignment is an agronomic progress measure; it does not rate team performance or predict yield.",
	}
	if method == "das" {
		notes = append(notes, "Derived from confirmed sowing dates plus variety-declared day thresholds (days after sowing).")
	} else {
		notes = append(notes, "Derived from in-region temperature observations accumulated as growing degree days.")
	}
	if calendarUsed {
		notes = append(notes, "Some segments used the agronomy department's published stage calendar because the variety declared no day thresholds.")
	}
	if len(pending) > 0 {
		notes = append(notes, "Pending area is reported separately and never forced into a stage.")
	}
	return notes
}

func (s *Service) buildPhenologyProduct(run model.PhenologyRun) model.PhenologyProduct {
	pending := map[string]float64{}
	for _, p := range run.Summary.Pending {
		pending[p.Reason] = p.AreaHa
	}
	return model.PhenologyProduct{
		Schema:             "agrisync.phenology_stage/v1",
		Region:             run.Region,
		RunID:              run.RunID,
		RuleID:             run.RuleID,
		SeasonID:           run.SeasonID,
		RuleVersion:        run.RuleVersion,
		Crop:               run.Crop,
		Method:             run.Method,
		AsOfDate:           run.AsOfDate,
		SownAreaHa:         run.Summary.SownAreaHa,
		StageAreas:         run.Summary.StageAreas,
		PreEmergenceAreaHa: run.Summary.PreEmergenceAreaHa,
		PendingAreaHa:      run.Summary.PendingAreaHa,
		Pending:            pending,
		EvidenceRanges:     run.Summary.EvidenceRanges,
		PendingEvidence:    run.Summary.PendingEvidence,
		WeatherCoverage:    run.Summary.WeatherCoverage,
		SegmentedPlots:     run.Summary.SegmentedPlots,
		ReseededAreaHa:     run.Summary.ReseededAreaHa,
		GeneratedAt:        run.CreatedAt,
		DataCutoff:         run.CreatedAt,
		Disclaimer:         disclaimer(),
	}
}

func disclaimer() []string {
	return []string{
		"Stage alignment is an agronomic progress measure only.",
		"It does not evaluate team performance and does not forecast yield.",
		"Region-granularity area only: no field identifiers or trajectories are included.",
	}
}

func roundMap(in map[string]float64) map[string]float64 {
	out := map[string]float64{}
	for k, v := range in {
		out[k] = round2(v)
	}
	return out
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round4(v float64) float64 { return math.Round(v*10000) / 10000 }

// GetPhenologyRun returns one region-local run (with plot segments).
func (s *Service) GetPhenologyRun(ctx context.Context, region, runID string) (model.PhenologyRun, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return model.PhenologyRun{}, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	var run model.PhenologyRun
	var summary, segments []byte
	var asOf time.Time
	err := pool.QueryRow(ctx, `
		select run_id, rule_id, season_id, rule_version, crop, method, as_of_date, status,
		       evidence_fingerprint, summary, segments, created_at
		from phenology_runs where run_id=$1`, runID).
		Scan(&run.RunID, &run.RuleID, &run.SeasonID, &run.RuleVersion, &run.Crop, &run.Method,
			&asOf, &run.Status, &run.EvidenceFingerprint, &summary, &segments, &run.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return run, fmt.Errorf("%w: phenology run %s", ErrNotFound, runID)
	}
	if err != nil {
		return run, err
	}
	run.AsOfDate = dateString(asOf)
	run.Region = region
	_ = json.Unmarshal(summary, &run.Summary)
	_ = json.Unmarshal(segments, &run.Segments)
	return run, nil
}

// ListPhenologyRuns lists the most recent runs for a region.
func (s *Service) ListPhenologyRuns(ctx context.Context, region, ruleID, method string, limit int) ([]model.PhenologyRun, error) {
	pool := s.db.Region(region)
	if pool == nil {
		return nil, fmt.Errorf("%w: unknown region %s", ErrNotFound, region)
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := pool.Query(ctx, `
		select run_id from phenology_runs
		where ($1 = '' or rule_id=$1) and ($2 = '' or method=$2)
		order by created_at desc limit $3`, ruleID, method, limit)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	out := []model.PhenologyRun{}
	for _, id := range ids {
		run, err := s.GetPhenologyRun(ctx, region, id)
		if err != nil {
			return nil, err
		}
		out = append(out, run)
	}
	return out, nil
}

func sortedKeys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
