// handlers_operator_actions.go — per-node operator actions on the
// material flow: request supply, release the bin (empty / partial /
// staged-orders) and finalize a produce node. The two-robot staged-
// orders release goes through parseReleaseRequest + buildReleaseDisposition
// (handlers_release.go) so every release endpoint inherits the same
// post-2026-04-27 body-validation guard.

package www

import (
	"encoding/json"
	"net/http"

	"shingoedge/domain"
)

func (h *Handlers) apiRequestNodeMaterial(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.orchestration.RequestNodeMaterial(id, 1)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
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
	var order *domain.Order
	if req.PartialCount != nil {
		count := *req.PartialCount
		if count < 0 {
			writeError(w, http.StatusBadRequest, "partial_count must be >= 0")
			return
		}
		order, err = h.orchestration.ReleaseNodeWithRemainingUOP(id, 1, count)
	} else {
		order, err = h.orchestration.ReleaseNodeEmpty(id)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
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
	order, err := h.orchestration.ReleaseNodePartial(id, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, order, "refreshMaterial")
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

func (h *Handlers) apiRequestProduceSwap(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	result, err := h.orchestration.RequestProduceSwap(id)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSONWithTrigger(w, r, result, "refreshMaterial")
}
