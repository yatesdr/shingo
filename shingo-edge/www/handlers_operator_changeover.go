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

// apiReleaseChangeoverWait gates the changeover wait-points (ready / tooling done).
//
// HMI ORPHAN as of 2026-05-10 (HMI Tier 2). The operator-station
// changeover-wide RELEASE header button was removed and per-node release
// (apiReleaseOrder on the evac order, with HandleBinPickedUp auto-firing
// the supply on pickup-confirm) became the only operator-driven release
// path during changeover. No HMI surface currently posts to this
// endpoint. Two reasons it's intentionally kept rather than deleted:
//
//  1. Future bulk shortcut. If floor pushes back on per-node click count
//     for happy-path multi-node changeovers, a "Release All Ready Nodes"
//     button can repurpose this endpoint with a single confirmation
//     modal upfront (vs the toast-after-batch pattern we removed).
//
//  2. ReleaseChangeoverWait still has an audit-driven contract worth
//     preserving — Phase 2's evac-first sequencing + pending-supply
//     counter — so the engine method stays as a future composition
//     target. Deleting the HTTP wrapper would force a re-derivation if
//     either need surfaces.
//
// If neither condition pans out within a reasonable window, this
// endpoint + handler + the router registration are safe to remove
// together. Until then: marked dead so a reader doesn't waste time
// chasing why the HMI doesn't hit it.
//
// Body shape (when present) matches apiReleaseOrder /
// apiReleaseNodeStagedOrders — disposition string + qty_by_part / partial
// count + called_by. The disposition the operator chose at the modal
// applies to the EVAC leg of each task; the supply leg is always released
// with no manifest action regardless of body content (see
// ReleaseChangeoverWait godoc for the asymmetry rationale).
//
// Body / called_by are OPTIONAL on this endpoint. A bare-body POST is
// accepted and operates with default disposition — evac legs default to
// capture_lineside (release_empty on the wire), supply legs always no-op.
// Empty called_by is logged and replaced with "operator_station" so
// audit trails remain populated if a future caller posts without it.
func (h *Handlers) apiReleaseChangeoverWait(w http.ResponseWriter, r *http.Request) {
	processID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid process id")
		return
	}
	var req releaseRequest
	if r.ContentLength > 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if strings.TrimSpace(req.CalledBy) == "" {
		req.CalledBy = "operator_station"
	}
	disp := buildReleaseDisposition(req)
	// Optional per-node scope. Absent = every task (the changeover-wide
	// release this endpoint has always done); present = just that node's task,
	// which is the affordance the operator board actually uses.
	var result engine.ReleaseChangeoverWaitResult
	if req.NodeID != 0 {
		result, err = h.orchestration.ReleaseChangeoverWaitForNode(processID, req.NodeID, disp)
	} else {
		result, err = h.orchestration.ReleaseChangeoverWait(processID, disp)
	}
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "wait-released"}})
	writeJSONWithTrigger(w, r, map[string]any{
		"status":   "ok",
		"released": result.Released,
		"pending":  result.Pending,
	}, "refreshChangeover")
}

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
	order, err := h.orchestration.StageNodeChangeoverMaterial(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "stage-material"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
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
	order, err := h.orchestration.EvacuateNode(processID, nodeID, req.Qty)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "evacuate-node"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
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
	order, err := h.orchestration.DeliverNewMaterialForChangeover(processID, nodeID)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	h.eventHub.Broadcast(SSEEvent{Type: "changeover-update", Data: map[string]string{"action": "deliver-new-material"}})
	writeJSONWithTrigger(w, r, order, "refreshChangeover")
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
