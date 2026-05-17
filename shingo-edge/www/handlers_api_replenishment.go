// handlers_api_replenishment.go — API for the replenishment admin page.
//
// PUT    /api/replenishment/loader-threshold      — upsert binding row.
// DELETE /api/replenishment/loader-threshold      — remove binding row.
// PUT    /api/replenishment/cell-reorder          — update claim row.
// POST   /api/replenishment/calculate             — engineer-triggered
//                                                   calculate; pure read,
//                                                   returns inputs +
//                                                   outputs + confidence.
// POST   /api/replenishment/calculate-and-apply   — writes threshold
//                                                   row with source=
//                                                   'calculated' and
//                                                   stamps confidence +
//                                                   computed_at.
// POST   /api/replenishment/override              — writes threshold row
//                                                   with source='manual'
//                                                   and the engineer-
//                                                   typed override value.
// POST   /api/replenishment/recalculate-all       — enumerate every
//                                                   (loader, payload) on
//                                                   active processes and
//                                                   return per-row
//                                                   calculate summaries
//                                                   for engineer review.
//
// There is no audit table; Apply/Override are single upserts. The
// threshold row's source / updated_at / updated_by columns are the
// forensic surface for "what produced the current value".

package www

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"shingoedge/engine"
)

type apiLoaderThresholdReq struct {
	CoreNodeName          string  `json:"core_node_name"`
	PayloadCode           string  `json:"payload_code"`
	ReplenishUOPThreshold int     `json:"replenish_uop_threshold"`
	Source                string  `json:"source"`
	SafetyFactor          float64 `json:"safety_factor"`
}

func (h *Handlers) apiUpsertLoaderThreshold(w http.ResponseWriter, r *http.Request) {
	var req apiLoaderThresholdReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	username, _ := h.sessions.getUser(r)
	if err := h.orchestration.UpsertLoaderThreshold(engine.LoaderThresholdInput{
		CoreNodeName:          req.CoreNodeName,
		PayloadCode:           req.PayloadCode,
		ReplenishUOPThreshold: req.ReplenishUOPThreshold,
		Source:                req.Source,
		SafetyFactor:          req.SafetyFactor,
		UpdatedBy:             username,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiLoaderThresholdDeleteReq struct {
	CoreNodeName string `json:"core_node_name"`
	PayloadCode  string `json:"payload_code"`
}

func (h *Handlers) apiDeleteLoaderThreshold(w http.ResponseWriter, r *http.Request) {
	var req apiLoaderThresholdDeleteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := h.orchestration.DeleteLoaderThreshold(req.CoreNodeName, req.PayloadCode); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiCellReorderReq struct {
	ClaimID      int64  `json:"claim_id"`
	ReorderPoint int    `json:"reorder_point"`
	Source       string `json:"source"`
	AutoReorder  bool   `json:"auto_reorder"`
}

func (h *Handlers) apiUpdateCellReorder(w http.ResponseWriter, r *http.Request) {
	var req apiCellReorderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	if err := h.orchestration.UpdateCellReorder(engine.CellReorderInput{
		ClaimID:      req.ClaimID,
		ReorderPoint: req.ReorderPoint,
		Source:       req.Source,
		AutoReorder:  req.AutoReorder,
	}); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// apiCalculateReq is the modal's "Calculate" submission. Date range is
// in RFC3339 strings to keep JSON unambiguous about timezone.
type apiCalculateReq struct {
	CoreNodeName   string  `json:"core_node_name"`
	PayloadCode    string  `json:"payload_code"`
	DateRangeStart string  `json:"date_range_start"`
	DateRangeEnd   string  `json:"date_range_end"`
	SafetyFactor   float64 `json:"safety_factor"`
	CycleSeconds   float64 `json:"cycle_seconds"`
}

func (req apiCalculateReq) toEngineInput() (engine.CalculateInput, error) {
	start, err := time.Parse(time.RFC3339, req.DateRangeStart)
	if err != nil {
		return engine.CalculateInput{}, err
	}
	end, err := time.Parse(time.RFC3339, req.DateRangeEnd)
	if err != nil {
		return engine.CalculateInput{}, err
	}
	return engine.CalculateInput{
		CoreNodeName:   req.CoreNodeName,
		PayloadCode:    req.PayloadCode,
		DateRangeStart: start,
		DateRangeEnd:   end,
		SafetyFactor:   req.SafetyFactor,
		CycleSeconds:   req.CycleSeconds,
	}, nil
}

func (h *Handlers) apiCalculateThresholds(w http.ResponseWriter, r *http.Request) {
	var req apiCalculateReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	in, err := req.toEngineInput()
	if err != nil {
		http.Error(w, "invalid date range: "+err.Error(), http.StatusBadRequest)
		return
	}
	result, err := h.orchestration.CalculateThresholdForLoader(in)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// apiCalculateApplyReq is the engineer's Apply submission. The
// client echoes back the Calculate response's threshold output,
// confidence, and computed_at timestamp so the threshold row records
// what produced the value without a server-side audit-table lookup.
type apiCalculateApplyReq struct {
	apiCalculateReq
	Value               int      `json:"value"`
	Confidence          string   `json:"confidence"`
	ThresholdCalculated int      `json:"threshold_calculated"`
	ComputedAt          string   `json:"computed_at"`
	OverriddenInputs    []string `json:"overridden_inputs,omitempty"`
}

func (h *Handlers) apiCalculateAndApply(w http.ResponseWriter, r *http.Request) {
	var req apiCalculateApplyReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	username, _ := h.sessions.getUser(r)
	thr := engine.LoaderThresholdInput{
		CoreNodeName:          req.CoreNodeName,
		PayloadCode:           req.PayloadCode,
		ReplenishUOPThreshold: req.Value,
		SafetyFactor:          req.SafetyFactor,
		Confidence:            req.Confidence,
		ThresholdCalculated:   req.ThresholdCalculated,
		ThresholdCalculatedAt: nullableTimestamp(req.ComputedAt),
		OverriddenInputs:      joinOverrides(req.OverriddenInputs),
		UpdatedBy:             username,
	}
	if err := h.orchestration.ApplyCalculatedThreshold(thr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type apiOverrideReq struct {
	CoreNodeName        string   `json:"core_node_name"`
	PayloadCode         string   `json:"payload_code"`
	OverrideValue       int      `json:"override_value"`
	Confidence          string   `json:"confidence"`
	ThresholdCalculated int      `json:"threshold_calculated"`
	ComputedAt          string   `json:"computed_at"`
	OverriddenInputs    []string `json:"overridden_inputs,omitempty"`
}

func (h *Handlers) apiOverrideThreshold(w http.ResponseWriter, r *http.Request) {
	var req apiOverrideReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	username, _ := h.sessions.getUser(r)
	thr := engine.LoaderThresholdInput{
		CoreNodeName:          req.CoreNodeName,
		PayloadCode:           req.PayloadCode,
		Confidence:            req.Confidence,
		ThresholdCalculated:   req.ThresholdCalculated,
		ThresholdCalculatedAt: nullableTimestamp(req.ComputedAt),
		OverriddenInputs:      joinOverrides(req.OverriddenInputs),
		UpdatedBy:             username,
	}
	if err := h.orchestration.OverrideCalculatedThreshold(req.OverrideValue, thr); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nullableTimestamp wraps the client-echoed computed_at string so
// blank values become a NULL on the threshold row rather than the
// zero-time literal.
func nullableTimestamp(s string) sql.NullString {
	s = strings.TrimSpace(s)
	if s == "" {
		return sql.NullString{}
	}
	return sql.NullString{String: s, Valid: true}
}

// joinOverrides serializes the engineer's overridden-input list to
// the canonical comma-separated storage form. Whitespace and empty
// entries are dropped; order is preserved (the UI sends fields in
// the order they appear in the modal, which is also the order they
// render under the "Overrides:" label on the threshold row).
func joinOverrides(items []string) string {
	out := make([]string, 0, len(items))
	for _, s := range items {
		s = strings.TrimSpace(s)
		if s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ",")
}

// apiRecalculateAllReq carries the common parameters that apply to
// every binding in the sweep — date range, safety factor, cycle time.
// The engineer can adjust each row's values after seeing the summary
// but the bulk sweep uses one shared input set.
type apiRecalculateAllReq struct {
	DateRangeStart string  `json:"date_range_start"`
	DateRangeEnd   string  `json:"date_range_end"`
	SafetyFactor   float64 `json:"safety_factor"`
	CycleSeconds   float64 `json:"cycle_seconds"`
}

// apiRecalculateAllRow is one binding's calculate result in the
// summary response. BinCapacityUOP carries the loader's per-bin UOP
// capacity so the client can render the "≈ N bins" annotation; 0 means
// no claim was resolvable and the annotation is suppressed.
type apiRecalculateAllRow struct {
	CoreNodeName   string `json:"core_node_name"`
	PayloadCode    string `json:"payload_code"`
	Threshold      int    `json:"threshold"`
	CellReorder    int    `json:"cell_reorder"`
	Confidence     string `json:"confidence"`
	BinCapacityUOP int    `json:"bin_capacity_uop"`
	Error          string `json:"error,omitempty"`
}

func (h *Handlers) apiRecalculateAll(w http.ResponseWriter, r *http.Request) {
	var req apiRecalculateAllReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid json", http.StatusBadRequest)
		return
	}
	start, err := time.Parse(time.RFC3339, req.DateRangeStart)
	if err != nil {
		http.Error(w, "invalid date_range_start", http.StatusBadRequest)
		return
	}
	end, err := time.Parse(time.RFC3339, req.DateRangeEnd)
	if err != nil {
		http.Error(w, "invalid date_range_end", http.StatusBadRequest)
		return
	}
	pairs, err := h.orchestration.ListLoaderClaimsForRecalculate()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	out := make([]apiRecalculateAllRow, 0, len(pairs))
	for _, p := range pairs {
		res, err := h.orchestration.CalculateThresholdForLoader(engine.CalculateInput{
			CoreNodeName:   p.CoreNodeName,
			PayloadCode:    p.PayloadCode,
			DateRangeStart: start,
			DateRangeEnd:   end,
			SafetyFactor:   req.SafetyFactor,
			CycleSeconds:   req.CycleSeconds,
		})
		row := apiRecalculateAllRow{
			CoreNodeName: p.CoreNodeName,
			PayloadCode:  p.PayloadCode,
		}
		if err != nil {
			row.Error = err.Error()
		} else {
			row.Threshold = res.Outputs.L1Threshold
			row.CellReorder = res.Outputs.CellReorder
			row.Confidence = res.Confidence
			row.BinCapacityUOP = res.Inputs.BinCapacityUOP
		}
		out = append(out, row)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
