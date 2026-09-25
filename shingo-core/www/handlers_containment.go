package www

// handlers_containment.go — the quality-containment API. The payload flag is
// Core-owned state (the divert in dispatch reads it), so its write lives
// here, session-gated like every other Core write; the read is public so the
// Edge's containment screens can render the state without holding Core
// credentials (Core has no machine-auth path — see auth.go).
//
// There is deliberately NO bin-hold endpoint yet. The per-bin flow is
// delivered by the move order the station action creates (order provenance is
// the audit); the bins.quality_hold marker shipped in v100 for the v2
// hardening pass (re-release protection for a bin whose containment move
// failed), and wiring an Edge→Core write for it needs an auth story Core
// does not have — session cookies only.

import (
	"encoding/json"
	"net/http"
	"strings"

	"shingocore/domain"
)

// containmentState is the GET's body: the flag rows and the bins currently
// carrying the hold marker, one read for every screen that needs the
// picture.
type containmentState struct {
	Containment []domain.PayloadContainmentRow `json:"containment"`
	HeldBins    []domain.HeldBinRow            `json:"held_bins"`
}

func (h *Handlers) apiGetContainment(w http.ResponseWriter, r *http.Request) {
	rows, err := h.engine.PayloadService().ListContainment()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	held, err := h.engine.PayloadService().ListHeldBins()
	if err != nil {
		h.jsonError(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(containmentState{Containment: rows, HeldBins: held})
}

func (h *Handlers) apiSetPayloadContainment(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PayloadCode string `json:"payload_code"`
		Active      bool   `json:"active"`
		Reason      string `json:"reason"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	req.PayloadCode = strings.TrimSpace(req.PayloadCode)
	if req.PayloadCode == "" {
		h.jsonError(w, "payload_code is required", http.StatusBadRequest)
		return
	}
	by := h.getUsername(r)
	if by == "" {
		by = "unknown"
	}
	if err := h.engine.PayloadService().SetContainment(req.PayloadCode, req.Reason, by, req.Active); err != nil {
		// The no-routed-claims refusal is a 400 (operator-fixable config),
		// not a 500 — the message names the fix.
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}

// handlePayloadContainment is the payloads screen's form-POST toggle (the
// page's verbs are classic form POSTs with data-action-submit confirms; the
// JSON endpoint above exists for programmatic callers). Redirects back to the
// screen, which re-renders the new state.
func (h *Handlers) handlePayloadContainment(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	payloadCode := strings.TrimSpace(r.FormValue("payload_code"))
	if payloadCode == "" {
		h.jsonError(w, "payload_code is required", http.StatusBadRequest)
		return
	}
	active := r.FormValue("active") == "true"
	reason := strings.TrimSpace(r.FormValue("reason"))
	by := h.getUsername(r)
	if by == "" {
		by = "unknown"
	}
	if err := h.engine.PayloadService().SetContainment(payloadCode, reason, by, active); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	http.Redirect(w, r, "/payloads", http.StatusSeeOther)
}

// apiBinQualityHold is the machine path (telemetry group) the Edge's station
// action calls: set or clear one bin's hold marker. Keyed by bin_id — the
// Edge reads the bin off the node and carries Core's authoritative id, the
// same id its containment move order binds.
func (h *Handlers) apiBinQualityHold(w http.ResponseWriter, r *http.Request) {
	var req struct {
		BinID int64  `json:"bin_id"`
		Hold  bool   `json:"hold"`
		By    string `json:"by"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.By == "" {
		req.By = "unknown"
	}
	if err := h.engine.PayloadService().SetBinHold(req.BinID, req.Hold, req.By); err != nil {
		h.jsonError(w, err.Error(), http.StatusBadRequest)
		return
	}
	json.NewEncoder(w).Encode(map[string]bool{"ok": true})
}
