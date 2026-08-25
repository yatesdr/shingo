// handlers_operator_changeover.go — changeover ACTION endpoints
// (preview, start, cancel, release-wait, cutover, sequential cutover,
// stage/evac/deliver per-node, switch-to-target).
//
// Distinct from handlers_changeover.go, which renders the changeover
// PAGE and holds the view-DTO types (changeoverNodeView,
// changeoverViewData). Mirrors the engine-side operator_changeover_*.go
// family so action endpoints live near their engine entry points
// rather than in the operator-station catch-all.

package www

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/engine/changeover"
)

// changeoverPreviewAction is the JSON DTO for one node in a changeover preview.
// Mirrors changeover.NodeAction but turns the error into a string and flattens
// the OrderSpec union so the UI can render it without a discriminator dance.
type changeoverPreviewAction struct {
	NodeID      int64                  `json:"node_id"`
	NodeName    string                 `json:"node_name"`
	Situation   string                 `json:"situation"`
	SupplyOrder *changeoverPreviewSpec `json:"supply_order,omitempty"`
	EvacOrder   *changeoverPreviewSpec `json:"evac_order,omitempty"`
	NextState   string                 `json:"next_state,omitempty"`
	LogTag      string                 `json:"log_tag,omitempty"`
	Error       string                 `json:"error,omitempty"`
}

type changeoverPreviewSpec struct {
	Kind         string `json:"kind"` // "complex" or "retrieve"
	DeliveryNode string `json:"delivery_node,omitempty"`
	StagingNode  string `json:"staging_node,omitempty"`
	StepCount    int    `json:"step_count,omitempty"`
	PayloadCode  string `json:"payload_code,omitempty"`
	AutoConfirm  bool   `json:"auto_confirm"`
}

func toPreviewSpec(spec *changeover.OrderSpec) *changeoverPreviewSpec {
	if spec == nil {
		return nil
	}
	if spec.Complex != nil {
		return &changeoverPreviewSpec{
			Kind:         "complex",
			DeliveryNode: spec.Complex.DeliveryNode,
			StepCount:    len(spec.Complex.Steps),
			AutoConfirm:  spec.Complex.AutoConfirm,
		}
	}
	if spec.Retrieve != nil {
		return &changeoverPreviewSpec{
			Kind:         "retrieve",
			DeliveryNode: spec.Retrieve.DeliveryNode,
			StagingNode:  spec.Retrieve.StagingNode,
			PayloadCode:  spec.Retrieve.PayloadCode,
			AutoConfirm:  spec.Retrieve.AutoConfirm,
		}
	}
	return nil
}

func (h *Handlers) apiPreviewProcessChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		ToStyleID int64 `json:"to_style_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	plan, err := h.orchestration.PreviewChangeoverPlan(processID, req.ToStyleID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	dto := struct {
		Actions []changeoverPreviewAction `json:"actions"`
	}{Actions: make([]changeoverPreviewAction, 0, len(plan.Actions))}
	for _, a := range plan.Actions {
		out := changeoverPreviewAction{
			NodeID:      a.NodeID,
			NodeName:    a.NodeName,
			Situation:   a.Situation,
			SupplyOrder: toPreviewSpec(a.SupplyOrder),
			EvacOrder:   toPreviewSpec(a.EvacOrder),
			NextState:   string(a.NextState),
			LogTag:      a.LogTag,
		}
		if a.Err != nil {
			out.Error = a.Err.Error()
		}
		dto.Actions = append(dto.Actions, out)
	}
	writeJSON(w, dto)
}

func (h *Handlers) apiStartProcessChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req struct {
		ToStyleID int64  `json:"to_style_id"`
		CalledBy  string `json:"called_by"`
		Notes     string `json:"notes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	co, err := h.orchestration.StartProcessChangeover(processID, req.ToStyleID, req.CalledBy, req.Notes)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: co})
	writeJSONWithTrigger(w, r, co, "refreshChangeover")
}

func (h *Handlers) apiCancelProcessChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}

	// Parse optional next_style_id for cancel-as-redirect
	var req struct {
		NextStyleID *int64 `json:"next_style_id,omitempty"`
	}
	// Body is optional — plain cancel has no body
	_ = json.NewDecoder(r.Body).Decode(&req)

	if req.NextStyleID != nil {
		if err := h.orchestration.CancelProcessChangeoverRedirect(processID, req.NextStyleID); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "redirected"}})
		writeJSONWithTrigger(w, r, map[string]string{"status": "ok", "action": "redirected"}, "refreshChangeover")
		return
	}

	if err := h.orchestration.CancelProcessChangeover(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "cancelled"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

// apiGetPostCutoverFlag returns the process's post-cutover part-id verification
// flag, or {flagged:false} when none. Polled on station load so a flag raised
// while no one was looking still surfaces.
func (h *Handlers) apiGetPostCutoverFlag(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	flag, ok := h.orchestration.PostCutoverFlag(processID)
	if !ok {
		writeJSON(w, map[string]any{"flagged": false})
		return
	}
	writeJSON(w, map[string]any{"flagged": true, "flag": flag})
}

// apiConfirmPostCutoverFlag clears the flag — the operator confirmed the press
// is correct (or handled it another way). The corrective-changeover exit clears
// it on its own via StartProcessChangeover.
func (h *Handlers) apiConfirmPostCutoverFlag(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	if err := h.orchestration.ClearPostCutoverFlag(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-verify-cleared", Data: map[string]int64{"process_id": processID}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

// apiReleaseChangeoverProcess is the operator's "the setup is finished" click:
// ONE call that releases every staged leg of this changeover.
//
// ── IT WAS DELETED, AND THAT WAS RIGHT AT THE TIME ─────────────────────────
//
// Its ancestor, apiReleaseChangeoverWait, was removed in 2026-08 as an HMI
// orphan — a registered route with no caller is a door nobody checks, and the
// note left behind said a future "Release All Ready Nodes" button would compose
// the engine methods again in a handler written for the button it serves. This
// is that handler, and the button is the one the floor described: a tool change
// is human work at the asset, and when the humans are done they say so ONCE.
//
// The alternative is what the sim measured on 2026-08-24: four legs staged, the
// per-node RELEASE refusing them, and the only working door being per-order
// release from a screen the operator is not standing at — four clicks, none of
// them in front of them.
//
// Body is the shared release shape (called_by required, optional disposition),
// so this endpoint inherits the same audit guard as every other release. The
// engine's per-slot rules are untouched: an evac leg carries the operator's
// disposition, a supply leg paired with one defers to its pickup, and a lone
// supply leg — which is what a cleared seat's single order is — fires now.
func (h *Handlers) apiReleaseChangeoverProcess(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	req, err := parseReleaseRequest(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	res, err := h.orchestration.ReleaseChangeoverWait(processID, buildReleaseDisposition(req))
	if err != nil {
		// Partial failure surfaces as an error with the failing nodes named —
		// the engine joins them rather than reporting a success that left one
		// cell's material where it was.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "release"}})
	writeJSONWithTrigger(w, r, res, "refreshChangeover")
}

// apiReleaseChangeoverWait WAS HERE, and is gone.
//
// It documented itself as an HMI orphan on 2026-05-10 and said the endpoint,
// handler and router registration were safe to remove together if neither of
// its two reasons for existing panned out "within a reasonable window". Three
// months on, neither did: no HMI surface posts to it, and the per-node scope it
// grew in the meantime was never wired to a button either. The operator board
// releases per node through /process-nodes/{id}/release-staged.
//
// A registered route with no caller is a door nobody checks. It still parses a
// body, still builds a disposition, and still fires the SSE broadcast — so it
// is reachable by anything that can reach the API, and it is the one release
// path no operator flow exercises.
//
// The ENGINE side stays: Engine.ReleaseChangeoverWait and
// ReleaseChangeoverWaitForNode keep Phase 2's evac-first sequencing and its
// pending-supply counter, and both are covered by tests. What is gone is the
// HTTP wrapper. A future "Release All Ready Nodes" button composes them again
// in a handler written for the button it serves.

// apiChangeoverGateStatus is the read-only "what is the changeover waiting
// on" endpoint behind the live panel. GET, no mutation, safe to poll.
//
// It exists because the gate's answer was previously only observable by
// ATTEMPTING a cutover and reading the 400 toast — so an operator watching a
// changeover that would not complete had no way to see why without clicking
// the one button that must not be clicked speculatively. Same computation,
// same blockers, no side effects.
//
// No active changeover is not an error: can_complete=true with an empty list,
// which the panel renders as "nothing pending".
func (h *Handlers) apiChangeoverGateStatus(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	canComplete, blockers, err := h.orchestration.ChangeoverGateStatus(processID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if blockers == nil {
		blockers = []domain.Blocker{} // render as [] not null
	}
	writeJSON(w, map[string]any{
		"can_complete": canComplete,
		"blockers":     blockers,
	})
}

func (h *Handlers) apiCompleteProcessProductionCutover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	if err := h.orchestration.CompleteProcessProductionCutover(processID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "cutover-complete"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

// apiSequentialChangeoverCutover is the per-node operator-action endpoint
// for the sequential SWAP mid-sequence cutover. Atomically flips
// ActivePull to the freshly-stocked previously-inactive side and
// releases the wait inside the running complex order. Distinct from
// apiCompleteProcessProductionCutover (the final production-state-flip
// / changeover-completion call); this one fires mid-changeover,
// per-node, and only for sequential SWAP tasks.
func (h *Handlers) apiSequentialChangeoverCutover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		CalledBy string `json:"called_by"`
	}
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&req)
	}
	if strings.TrimSpace(req.CalledBy) == "" {
		req.CalledBy = "operator_station"
	}
	if err := h.orchestration.SequentialChangeoverCutover(processID, nodeID, req.CalledBy); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "sequential-cutover"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiStageNodeChangeoverMaterial(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	_, err = h.orchestration.StageNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "stage-material"}})
	writeActionOK(w, r, "refreshChangeover")
}

func (h *Handlers) apiEvacuateNode(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	var req struct {
		Qty int64 `json:"qty"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	_, err = h.orchestration.EvacuateNode(processID, nodeID, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "evacuate-node"}})
	writeActionOK(w, r, "refreshChangeover")
}

func (h *Handlers) apiDeliverNewMaterialForChangeover(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	_, err = h.orchestration.DeliverNewMaterialForChangeover(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "deliver-new-material"}})
	writeActionOK(w, r, "refreshChangeover")
}

func (h *Handlers) apiSwitchNodeToTarget(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	if err := h.orchestration.SwitchNodeToTarget(processID, nodeID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "switch-to-target"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

// apiAbandonChangeoverNode is the operator exit from awaiting_material — a
// node task whose supply order Core parked for lack of material. Plain POST
// abandons both halves (refused 409 while the partner evac is fleet-active);
// ?accept_half=1 keeps the evac and cancels only the supply. Body-less by
// design so the template buttons can hx-post it like their siblings.
func (h *Handlers) apiAbandonChangeoverNode(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	nodeID, err := parseID(r, "nodeID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid node id")
		return
	}
	acceptHalf := r.URL.Query().Get("accept_half") == "1"
	if err := h.orchestration.AbandonChangeoverSupply(processID, nodeID, acceptHalf, "operator"); err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, engine.ErrPartnerInFlight) {
			// Conflict, not a bad request: the request was well-formed but the
			// floor state forbids it right now. The toast tells the operator
			// their two real options (wait, or accept the half-swap).
			status = http.StatusConflict
		}
		writeError(w, status, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "supply-abandoned"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}

func (h *Handlers) apiSwitchOperatorStationToTarget(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	stationID, err := parseID(r, "stationID")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid station id")
		return
	}
	if err := h.orchestration.SwitchOperatorStationToTarget(processID, stationID); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "switch-station-to-target"}})
	writeJSONWithTrigger(w, r, map[string]string{"status": "ok"}, "refreshChangeover")
}
