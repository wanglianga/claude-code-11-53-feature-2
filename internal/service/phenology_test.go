package service

import (
	"strings"
	"testing"
	"time"

	"agrisync/internal/model"
)

func day(y int, m time.Month, d int) time.Time {
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func TestAccumulateGDDNoGap(t *testing.T) {
	// Five observed days: mean 25, base 10 => 15 GDD/day => 75.
	days := []weatherDay{}
	for i := 0; i < 5; i++ {
		days = append(days, weatherDay{Date: day(2026, 3, 1+i), TMin: 20, TMax: 30, Present: true})
	}
	acc := accumulateGDD(days, 10)
	if acc.Lower != 75 || acc.Upper != 75 {
		t.Fatalf("no-gap accumulation: lower=%v upper=%v", acc.Lower, acc.Upper)
	}
	if acc.Coverage != 1 {
		t.Fatalf("coverage: %v", acc.Coverage)
	}
}

func TestAccumulateGDDGapWidensUpper(t *testing.T) {
	days := []weatherDay{
		{Date: day(2026, 3, 1), TMin: 20, TMax: 30, Present: true}, // 15
		{Date: day(2026, 3, 2)}, // missing
		{Date: day(2026, 3, 3)}, // missing
	}
	acc := accumulateGDD(days, 10)
	if acc.Lower != 15 {
		t.Fatalf("lower should be 15, got %v", acc.Lower)
	}
	// Upper adds 2 missing days at the observed max daily rate (15).
	if acc.Upper != 45 {
		t.Fatalf("upper should be 45, got %v", acc.Upper)
	}
	if acc.MissingDays != 2 || acc.ObservedDays != 1 {
		t.Fatalf("counts: %+v", acc)
	}
}

func TestGDDCrossingsPointAndUpperDates(t *testing.T) {
	// Three observed days (15 GDD/day) then one missing day. Lower-bound
	// accumulation never reaches 50, but the gap-widened upper bound does.
	days := []weatherDay{
		{Date: day(2026, 3, 1), TMin: 20, TMax: 30, Present: true},
		{Date: day(2026, 3, 2), TMin: 20, TMax: 30, Present: true},
		{Date: day(2026, 3, 3), TMin: 20, TMax: 30, Present: true},
		{Date: day(2026, 3, 4)},
	}
	thresholds := map[string]float64{model.StageEmergence: 50}
	lower, upper := gddCrossings(days, 10, thresholds, 1.0)
	if _, ok := lower[model.StageEmergence]; ok {
		t.Fatalf("lower bound should not cross 50 (got %v)", lower[model.StageEmergence])
	}
	if dateString(upper[model.StageEmergence]) != "2026-03-04" {
		t.Fatalf("upper crossing: %v", upper[model.StageEmergence])
	}
	// With a threshold the point estimate does cross, both bounds agree.
	lower2, upper2 := gddCrossings(days, 10, map[string]float64{model.StageEmergence: 30}, 1.0)
	if dateString(lower2[model.StageEmergence]) != "2026-03-02" ||
		dateString(upper2[model.StageEmergence]) != "2026-03-02" {
		t.Fatalf("point crossing lower=%v upper=%v", lower2, upper2)
	}
}

func TestFurthestStageOrdering(t *testing.T) {
	th := map[string]float64{
		model.StageEmergence: 100, model.StageVegetative: 300, model.StageHarvestPrep: 800,
	}
	if got := furthestStage(th, 99); got != "" {
		t.Fatalf("99 should reach nothing, got %s", got)
	}
	if got := furthestStage(th, 100); got != model.StageEmergence {
		t.Fatalf("100 should be emergence, got %s", got)
	}
	if got := furthestStage(th, 500); got != model.StageVegetative {
		t.Fatalf("500 should be vegetative, got %s", got)
	}
	if got := furthestStage(th, 1000); got != model.StageHarvestPrep {
		t.Fatalf("1000 should be harvest prep, got %s", got)
	}
}

func TestRunStatusAndSummarise(t *testing.T) {
	if runStatus(50, 100) != "partial" || runStatus(100, 100) != "pending_all" ||
		runStatus(0, 0) != "empty" || runStatus(0, 50) != "complete" {
		t.Fatal("runStatus mapping wrong")
	}
	regions := []model.ComparisonRegion{
		{RegionCode: "DE-BB", Included: true, SownAreaHa: 100, StageAreas: map[string]float64{
			model.StageEmergence: 20, model.StageVegetative: 80}, FurthestStage: model.StageVegetative,
			Pending: map[string]float64{}},
		{RegionCode: "BR-MT", Included: true, SownAreaHa: 200, StageAreas: map[string]float64{
			model.StageEmergence: 200}, FurthestStage: model.StageEmergence,
			Pending: map[string]float64{PendingWeatherMissing: 50}},
	}
	totals, pending, common, comparable := summariseComparison(regions, model.PhenologyStageOrder)
	if totals[model.StageEmergence] != 220 || totals[model.StageVegetative] != 80 {
		t.Fatalf("stage totals: %v", totals)
	}
	if pending[PendingWeatherMissing] != 50 {
		t.Fatalf("pending totals: %v", pending)
	}
	// The common stage is the least advanced furthest-stage across regions.
	if common != model.StageEmergence {
		t.Fatalf("common stage: %s", common)
	}
	if strings.Join(comparable, ",") != "BR-MT,DE-BB" {
		t.Fatalf("comparable regions: %v", comparable)
	}
}

func TestNormalizeStageKeys(t *testing.T) {
	all, err := normalizeStageKeys(nil)
	if err != nil || len(all) != 3 {
		t.Fatalf("default stages: %v %v", all, err)
	}
	got, err := normalizeStageKeys([]string{"harvest_prep", "emergence", "harvest_prep"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "emergence,harvest_prep" {
		t.Fatalf("ordered/deduped: %v", got)
	}
	if _, err := normalizeStageKeys([]string{"flowering"}); err == nil {
		t.Fatal("unknown stage should be rejected")
	}
}

func TestPickCalendarVarietyThenCrop(t *testing.T) {
	cal := map[string]map[string]model.StageCalendar{
		"V1|maize": {model.StageEmergence: {StartDay: 7}},
		"|maize":   {model.StageEmergence: {StartDay: 10}},
	}
	if d := pickCalendar(cal, "V1", "maize"); d[model.StageEmergence] != 7 {
		t.Fatalf("variety calendar not preferred: %v", d)
	}
	if d := pickCalendar(cal, "V2", "maize"); d[model.StageEmergence] != 10 {
		t.Fatalf("crop calendar fallback: %v", d)
	}
	if d := pickCalendar(cal, "V9", "wheat"); d != nil {
		t.Fatalf("no calendar expected: %v", d)
	}
}

func TestEvidenceFingerprintChangesWithRevision(t *testing.T) {
	rule := model.SeasonRule{RuleID: "r1", RuleVersion: "v1"}
	facts := []sowingSegment{{FactID: "f1", Revision: 1}}
	a := buildEvidenceFingerprint(rule, "das", facts, nil)
	facts[0].Revision = 2
	b := buildEvidenceFingerprint(rule, "das", facts, nil)
	if a == b {
		t.Fatal("fingerprint must change when a sowing fact revision changes")
	}
}
