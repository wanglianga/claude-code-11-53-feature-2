package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"agrisync/internal/model"
)

// allowedPhenologyMethods is the comparison method allowlist.
func normalizeMethod(method string) (string, error) {
	m := strings.ToLower(strings.TrimSpace(method))
	if m != "das" && m != "gdd" {
		return "", fmt.Errorf("%w: method must be das or gdd", ErrInvalid)
	}
	return m, nil
}

// normalizeStageKeys validates and orders the stage keys requested for
// comparison. An empty list defaults to all canonical stages.
func normalizeStageKeys(keys []string) ([]string, error) {
	if len(keys) == 0 {
		return append([]string(nil), model.PhenologyStageOrder...), nil
	}
	seen := map[string]bool{}
	out := []string{}
	for _, k := range keys {
		k = strings.TrimSpace(k)
		if model.PhenologyStageRank(k) == 0 {
			return nil, fmt.Errorf("%w: unknown stage %q", ErrInvalid, k)
		}
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return model.PhenologyStageRank(out[i]) < model.PhenologyStageRank(out[j])
	})
	return out, nil
}

// ComparePhenology aligns region-published stage products for one season rule
// version and method at a common as-of date. It reads only confirmed replica
// products: the group plane never sees plot identifiers or trajectories.
//
// The output is a comparison of agronomic progress. It intentionally reports
// no efficiency or yield figure.
func (s *Service) ComparePhenology(ctx context.Context, req model.PhenologyComparisonRequest, actor, role string) (model.PhenologyComparison, error) {
	method, err := normalizeMethod(req.Method)
	if err != nil {
		return model.PhenologyComparison{}, err
	}
	stageKeys, err := normalizeStageKeys(req.StageKeys)
	if err != nil {
		return model.PhenologyComparison{}, err
	}
	asOf, err := parseDate(req.AsOfDate)
	if err != nil {
		return model.PhenologyComparison{}, fmt.Errorf("%w: as_of_date must be YYYY-MM-DD", ErrInvalid)
	}
	if strings.TrimSpace(req.RuleID) == "" {
		return model.PhenologyComparison{}, fmt.Errorf("%w: rule_id is required", ErrInvalid)
	}
	approval, err := s.GetApproval(ctx, req.ApprovalID)
	if err != nil {
		return model.PhenologyComparison{}, err
	}
	if approval.Status != "approved" {
		return model.PhenologyComparison{}, fmt.Errorf("%w: approval %s is %s, not approved", ErrForbidden, approval.ID, approval.Status)
	}
	now := s.now().UTC()
	if approval.ValidFrom != nil && now.Before(*approval.ValidFrom) {
		return model.PhenologyComparison{}, fmt.Errorf("%w: approval not yet valid", ErrForbidden)
	}
	if approval.ValidUntil != nil && now.After(*approval.ValidUntil) {
		return model.PhenologyComparison{}, fmt.Errorf("%w: approval expired", ErrForbidden)
	}
	if len(approval.ScopeRegions) == 0 {
		return model.PhenologyComparison{}, fmt.Errorf("%w: approval has empty region scope", ErrInvalid)
	}
	// The comparison must itself be within the approved agent scope. We reuse
	// the aggregate_metric scope marker so a narrow approval cannot be widened
	// by asking for phenology: the steward must have approved *a* reporting
	// metric for each region. This keeps existing approvals compatible while
	// refusing approvals that authorize no metric at all.
	if !containsString(approval.ScopeMetrics, "area_ha") {
		return model.PhenologyComparison{}, fmt.Errorf("%w: approval scope does not authorize area reporting required for stage comparison", ErrForbidden)
	}

	// The rule must be consistent across every region; we take season/crop
	// from the first confirmed product and reject mixed rules.
	regions := []model.ComparisonRegion{}
	included := []string{}
	excluded := []string{}
	var seasonID, ruleVersion, crop string

	scopeRegions := append([]string(nil), approval.ScopeRegions...)
	sort.Strings(scopeRegions)
	for _, region := range scopeRegions {
		regionAsOf := dateString(asOf)
		if v, ok := req.RegionAsOf[region]; ok && v != "" {
			d, err := parseDate(v)
			if err != nil {
				return model.PhenologyComparison{}, fmt.Errorf("%w: region_as_of for %s must be YYYY-MM-DD", ErrInvalid, region)
			}
			regionAsOf = dateString(d)
		}
		cr := s.loadComparisonRegion(ctx, region, req.RuleID, method, regionAsOf, req.RegionRuns)
		if !cr.Included {
			excluded = append(excluded, region)
			regions = append(regions, cr)
			continue
		}
		prod, _ := s.loadProductForRegion(ctx, region, cr.RunID)
		if seasonID == "" {
			seasonID, ruleVersion, crop = prod.SeasonID, prod.RuleVersion, prod.Crop
		} else if prod.SeasonID != seasonID || prod.RuleVersion != ruleVersion || prod.Crop != crop {
			// Mixed season rules cannot be pooled into one comparison.
			cr.Included = false
			cr.ExclusionReason = "mixed_season_rule"
			excluded = append(excluded, region)
			regions = append(regions, cr)
			continue
		}
		included = append(included, region)
		regions = append(regions, cr)
	}

	stageTotals, pendingTotals, commonStage, comparable := summariseComparison(regions, stageKeys)
	cmp := model.PhenologyComparison{
		ID:                      randID("phcmp"),
		ApprovalID:              approval.ID,
		ApprovalVersion:         approval.Version,
		RuleID:                  req.RuleID,
		SeasonID:                seasonID,
		RuleVersion:             ruleVersion,
		Crop:                    crop,
		Method:                  method,
		AsOfDate:                dateString(asOf),
		StageKeys:               stageKeys,
		Status:                  "computed",
		IncludedRegions:         included,
		ExcludedRegions:         excluded,
		Regions:                 regions,
		CommonStage:             commonStage,
		CommonComparableRegions: comparable,
		StageTotals:             stageTotals,
		PendingTotals:           pendingTotals,
		Disclaimer:              comparisonDisclaimer(),
		RequestedBy:             actor,
		CreatedAt:               now,
	}
	tx, err := s.db.Control().Begin(ctx)
	if err != nil {
		return model.PhenologyComparison{}, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		insert into phenology_comparisons (id, approval_id, approval_version, rule_id, season_id,
			rule_version, crop, method, as_of_date, stage_keys, status, included_regions,
			excluded_regions, regions, common_stage, common_comparable_regions, stage_totals,
			pending_totals, requested_by, created_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)`,
		cmp.ID, cmp.ApprovalID, cmp.ApprovalVersion, cmp.RuleID, cmp.SeasonID, cmp.RuleVersion,
		cmp.Crop, cmp.Method, cmp.AsOfDate, cmp.StageKeys, cmp.Status, cmp.IncludedRegions,
		cmp.ExcludedRegions, jsonBytes(cmp.Regions), cmp.CommonStage, cmp.CommonComparableRegions,
		jsonBytes(cmp.StageTotals), jsonBytes(cmp.PendingTotals), cmp.RequestedBy, cmp.CreatedAt); err != nil {
		return model.PhenologyComparison{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return model.PhenologyComparison{}, err
	}
	if _, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: "phenology.compare", SubjectType: "phenology_comparison",
		SubjectID: cmp.ID, Decision: "allow",
		Detail: map[string]any{
			"approval_id": approval.ID, "approval_version": approval.Version,
			"rule_id": cmp.RuleID, "method": cmp.Method, "as_of_date": cmp.AsOfDate,
			"included_regions": included, "excluded_regions": excluded,
			"common_stage": commonStage, "common_comparable_regions": comparable,
			"stage_totals": stageTotals,
		},
	}); err != nil {
		return model.PhenologyComparison{}, err
	}
	return cmp, nil
}

// loadComparisonRegion resolves the confirmed product for a region and turns
// it into a comparison row. A region with no confirmed run is excluded with a
// machine-readable reason rather than silently omitted.
func (s *Service) loadComparisonRegion(ctx context.Context, region, ruleID, method, asOf string, pinned map[string]string) model.ComparisonRegion {
	cr := model.ComparisonRegion{RegionCode: region, AsOfDate: asOf, StageAreas: map[string]float64{},
		Pending: map[string]float64{}, EvidenceRanges: map[string]model.EvidenceRange{},
		PendingEvidence: map[string]model.EvidenceRange{}}
	q := `select run_id, source_seq, product from replica_phenology_products
	      where source_region=$1 and rule_id=$2 and method=$3 and as_of_date=$4 and status='confirmed'`
	args := []any{region, ruleID, method, asOf}
	if pinnedID, ok := pinned[region]; ok && pinnedID != "" {
		q = `select run_id, source_seq, product from replica_phenology_products
		     where source_region=$1 and run_id=$2 and status='confirmed'`
		args = []any{region, pinnedID}
	}
	q += ` order by source_seq desc limit 1`
	var runID string
	var sourceSeq int64
	var raw []byte
	err := s.db.Control().QueryRow(ctx, q, args...).Scan(&runID, &sourceSeq, &raw)
	if errors.Is(err, pgx.ErrNoRows) {
		cr.Included = false
		cr.ExclusionReason = "no_confirmed_stage_product"
		return cr
	}
	if err != nil {
		cr.Included = false
		cr.ExclusionReason = "replica_read_error"
		return cr
	}
	var prod model.PhenologyProduct
	if err := json.Unmarshal(raw, &prod); err != nil {
		cr.Included = false
		cr.ExclusionReason = "product_decode_error"
		return cr
	}
	cr.Included = true
	cr.RunID = runID
	cr.ReplicationSeq = sourceSeq
	cr.SownAreaHa = prod.SownAreaHa
	cr.PreEmergenceAreaHa = prod.PreEmergenceAreaHa
	cr.StageAreas = prod.StageAreas
	cr.PendingAreaHa = prod.PendingAreaHa
	cr.Pending = prod.Pending
	cr.EvidenceRanges = prod.EvidenceRanges
	cr.PendingEvidence = prod.PendingEvidence
	cr.WeatherCoverage = prod.WeatherCoverage
	cr.FurthestStage = furthestReachedStage(prod.StageAreas)
	return cr
}

func (s *Service) loadProductForRegion(ctx context.Context, region, runID string) (model.PhenologyProduct, error) {
	var prod model.PhenologyProduct
	var raw []byte
	err := s.db.Control().QueryRow(ctx, `
		select product from replica_phenology_products
		where source_region=$1 and run_id=$2 and status='confirmed'`, region, runID).Scan(&raw)
	if err != nil {
		return prod, err
	}
	err = json.Unmarshal(raw, &prod)
	return prod, err
}

// summariseComparison computes stage totals, per-stage pending totals, the
// furthest stage reached by every included region (the common stage), and the
// regions that are actually comparable at that stage.
func summariseComparison(regions []model.ComparisonRegion, stageKeys []string) (map[string]float64, map[string]float64, string, []string) {
	stageTotals := map[string]float64{}
	for _, k := range stageKeys {
		stageTotals[k] = 0
	}
	pendingTotals := map[string]float64{}
	common := ""
	comparable := []string{}
	includedCount := 0
	for _, r := range regions {
		if !r.Included {
			continue
		}
		includedCount++
		for _, k := range stageKeys {
			stageTotals[k] = round2(stageTotals[k] + r.StageAreas[k])
		}
		for reason, area := range r.Pending {
			pendingTotals[reason] = round2(pendingTotals[reason] + area)
		}
		if r.FurthestStage != "" {
			if common == "" || model.PhenologyStageRank(r.FurthestStage) < model.PhenologyStageRank(common) {
				common = r.FurthestStage
			}
		}
	}
	// A region is comparable at the common stage only if it has area in that
	// stage (i.e. it has actually reached it) and is not entirely pending.
	for _, r := range regions {
		if !r.Included {
			continue
		}
		if common == "" || model.PhenologyStageRank(r.FurthestStage) < model.PhenologyStageRank(common) {
			continue
		}
		if r.PendingAreaHa >= r.SownAreaHa && r.SownAreaHa > 0 {
			continue
		}
		comparable = append(comparable, r.RegionCode)
	}
	sort.Strings(comparable)
	return stageTotals, pendingTotals, common, comparable
}

func furthestReachedStage(stageAreas map[string]float64) string {
	stage := ""
	for _, s := range model.PhenologyStageOrder {
		if stageAreas[s] > 0 {
			stage = s
		}
	}
	return stage
}

func comparisonDisclaimer() []string {
	return []string{
		"Stage alignment compares agronomic progress only; it does not rate team performance.",
		"It does not predict yield and does not adjust for weather, soil or management differences.",
		"Common comparable regions are those that have reached the common stage with area not left pending.",
		"Pending area is reported separately; it is never forced into a stage to complete the map.",
	}
}

func (s *Service) GetPhenologyComparison(ctx context.Context, id string) (model.PhenologyComparison, error) {
	var cmp model.PhenologyComparison
	var regionsJSON, stageTotals, pendingTotals []byte
	var asOf time.Time
	err := s.db.Control().QueryRow(ctx, `
		select id, approval_id, approval_version, rule_id, season_id, rule_version, crop, method,
		       as_of_date, stage_keys, status, included_regions, excluded_regions, regions,
		       common_stage, common_comparable_regions, stage_totals, pending_totals,
		       requested_by, created_at
		from phenology_comparisons where id=$1`, id).
		Scan(&cmp.ID, &cmp.ApprovalID, &cmp.ApprovalVersion, &cmp.RuleID, &cmp.SeasonID,
			&cmp.RuleVersion, &cmp.Crop, &cmp.Method, &asOf, &cmp.StageKeys, &cmp.Status,
			&cmp.IncludedRegions, &cmp.ExcludedRegions, &regionsJSON, &cmp.CommonStage,
			&cmp.CommonComparableRegions, &stageTotals, &pendingTotals, &cmp.RequestedBy, &cmp.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return cmp, fmt.Errorf("%w: phenology comparison %s", ErrNotFound, id)
	}
	if err != nil {
		return cmp, err
	}
	cmp.AsOfDate = dateString(asOf)
	_ = json.Unmarshal(regionsJSON, &cmp.Regions)
	_ = json.Unmarshal(stageTotals, &cmp.StageTotals)
	_ = json.Unmarshal(pendingTotals, &cmp.PendingTotals)
	cmp.Disclaimer = comparisonDisclaimer()
	d, err := s.latestPhenologyDelivery(ctx, id)
	if err == nil {
		cmp.Delivery = &d
	}
	return cmp, nil
}

func (s *Service) ListPhenologyComparisons(ctx context.Context, limit int) ([]model.PhenologyComparison, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Control().Query(ctx, `
		select id from phenology_comparisons order by created_at desc limit $1`, limit)
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
	out := []model.PhenologyComparison{}
	for _, id := range ids {
		c, err := s.GetPhenologyComparison(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// DeliverPhenologyComparison re-validates the approval immediately before
// sending, scans the exact bytes for restricted fields, and stores the
// artifact text plus its SHA-256. It mirrors the operational delivery gate.
func (s *Service) DeliverPhenologyComparison(ctx context.Context, id, actor, role string) (model.PhenologyDelivery, error) {
	cmp, err := s.GetPhenologyComparison(ctx, id)
	if err != nil {
		return model.PhenologyDelivery{}, err
	}
	if cmp.Status == "delivered" {
		return model.PhenologyDelivery{}, fmt.Errorf("%w: comparison %s was already delivered; a new delivery needs a new run",
			ErrConflict, cmp.ID)
	}
	approval, err := s.GetApproval(ctx, cmp.ApprovalID)
	if err != nil {
		return model.PhenologyDelivery{}, err
	}
	now := s.now().UTC()
	d := model.PhenologyDelivery{
		ID: randID("phdlv"), ComparisonID: cmp.ID, Status: "blocked", Channel: s.cfg.DeliveryChannel,
		Checks: []model.Check{}, RequestedBy: actor, CreatedAt: now,
	}
	addCheck := func(name string, passed bool, detail string) {
		d.Checks = append(d.Checks, model.Check{Name: name, Passed: passed, Detail: detail})
	}
	addCheck("approval_status", approval.Status == "approved", "approval status is "+approval.Status)
	addCheck("approval_version_current", approval.Version == cmp.ApprovalVersion,
		fmt.Sprintf("comparison computed under version %d, current version %d", cmp.ApprovalVersion, approval.Version))
	inValidity := true
	if approval.ValidFrom != nil && now.Before(*approval.ValidFrom) {
		inValidity = false
	}
	if approval.ValidUntil != nil && now.After(*approval.ValidUntil) {
		inValidity = false
	}
	addCheck("approval_validity_window", inValidity, "checked against current approval validity window")

	regionsOutside := []string{}
	for _, r := range cmp.IncludedRegions {
		if !containsString(approval.ScopeRegions, r) {
			regionsOutside = append(regionsOutside, r)
		}
	}
	addCheck("region_scope_current", len(regionsOutside) == 0,
		"included regions outside current scope: "+strings.Join(regionsOutside, ","))
	addCheck("metric_scope_current", containsString(approval.ScopeMetrics, "area_ha"),
		"current approval authorizes area reporting")

	restricted, err := s.restrictedRegions(ctx)
	if err != nil {
		return d, err
	}
	leaked := []string{}
	for _, r := range restricted {
		var n int
		if err := s.db.Control().QueryRow(ctx,
			`select count(*) from replica_trajectories where source_region=$1`, r).Scan(&n); err != nil {
			return d, err
		}
		if n > 0 {
			leaked = append(leaked, fmt.Sprintf("%s:%d", r, n))
		}
	}
	addCheck("restricted_raw_never_in_group_store", len(leaked) == 0,
		"restricted raw trajectory replicas found: "+strings.Join(leaked, ","))

	artifact := s.buildPhenologyArtifact(cmp, approval, now)
	artifactJSON := jsonBytes(artifact)
	forbidden := scanForRestrictedFields(artifact, "")
	addCheck("artifact_has_no_coordinates", len(forbidden) == 0,
		"restricted field paths found in artifact: "+strings.Join(forbidden, ","))

	allPassed := true
	for _, c := range d.Checks {
		if !c.Passed {
			allPassed = false
			if d.BlockedReason == "" {
				d.BlockedReason = c.Name + ": " + c.Detail
			}
		}
	}

	tx, err := s.db.Control().Begin(ctx)
	if err != nil {
		return d, err
	}
	defer tx.Rollback(ctx)
	var stored any
	if allPassed {
		d.Status = "sent"
		sentAt := now
		d.SentAt = &sentAt
		d.ArtifactSHA = sha256Hex(artifactJSON)
		d.Artifact = artifact
		stored = artifactJSON
		if _, err := tx.Exec(ctx, `update phenology_comparisons set status='delivered' where id=$1`, cmp.ID); err != nil {
			return d, err
		}
	} else {
		d.Artifact = nil
		d.ArtifactSHA = ""
		stored = nil
		if _, err := tx.Exec(ctx, `update phenology_comparisons set status='blocked' where id=$1 and status<>'delivered'`, cmp.ID); err != nil {
			return d, err
		}
	}
	if _, err := tx.Exec(ctx, `
		insert into phenology_deliveries (id, comparison_id, approval_id, status, channel, checks,
			blocked_reason, artifact, artifact_sha256, requested_by, created_at, sent_at)
		values ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		d.ID, d.ComparisonID, cmp.ApprovalID, d.Status, d.Channel, jsonBytes(d.Checks),
		nullStr(d.BlockedReason), stored, nullStr(d.ArtifactSHA), d.RequestedBy, d.CreatedAt, d.SentAt); err != nil {
		return d, err
	}
	if err := tx.Commit(ctx); err != nil {
		return d, err
	}
	action := "phenology.delivery.sent"
	decision := "allow"
	if !allPassed {
		action = "phenology.delivery.blocked"
		decision = "deny"
	}
	auditSeq, err := s.AppendAudit(ctx, model.AuditEvent{
		Actor: actor, Role: role, Action: action, SubjectType: "phenology_delivery", SubjectID: d.ID,
		Decision: decision,
		Detail: map[string]any{
			"comparison_id": cmp.ID, "approval_id": approval.ID, "approval_version": approval.Version,
			"approval_status": approval.Status, "channel": d.Channel,
			"included_regions": cmp.IncludedRegions, "excluded_regions": cmp.ExcludedRegions,
			"artifact_sha256": d.ArtifactSHA, "blocked_reason": d.BlockedReason, "checks": d.Checks,
		},
	})
	if err != nil {
		return d, err
	}
	d.AuditSeq = auditSeq
	_, _ = s.db.Control().Exec(ctx, `update phenology_deliveries set audit_seq=$2 where id=$1`, d.ID, auditSeq)
	return d, nil
}

type phenologyArtifact struct {
	Schema            string                    `json:"schema"`
	ComparisonID      string                    `json:"comparison_id"`
	ApprovalID        string                    `json:"approval_id"`
	ApprovalVersion   int                       `json:"approval_version"`
	Purpose           string                    `json:"purpose"`
	Recipient         string                    `json:"recipient"`
	RuleID            string                    `json:"rule_id"`
	SeasonID          string                    `json:"season_id"`
	RuleVersion       string                    `json:"rule_version"`
	Crop              string                    `json:"crop"`
	Method            string                    `json:"method"`
	StageKeys         []string                  `json:"stage_keys"`
	AsOfDate          string                    `json:"as_of_date"`
	IncludedRegions   []string                  `json:"included_regions"`
	ExcludedRegions   []string                  `json:"excluded_regions"`
	CommonStage       string                    `json:"common_stage"`
	ComparableRegions []string                  `json:"common_comparable_regions"`
	StageTotals       map[string]float64        `json:"stage_totals"`
	PendingTotals     map[string]float64        `json:"pending_totals"`
	Regions           []artifactPhenologyRegion `json:"regions"`
	ContainsRawPoints bool                      `json:"contains_raw_trajectories"`
	Unit              string                    `json:"area_unit"`
	Disclaimer        []string                  `json:"disclaimer"`
	GeneratedAt       time.Time                 `json:"generated_at"`
}

type artifactPhenologyRegion struct {
	RegionCode         string                         `json:"region_code"`
	AsOfDate           string                         `json:"as_of_date"`
	Included           bool                           `json:"included"`
	ExclusionReason    string                         `json:"exclusion_reason,omitempty"`
	RunID              string                         `json:"run_id,omitempty"`
	ReplicationSeq     int64                          `json:"replication_seq,omitempty"`
	SownAreaHa         float64                        `json:"sown_area_ha"`
	StageAreas         map[string]float64             `json:"stage_areas"`
	PreEmergenceAreaHa float64                        `json:"pre_emergence_area_ha"`
	PendingAreaHa      float64                        `json:"pending_area_ha"`
	Pending            map[string]float64             `json:"pending"`
	EvidenceRanges     map[string]model.EvidenceRange `json:"evidence_ranges"`
	WeatherCoverage    float64                        `json:"weather_coverage"`
	FurthestStage      string                         `json:"furthest_stage"`
}

func (s *Service) buildPhenologyArtifact(cmp model.PhenologyComparison, approval model.OutboundRequest, now time.Time) phenologyArtifact {
	doc := phenologyArtifact{
		Schema: "agrisync.phenology_comparison/v1", ComparisonID: cmp.ID, ApprovalID: approval.ID,
		ApprovalVersion: approval.Version, Purpose: approval.Purpose, Recipient: approval.Recipient,
		RuleID: cmp.RuleID, SeasonID: cmp.SeasonID, RuleVersion: cmp.RuleVersion, Crop: cmp.Crop,
		Method: cmp.Method, StageKeys: cmp.StageKeys, AsOfDate: cmp.AsOfDate,
		IncludedRegions: []string{}, ExcludedRegions: []string{}, CommonStage: cmp.CommonStage,
		ComparableRegions: cmp.CommonComparableRegions, StageTotals: cmp.StageTotals,
		PendingTotals: cmp.PendingTotals, Regions: []artifactPhenologyRegion{},
		ContainsRawPoints: false, Unit: "hectare", Disclaimer: comparisonDisclaimer(), GeneratedAt: now,
	}
	for _, r := range cmp.Regions {
		// Only regions the current approval still covers are exported.
		if !containsString(approval.ScopeRegions, r.RegionCode) {
			continue
		}
		ar := artifactPhenologyRegion{
			RegionCode: r.RegionCode, AsOfDate: r.AsOfDate, Included: r.Included, ExclusionReason: r.ExclusionReason,
			RunID: r.RunID, ReplicationSeq: r.ReplicationSeq, SownAreaHa: r.SownAreaHa,
			StageAreas: r.StageAreas, PreEmergenceAreaHa: r.PreEmergenceAreaHa,
			PendingAreaHa: r.PendingAreaHa, Pending: r.Pending, EvidenceRanges: r.EvidenceRanges,
			WeatherCoverage: r.WeatherCoverage, FurthestStage: r.FurthestStage,
		}
		doc.Regions = append(doc.Regions, ar)
		if r.Included {
			doc.IncludedRegions = append(doc.IncludedRegions, r.RegionCode)
		} else {
			doc.ExcludedRegions = append(doc.ExcludedRegions, r.RegionCode)
		}
	}
	return doc
}

func (s *Service) latestPhenologyDelivery(ctx context.Context, comparisonID string) (model.PhenologyDelivery, error) {
	var d model.PhenologyDelivery
	var checks, artifact []byte
	err := s.db.Control().QueryRow(ctx, `
		select id, comparison_id, status, channel, checks, coalesce(blocked_reason,''), artifact,
		       coalesce(artifact_sha256,''), coalesce(audit_seq,0), requested_by, created_at, sent_at
		from phenology_deliveries where comparison_id=$1 order by created_at desc limit 1`, comparisonID).
		Scan(&d.ID, &d.ComparisonID, &d.Status, &d.Channel, &checks, &d.BlockedReason, &artifact,
			&d.ArtifactSHA, &d.AuditSeq, &d.RequestedBy, &d.CreatedAt, &d.SentAt)
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal(checks, &d.Checks)
	_ = json.Unmarshal(artifact, &d.Artifact)
	return d, nil
}

func (s *Service) GetPhenologyDelivery(ctx context.Context, id string) (model.PhenologyDelivery, error) {
	var d model.PhenologyDelivery
	var checks, artifact []byte
	err := s.db.Control().QueryRow(ctx, `
		select id, comparison_id, status, channel, checks, coalesce(blocked_reason,''), artifact,
		       coalesce(artifact_sha256,''), coalesce(audit_seq,0), requested_by, created_at, sent_at
		from phenology_deliveries where id=$1`, id).
		Scan(&d.ID, &d.ComparisonID, &d.Status, &d.Channel, &checks, &d.BlockedReason, &artifact,
			&d.ArtifactSHA, &d.AuditSeq, &d.RequestedBy, &d.CreatedAt, &d.SentAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return d, fmt.Errorf("%w: phenology delivery %s", ErrNotFound, id)
	}
	if err != nil {
		return d, err
	}
	_ = json.Unmarshal(checks, &d.Checks)
	_ = json.Unmarshal(artifact, &d.Artifact)
	return d, nil
}

// VerifyPhenologyDelivery re-checks a stored delivery: byte-for-byte hash,
// restricted-field scan, and the audit row binding the digest.
func (s *Service) VerifyPhenologyDelivery(ctx context.Context, id string) (model.PhenologyDeliveryVerification, error) {
	d, err := s.GetPhenologyDelivery(ctx, id)
	if err != nil {
		return model.PhenologyDeliveryVerification{}, err
	}
	out := model.PhenologyDeliveryVerification{
		DeliveryID: d.ID, Status: d.Status, RecordedSHA: d.ArtifactSHA,
		VerifiedAt: s.now().UTC(),
	}
	var raw []byte
	if err := s.db.Control().QueryRow(ctx, `select artifact from phenology_deliveries where id=$1`, id).Scan(&raw); err != nil {
		return out, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		out.SHA256Matches = d.Status != "sent"
	} else {
		out.ArtifactSize = len(raw)
		out.RecomputedSHA = sha256Hex(raw)
		out.SHA256Matches = out.RecomputedSHA == d.ArtifactSHA
		var generic any
		if err := json.Unmarshal(raw, &generic); err != nil {
			return out, err
		}
		out.RestrictedHits = scanForRestrictedFields(generic, "")
		if len(out.RestrictedHits) > 0 {
			out.SHA256Matches = false
		}
	}
	events, err := s.AuditFor(ctx, d.ID)
	if err != nil {
		return out, err
	}
	for _, e := range events {
		if e.Action != "phenology.delivery.sent" && e.Action != "phenology.delivery.blocked" {
			continue
		}
		if v, ok := e.Detail["artifact_sha256"].(string); ok && v == d.ArtifactSHA {
			out.AuditValid = true
		}
	}
	chain, err := s.VerifyAudit(ctx)
	if err != nil {
		return out, err
	}
	out.AuditVerified = chain.Valid
	return out, nil
}
