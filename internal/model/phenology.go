package model

import "time"

// Phenological stage keys. The stage order is meaningful: a plot is in the
// highest stage whose threshold has been reached.
const (
	StageEmergence   = "emergence"
	StageVegetative  = "vegetative"
	StageHarvestPrep = "harvest_prep"
)

// PhenologyStageOrder is the canonical progressive order of the stages this
// data product aligns. Anything not in this list is not a stage.
var PhenologyStageOrder = []string{StageEmergence, StageVegetative, StageHarvestPrep}

// PhenologyStageRank returns the 1-based rank of a stage, or 0 if unknown.
func PhenologyStageRank(stage string) int {
	for i, s := range PhenologyStageOrder {
		if s == stage {
			return i + 1
		}
	}
	return 0
}

// SeasonRule binds every part of the product to one season/rule version:
// sowing facts, variety catalog, weather/calendar evidence, derivation,
// region aggregation and the group comparison all carry the same rule_id.
type SeasonRule struct {
	RuleID      string `json:"rule_id"`
	SeasonID    string `json:"season_id"`
	RuleVersion string `json:"rule_version"`
	Crop        string `json:"crop"`
	Hemisphere  string `json:"hemisphere"`
	// DASToleranceDays widens the day-threshold onset evidence window.
	DASToleranceDays int `json:"das_tolerance_days"`
	// WeatherGapPendingDays is how wide an uncertain (gap-driven) stage
	// crossing may become before the segment is listed as pending.
	WeatherGapPendingDays int `json:"weather_gap_pending_days"`
	// MinWeatherCoverage is the minimum fraction of days between sowing and
	// as_of that must have a temperature observation for the GDD route.
	MinWeatherCoverage float64 `json:"min_weather_coverage"`
	// StationMaxDistanceKm / StationMaxElevationDeltaM define when a weather
	// station is applicable to a plot. Out-of-range links make the plot
	// pending instead of silently borrowing a distant station.
	StationMaxDistanceKm      float64 `json:"station_max_distance_km"`
	StationMaxElevationDeltaM float64 `json:"station_max_elevation_delta_m"`
	// MissingDayUpperFraction bounds how much growing-degree-day accumulation
	// a missing observation day may contribute to the upper estimate. It is
	// only used to widen the evidence range, never to fill in data.
	MissingDayUpperFraction float64   `json:"missing_day_upper_fraction"`
	PublishedAt             time.Time `json:"published_at"`
	Notes                   []string  `json:"notes,omitempty"`
}

// GDDParams are variety-declared growing-degree-day parameters.
type GDDParams struct {
	BaseTempC  float64            `json:"base_temp_c"`
	Thresholds map[string]float64 `json:"thresholds"`
}

// Variety is one season-effective entry of the variety catalog. Day
// thresholds are cumulative days after sowing; GDD thresholds are cumulative
// growing-degree-days above BaseTempC.
type Variety struct {
	VarietyCode   string         `json:"variety_code"`
	Crop          string         `json:"crop"`
	MaturityGroup string         `json:"maturity_group"`
	DASThresholds map[string]int `json:"das_thresholds"`
	GDD           *GDDParams     `json:"gdd,omitempty"`
}

// Plot is the declared, area-bearing unit a sowing fact belongs to. It carries
// no coordinates: the group product never sees field geometry.
type Plot struct {
	PlotID         string  `json:"plot_id"`
	Crop           string  `json:"crop"`
	DeclaredAreaHa float64 `json:"declared_area_ha"`
	Note           string  `json:"note,omitempty"`
}

// SowingFact is an evidence-backed sowing record. A correction is the same
// fact_id at a higher revision; a reseed is a new fact_id (and segment). Only
// the highest revision with status "active" is used by a run.
type SowingFact struct {
	FactID      string    `json:"fact_id"`
	PlotID      string    `json:"plot_id"`
	SegmentNo   int       `json:"segment_no"`
	Crop        string    `json:"crop"`
	VarietyCode string    `json:"variety_code"`
	SownOn      string    `json:"sown_on"` // YYYY-MM-DD
	AreaHa      float64   `json:"area_ha"`
	EventType   string    `json:"event_type"` // initial | reseed
	EvidenceRef string    `json:"evidence_ref"`
	RecordedAt  time.Time `json:"recorded_at"`
	Revision    int       `json:"revision"`
	Status      string    `json:"status"` // active | superseded | cancelled
}

// StageCalendar is the agronomy department's published stage calendar for a
// crop/variety, expressed in days after sowing. It is used as a transparent
// threshold fallback when a variety does not declare day thresholds.
type StageCalendar struct {
	CalendarID  string    `json:"calendar_id"`
	Crop        string    `json:"crop"`
	VarietyCode string    `json:"variety_code,omitempty"`
	Stage       string    `json:"stage"`
	StartDay    int       `json:"start_day"`
	EndDay      int       `json:"end_day"`
	Source      string    `json:"source"`
	PublishedAt time.Time `json:"published_at"`
}

type WeatherStation struct {
	StationID  string  `json:"station_id"`
	Label      string  `json:"label,omitempty"`
	ElevationM float64 `json:"elevation_m"`
}

// StationLink records how far a plot's agronomic reference point sits from a
// station, and the elevation difference. Distance/height applicability is
// judged against the season rule.
type StationLink struct {
	PlotID          string  `json:"plot_id"`
	StationID       string  `json:"station_id"`
	DistanceKm      float64 `json:"distance_km"`
	ElevationDeltaM float64 `json:"elevation_delta_m"`
}

// TemperatureObservation is one station-day. A revision supersedes lower
// revisions for the same station-day; historical runs keep the revision they
// read.
type TemperatureObservation struct {
	StationID  string    `json:"station_id"`
	ObsDate    string    `json:"obs_date"` // YYYY-MM-DD
	TMinC      float64   `json:"tmin_c"`
	TMaxC      float64   `json:"tmax_c"`
	Source     string    `json:"source"`
	Revision   int       `json:"revision"`
	RecordedAt time.Time `json:"recorded_at"`
}

// StageObservation is field-scouted stage evidence. It is used only for
// contradiction detection, never to overwrite the derivation.
type StageObservation struct {
	ObservationID string    `json:"observation_id"`
	PlotID        string    `json:"plot_id"`
	SegmentNo     int       `json:"segment_no"`
	Stage         string    `json:"stage"`
	ObservedOn    string    `json:"observed_on"`
	Source        string    `json:"source"`
	ReceivedAt    time.Time `json:"received_at"`
}

// PhenologyEvidence is the full local evidence upload bundle.
type PhenologyEvidence struct {
	BatchID                 string                   `json:"batch_id"`
	Rule                    SeasonRule               `json:"rule"`
	Varieties               []Variety                `json:"varieties"`
	Plots                   []Plot                   `json:"plots"`
	SowingFacts             []SowingFact             `json:"sowing_facts"`
	StageCalendars          []StageCalendar          `json:"stage_calendars"`
	Stations                []WeatherStation         `json:"stations"`
	StationLinks            []StationLink            `json:"station_links"`
	TemperatureObservations []TemperatureObservation `json:"temperature_observations"`
	StageObservations       []StageObservation       `json:"stage_observations"`
}

type PhenologyIngestResult struct {
	Region   string         `json:"region"`
	BatchID  string         `json:"batch_id"`
	RuleID   string         `json:"rule_id"`
	Replayed bool           `json:"replayed_batch"`
	Counts   map[string]int `json:"counts"`
}

// PhenologyRunRequest asks the region to derive a new immutable stage run.
type PhenologyRunRequest struct {
	RuleID   string `json:"rule_id"`
	Method   string `json:"method"` // das | gdd
	AsOfDate string `json:"as_of_date"`
	Note     string `json:"note,omitempty"`
}

// PendingArea explains which area could not be assigned to a stage.
type PendingArea struct {
	Reason   string  `json:"reason"`
	AreaHa   float64 `json:"area_ha"`
	Segments int     `json:"segments"`
}

// EvidenceRange is the onset evidence window of one stage: the earliest and
// latest date on which a classified segment reached that stage. Weather gaps
// widen this window; a window wider than the rule allows becomes pending.
type EvidenceRange struct {
	Stage string `json:"stage"`
	From  string `json:"from"`
	To    string `json:"to"`
}

// PlotSegment is region-local detail. It is never included in the group
// product: the product is aggregated at region granularity.
type PlotSegment struct {
	PlotID          string  `json:"plot_id"`
	SegmentNo       int     `json:"segment_no"`
	FactID          string  `json:"fact_id"`
	FactRevision    int     `json:"fact_revision"`
	Crop            string  `json:"crop"`
	VarietyCode     string  `json:"variety_code"`
	AreaHa          float64 `json:"area_ha"`
	EventType       string  `json:"event_type"`
	SownOn          string  `json:"sown_on"`
	Status          string  `json:"status"` // classified | pending
	Stage           string  `json:"stage,omitempty"`
	PendingReason   string  `json:"pending_reason,omitempty"`
	DASDays         int     `json:"das_days"`
	GDDLower        float64 `json:"gdd_lower"`
	GDDUpper        float64 `json:"gdd_upper"`
	WeatherCoverage float64 `json:"weather_coverage"`
	StationID       string  `json:"station_id,omitempty"`
	OnsetFrom       string  `json:"onset_from,omitempty"`
	OnsetTo         string  `json:"onset_to,omitempty"`
}

// PhenologyRunSummary is the frozen aggregate stored with the run.
type PhenologyRunSummary struct {
	SownAreaHa         float64                  `json:"sown_area_ha"`
	StageAreas         map[string]float64       `json:"stage_areas"`
	PreEmergenceAreaHa float64                  `json:"pre_emergence_area_ha"`
	PendingAreaHa      float64                  `json:"pending_area_ha"`
	Pending            []PendingArea            `json:"pending"`
	EvidenceRanges     map[string]EvidenceRange `json:"evidence_ranges"`
	PendingEvidence    map[string]EvidenceRange `json:"pending_evidence"`
	WeatherCoverage    float64                  `json:"weather_coverage"`
	SegmentedPlots     int                      `json:"segmented_plots"`
	ReseededAreaHa     float64                  `json:"reseeded_area_ha"`
	CalendarUsed       bool                     `json:"calendar_used"`
	Notes              []string                 `json:"notes"`
}

// PhenologyRun is the region-local, immutable result of one derivation.
type PhenologyRun struct {
	RunID               string              `json:"run_id"`
	Region              string              `json:"region_code"`
	RuleID              string              `json:"rule_id"`
	SeasonID            string              `json:"season_id"`
	RuleVersion         string              `json:"rule_version"`
	Crop                string              `json:"crop"`
	Method              string              `json:"method"`
	AsOfDate            string              `json:"as_of_date"`
	Status              string              `json:"status"`
	EvidenceFingerprint string              `json:"evidence_fingerprint"`
	Summary             PhenologyRunSummary `json:"summary"`
	Segments            []PlotSegment       `json:"segments"`
	CreatedAt           time.Time           `json:"created_at"`
}

// PhenologyProduct is what the region publishes to the group: approved
// geographic granularity (region), stage areas, pending areas and evidence
// windows only. No plot identifiers and no coordinates.
type PhenologyProduct struct {
	Schema             string                   `json:"schema"`
	Region             string                   `json:"region_code"`
	RunID              string                   `json:"run_id"`
	RuleID             string                   `json:"rule_id"`
	SeasonID           string                   `json:"season_id"`
	RuleVersion        string                   `json:"rule_version"`
	Crop               string                   `json:"crop"`
	Method             string                   `json:"method"`
	AsOfDate           string                   `json:"as_of_date"`
	SownAreaHa         float64                  `json:"sown_area_ha"`
	StageAreas         map[string]float64       `json:"stage_areas"`
	PreEmergenceAreaHa float64                  `json:"pre_emergence_area_ha"`
	PendingAreaHa      float64                  `json:"pending_area_ha"`
	Pending            map[string]float64       `json:"pending"`
	EvidenceRanges     map[string]EvidenceRange `json:"evidence_ranges"`
	PendingEvidence    map[string]EvidenceRange `json:"pending_evidence"`
	WeatherCoverage    float64                  `json:"weather_coverage"`
	SegmentedPlots     int                      `json:"segmented_plots"`
	ReseededAreaHa     float64                  `json:"reseeded_area_ha"`
	GeneratedAt        time.Time                `json:"generated_at"`
	DataCutoff         time.Time                `json:"data_cutoff"`
	Disclaimer         []string                 `json:"disclaimer"`
}

// PhenologyComparisonRequest asks the group plane to align one season under
// one rule version and one method.
type PhenologyComparisonRequest struct {
	ApprovalID string   `json:"approval_id"`
	RuleID     string   `json:"rule_id"`
	Method     string   `json:"method"`
	AsOfDate   string   `json:"as_of_date"`
	StageKeys  []string `json:"stage_keys"`
	// RegionAsOf lets each region be compared at its own local equivalent date
	// (different hemispheres sow in different calendar months). Without it the
	// single as_of_date applies to every region.
	RegionAsOf map[string]string `json:"region_as_of,omitempty"`
	// RegionRuns optionally pins a specific region run per region. Without a
	// pin the comparison uses the latest confirmed run for the rule, method
	// and resolved as_of date; pinning is how a historical report keeps its
	// evidence.
	RegionRuns map[string]string `json:"region_runs,omitempty"`
}

type ComparisonRegion struct {
	RegionCode         string                   `json:"region_code"`
	AsOfDate           string                   `json:"as_of_date"`
	Included           bool                     `json:"included"`
	ExclusionReason    string                   `json:"exclusion_reason,omitempty"`
	RunID              string                   `json:"run_id,omitempty"`
	ReplicationSeq     int64                    `json:"replication_seq,omitempty"`
	SownAreaHa         float64                  `json:"sown_area_ha"`
	StageAreas         map[string]float64       `json:"stage_areas"`
	PreEmergenceAreaHa float64                  `json:"pre_emergence_area_ha"`
	PendingAreaHa      float64                  `json:"pending_area_ha"`
	Pending            map[string]float64       `json:"pending"`
	EvidenceRanges     map[string]EvidenceRange `json:"evidence_ranges"`
	PendingEvidence    map[string]EvidenceRange `json:"pending_evidence"`
	WeatherCoverage    float64                  `json:"weather_coverage"`
	FurthestStage      string                   `json:"furthest_stage"`
}

// PhenologyComparison is the group's stage-aligned view. It deliberately
// reports agronomic progress only.
type PhenologyComparison struct {
	ID                      string             `json:"id"`
	ApprovalID              string             `json:"approval_id"`
	ApprovalVersion         int                `json:"approval_version"`
	RuleID                  string             `json:"rule_id"`
	SeasonID                string             `json:"season_id"`
	RuleVersion             string             `json:"rule_version"`
	Crop                    string             `json:"crop"`
	Method                  string             `json:"method"`
	AsOfDate                string             `json:"as_of_date"`
	StageKeys               []string           `json:"stage_keys"`
	Status                  string             `json:"status"`
	IncludedRegions         []string           `json:"included_regions"`
	ExcludedRegions         []string           `json:"excluded_regions"`
	Regions                 []ComparisonRegion `json:"regions"`
	CommonStage             string             `json:"common_stage,omitempty"`
	CommonComparableRegions []string           `json:"common_comparable_regions"`
	StageTotals             map[string]float64 `json:"stage_totals"`
	PendingTotals           map[string]float64 `json:"pending_totals"`
	Disclaimer              []string           `json:"disclaimer"`
	RequestedBy             string             `json:"requested_by"`
	CreatedAt               time.Time          `json:"created_at"`
	Delivery                *PhenologyDelivery `json:"delivery,omitempty"`
}

type PhenologyDelivery struct {
	ID            string     `json:"id"`
	ComparisonID  string     `json:"comparison_id"`
	Status        string     `json:"status"`
	Channel       string     `json:"channel"`
	Checks        []Check    `json:"checks"`
	BlockedReason string     `json:"blocked_reason,omitempty"`
	Artifact      any        `json:"artifact,omitempty"`
	ArtifactSHA   string     `json:"artifact_sha256,omitempty"`
	AuditSeq      int64      `json:"audit_seq,omitempty"`
	RequestedBy   string     `json:"requested_by"`
	CreatedAt     time.Time  `json:"created_at"`
	SentAt        *time.Time `json:"sent_at,omitempty"`
}

type PhenologyDeliveryVerification struct {
	DeliveryID     string    `json:"delivery_id"`
	Status         string    `json:"status"`
	RecomputedSHA  string    `json:"recomputed_sha256,omitempty"`
	RecordedSHA    string    `json:"recorded_sha256,omitempty"`
	SHA256Matches  bool      `json:"sha256_matches"`
	RestrictedHits []string  `json:"restricted_fields,omitempty"`
	AuditValid     bool      `json:"audit_entry_valid"`
	AuditVerified  bool      `json:"audit_chain_valid"`
	ArtifactSize   int       `json:"artifact_bytes"`
	VerifiedAt     time.Time `json:"verified_at"`
}
