package www

// handlers_containment.go — the quality-containment API. The payload flag is
// Core-owned state (the divert in dispatch reads it), so its write lives
// here, session-gated like every other Core write; the read is public so the
// Edge's containment screens can render the state without holding Core
// credentials (Core has no machine-auth path — see auth.go).
//
// The per-bin hold has its own write: apiBinQualityHold below, on the
// telemetry machine path (POST /api/telemetry/bin-quality-hold, beside
// bin-load and bin-clear, router.go), which the Edge's station action calls
// when its operator sends a bin to containment. It sets or clears the
// bins.quality_hold marker (v100); the move order the same action creates
// carries the bin, and its provenance is the audit.

import (
	"encoding/json"
	"net/http"
	"strings"

	"shingocore/asks"
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

// qualityHoldRequest is the hold door's body: the bin, set or clear, and who.
type qualityHoldRequest struct {
	BinID int64  `json:"bin_id"`
	Hold  bool   `json:"hold"`
	By    string `json:"by"`
}

// apiBinQualityHold is the machine path (telemetry group) the Edge's station
// action calls: set or clear one bin's hold marker. Keyed by bin_id — the
// Edge reads the bin off the node and carries Core's authoritative id, the
// same id its containment move order binds. The adapter over
// Doors.binQualityHold, which the bin.quality_hold ask answers through too.
func (h *Handlers) apiBinQualityHold(w http.ResponseWriter, r *http.Request) {
	var req qualityHoldRequest
	if err := decodeDoor(r.Body, &req); err != nil {
		writeResult(w, badRequest(errText(err)))
		return
	}
	res := h.doors().binQualityHold(req)
	if res.Status == http.StatusOK {
		// The accept is a bare encode with no Content-Type of its own, as it
		// always was (TestBinQualityHoldDoor_AcceptHasNoContentType).
		_ = json.NewEncoder(w).Encode(res.Body)
		return
	}
	writeResult(w, res)
}

// binQualityHold is the hold's body: the marker set or cleared, {"ok": true};
// a write the store refuses is 400 with its error.
func (d *Doors) binQualityHold(req qualityHoldRequest) asks.Result {
	if req.By == "" {
		req.By = "unknown"
	}
	if err := d.engine.PayloadService().SetBinHold(req.BinID, req.Hold, req.By); err != nil {
		return badRequest(err.Error())
	}
	return accepted(map[string]bool{"ok": true})
}
