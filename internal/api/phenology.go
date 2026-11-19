package api

import (
	"fmt"
	"net/http"

	"agrisync/internal/model"
	"agrisync/internal/service"
)

// --- region-side phenology evidence and runs ----------------------------

func (s *Server) handlePhenologyEvidence(w http.ResponseWriter, r *http.Request) {
	region := pathValue(r, "region")
	p := principalFrom(r.Context())
	if err := s.requireRegion(p, region); err != nil {
		s.fail(w, err)
		return
	}
	if !s.regionKnown(region) {
		s.fail(w, fmt.Errorf("%w: unknown region %s", service.ErrNotFound, region))
		return
	}
	switch r.Method {
	case http.MethodPost:
		if !p.inAny("region_operator") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var ev model.PhenologyEvidence
		if err := decodeJSON(r, &ev); err != nil {
			s.fail(w, err)
			return
		}
		res, err := s.svc.IngestPhenologyEvidence(r.Context(), region, ev, p.Actor)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, res)
	case http.MethodGet:
		summary, err := s.svc.PhenologyEvidenceSummary(r.Context(), region, r.URL.Query().Get("rule_id"))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, summary)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

type phenologyRunBody struct {
	RuleID   string `json:"rule_id"`
	Method   string `json:"method"`
	AsOfDate string `json:"as_of_date"`
	Note     string `json:"note"`
}

func (s *Server) handlePhenologyRuns(w http.ResponseWriter, r *http.Request) {
	region := pathValue(r, "region")
	p := principalFrom(r.Context())
	if err := s.requireRegion(p, region); err != nil {
		s.fail(w, err)
		return
	}
	if !s.regionKnown(region) {
		s.fail(w, fmt.Errorf("%w: unknown region %s", service.ErrNotFound, region))
		return
	}
	switch r.Method {
	case http.MethodGet:
		runs, err := s.svc.ListPhenologyRuns(r.Context(), region,
			r.URL.Query().Get("rule_id"), r.URL.Query().Get("method"), queryInt(r, "limit", 50))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"region": region, "runs": runs})
	case http.MethodPost:
		if !p.inAny("region_operator") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var body phenologyRunBody
		if err := decodeJSON(r, &body); err != nil {
			s.fail(w, err)
			return
		}
		run, err := s.svc.DerivePhenologyRun(r.Context(), region, model.PhenologyRunRequest{
			RuleID: body.RuleID, Method: body.Method, AsOfDate: body.AsOfDate, Note: body.Note,
		}, p.Actor)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 201, run)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

func (s *Server) handlePhenologyRun(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	region := pathValue(r, "region")
	if err := s.requireRegion(principalFrom(r.Context()), region); err != nil {
		s.fail(w, err)
		return
	}
	run, err := s.svc.GetPhenologyRun(r.Context(), region, pathValue(r, "run"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, run)
}

// --- group-side stage comparison ----------------------------------------

func (s *Server) handlePhenologyComparisons(w http.ResponseWriter, r *http.Request) {
	p := principalFrom(r.Context())
	switch r.Method {
	case http.MethodGet:
		list, err := s.svc.ListPhenologyComparisons(r.Context(), queryInt(r, "limit", 50))
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"comparisons": list})
	case http.MethodPost:
		if !p.inAny("group_analyst") {
			s.fail(w, service.ErrForbidden)
			return
		}
		var req model.PhenologyComparisonRequest
		if err := decodeJSON(r, &req); err != nil {
			s.fail(w, err)
			return
		}
		cmp, err := s.svc.ComparePhenology(r.Context(), req, p.Actor, p.Role)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 201, cmp)
	default:
		writeError(w, 405, "method_not_allowed", "GET or POST only", nil)
	}
}

func (s *Server) handlePhenologyComparison(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	cmp, err := s.svc.GetPhenologyComparison(r.Context(), pathValue(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, cmp)
}

func (s *Server) handlePhenologyDeliver(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, 405, "method_not_allowed", "POST only", nil)
		return
	}
	p := principalFrom(r.Context())
	if !p.inAny("group_analyst") {
		s.fail(w, service.ErrForbidden)
		return
	}
	d, err := s.svc.DeliverPhenologyComparison(r.Context(), pathValue(r, "id"), p.Actor, p.Role)
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if d.Status != "sent" {
		code = http.StatusConflict
	}
	writeJSON(w, code, d)
}

func (s *Server) handlePhenologyDelivery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	d, err := s.svc.GetPhenologyDelivery(r.Context(), pathValue(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *Server) handlePhenologyVerifyDelivery(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, 405, "method_not_allowed", "GET only", nil)
		return
	}
	p := principalFrom(r.Context())
	if !p.inAny("auditor", "group_analyst") {
		s.fail(w, service.ErrForbidden)
		return
	}
	v, err := s.svc.VerifyPhenologyDelivery(r.Context(), pathValue(r, "id"))
	if err != nil {
		s.fail(w, err)
		return
	}
	code := http.StatusOK
	if v.Status == "sent" && (!v.SHA256Matches || len(v.RestrictedHits) > 0 || !v.AuditValid || !v.AuditVerified) {
		code = http.StatusConflict
	}
	writeJSON(w, code, v)
}
