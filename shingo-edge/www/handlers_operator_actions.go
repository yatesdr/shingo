// handlers_operator_actions.go — per-node operator actions on the
// material flow: request supply, release the bin (empty / partial /
// staged-orders) and finalize a produce node. The two-robot staged-
// orders release goes through parseReleaseRequest + buildReleaseDisposition
// (handlers_release.go) so every release endpoint inherits the same
// post-2026-04-27 body-validation guard.

package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"

	"shingoedge/engine"
)

// writeMaterialRequestError turns a material-request refusal into a response the
// operator station can act on. An armed-changeover block (ChangeoverArmedError)
// ships an inline exit carrying the process's cancel URL, so the operator can
// clear it and retry without a refresh; every other refusal is a plain error
// (2026-07-24).
//
// The exit cancels the WHOLE changeover, and its label says so. It used to read
// "Abandon changeover", which reads like skipping one node; the per-node exit is
// the node's own skip, not this.
//
// Every refusal is logged here, the one door both operator REQUESTs (consume and
// produce) answer through. The refusal otherwise lived only in the HTTP response,
// so a line that never got material left no record of why (SPR 2026-10-14). The
// auto-reorder sweep logs its own refusals (demand_reconciler.go), so this logs
// the operator's alone.
func writeMaterialRequestError(w http.ResponseWriter, nodeID int64, kind string, err error) {
	log.Printf("operator %s request refused: node %d: %v", kind, nodeID, err)
	var armed *engine.ChangeoverArmedError
	if errors.As(err, &armed) {
		writeErrorWithExit(w, http.StatusBadRequest, armed.Error(), actionExit{
			Kind:  "cancel_changeover",
			URL:   fmt.Sprintf("/api/processes/%d/changeover/cancel", armed.ProcessID),
			Label: "Cancel changeover to " + armed.ToStyleName,
		})
		return
	}
	// An ADVISORY refusal is the system working, not a fault: the request was
	// declined because the thing it needed is already happening. `error` is
	// unchanged for every existing consumer; `notice` is additive and tells a
	// client that knows about it to render this quietly rather than in the
	// colour reserved for things that are broken.
	var advisory interface{ Advisory() bool }
	if errors.As(err, &advisory) && advisory.Advisory() {
		writeAdvisoryRefusal(w, err.Error())
		return
	}
	writeError(w, http.StatusBadRequest, err.Error())
}

func (h *Handlers) apiRequestNodeMaterial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.orchestration.RequestNodeMaterial(id, 1)
	if err != nil {
		writeMaterialRequestError(w, id, "material", err)
		return
	}
	writeJSONWithTrigger(w, r, result, "refreshMaterial")
}

func (h *Handlers) apiReleaseNodeEmpty(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	// Optional body — if present and has a non-nil partial_count, the
	// operator has declared the bin's actual remaining count via prompt
	// (Material page Release flow). Route through the explicit-override
	// path so a stale runtime cache can't silently wipe a partial bin.
	// Empty body or missing field falls through to the legacy
	// cache-driven path used by code paths that don't expose a prompt.
	var req struct {
		PartialCount *int `json:"partial_count,omitempty"`
	}
	if r.Body != nil {
		// Body is optional — ignore decode errors (empty body etc.) and
		// fall through to the legacy ReleaseNodeEmpty path.
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if req.PartialCount != nil {
		count := *req.PartialCount
		if count < 0 {
			writeError(w, http.StatusBadRequest, "partial_count must be >= 0")
			return
		}
		_, err = h.orchestration.ReleaseNodeWithRemainingUOP(id, 1, count)
	} else {
		_, err = h.orchestration.ReleaseNodeEmpty(id)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeActionOK(w, r, "refreshMaterial")
}

func (h *Handlers) apiReleaseNodePartial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		Qty int64 `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	_, err = h.orchestration.ReleaseNodePartial(id, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeActionOK(w, r, "refreshMaterial")
}

// apiReleaseNodeStagedOrders releases both orders of a two-robot swap in one
// call. See Engine.ReleaseStagedOrders for ordering (B-then-A) and
// idempotency semantics.
//
// Phase 7 (lineside): the HMI's release prompt posts qty_by_part on this
// endpoint too (single release path covers single-order and two-robot
// swaps). Forwarded to the engine so two-robot releases capture lineside
// buckets like the single-order path does.
//
// Phase 8 (release-time manifest): body now also carries a disposition so
// the "SEND PARTIAL BACK" button can return the partially-consumed bin to
// the supermarket instead of declaring it empty. Legacy body shape
// (qty_by_part only, missing disposition) maps to zero-value disposition,
// which leaves the bin's manifest untouched at Core.
func (h *Handlers) apiReleaseNodeStagedOrders(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	// Body validation lives in parseReleaseRequest (handlers_release.go) so
	// every release endpoint inherits the same post-2026-04-27 guard. See
	// that function's docstring for the contract.
	req, err := parseReleaseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	disp := buildReleaseDisposition(req)
	if err := h.orchestration.ReleaseStagedOrders(id, disp); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

// apiResumeKeepStaged is the board's RESUME on a line whose keep-staged keeper
// a cancel paused. It re-arms the keeper and refills the spot; nothing is sent
// to the line.
func (h *Handlers) apiResumeKeepStaged(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.ResumeKeepStaged(id); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshMaterial")
}

func (h *Handlers) apiRequestProduceSwap(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	// bin_id: the held partial the operator picked from the REQUEST list (a
	// finish-later claim); absent lets Core choose, oldest held first.
	var body struct {
		BinID int64 `json:"bin_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	result, err := h.orchestration.RequestProduceSwap(id, body.BinID)
	if err != nil {
		writeMaterialRequestError(w, id, "produce", err)
		return
	}
	writeJSONWithTrigger(w, r, result, "refreshMaterial")
}

// apiHeldPartials is Core's list of a part's held partials, oldest held first,
// for a finish-later claim's REQUEST: the operator may pick which one the
// swap fetches (its bin_id rides the request).
func (h *Handlers) apiHeldPartials(w http.ResponseWriter, r *http.Request) {
	payload := r.URL.Query().Get("payload")
	if payload == "" {
		writeError(w, http.StatusBadRequest, "payload is required")
		return
	}
	core := h.engine.CoreAPI()
	if core == nil {
		writeError(w, http.StatusServiceUnavailable, "core API not configured")
		return
	}
	list, err := core.FetchHeldPartials(payload)
	if err != nil {
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, list)
}
