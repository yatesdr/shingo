// handlers_styles.go — style CRUD + style-node-claim endpoints. The
// ensurePressIndexBackNode helper is colocated because it's only called
// from apiUpsertStyleNodeClaim and exists to auto-provision back nodes
// when the operator configures a press-index claim.

package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
)

// --- Styles Admin ---

func (h *Handlers) apiListStyles(w http.ResponseWriter, r *http.Request) {
	styles, err := h.engine.StyleService().List()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, styles)
}

func (h *Handlers) apiCreateStyle(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ProcessID   int64  `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	id, err := h.engine.StyleService().Create(req.Name, req.Description, req.ProcessID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-created")
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		ProcessID   int64  `json:"process_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ProcessID == 0 {
		writeError(w, http.StatusBadRequest, "process_id is required")
		return
	}
	if err := h.engine.StyleService().Update(id, req.Name, req.Description, req.ProcessID); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-updated")
	writeJSON(w, map[string]string{"status": "ok"})
}

func (h *Handlers) apiDeleteStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.StyleService().Delete(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-deleted")
	writeJSON(w, map[string]string{"status": "ok"})
}

// --- Style Node Claims ---

func (h *Handlers) apiListStyleNodeClaims(w http.ResponseWriter, r *http.Request) {
	styleID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid style id")
		return
	}
	claims, err := h.engine.StyleService().ListClaims(styleID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, claims)
}

func (h *Handlers) apiUpsertStyleNodeClaim(w http.ResponseWriter, r *http.Request) {
	var in domain.NodeClaimInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	// Trim node-name-shaped fields at the API ingress. One-shot
	// warning per field if trim fires — gives operators forensic
	// visibility into upstream whitespace.
	if trimmed := strings.TrimSpace(in.CoreNodeName); trimmed != in.CoreNodeName {
		log.Printf("WARNING api apiUpsertStyleNodeClaim: trimmed whitespace from CoreNodeName %q", in.CoreNodeName)
		in.CoreNodeName = trimmed
	}
	if trimmed := strings.TrimSpace(in.PairedCoreNode); trimmed != in.PairedCoreNode {
		log.Printf("WARNING api apiUpsertStyleNodeClaim: trimmed whitespace from PairedCoreNode %q", in.PairedCoreNode)
		in.PairedCoreNode = trimmed
	}
	if trimmed := strings.TrimSpace(in.SecondPairedCoreNode); trimmed != in.SecondPairedCoreNode {
		log.Printf("WARNING api apiUpsertStyleNodeClaim: trimmed whitespace from SecondPairedCoreNode %q", in.SecondPairedCoreNode)
		in.SecondPairedCoreNode = trimmed
	}
	if in.StyleID == 0 {
		writeError(w, http.StatusBadRequest, "style_id is required")
		return
	}
	if in.CoreNodeName == "" {
		writeError(w, http.StatusBadRequest, "core_node_name is required")
		return
	}
	// Consume-role claims only pay off on LANE-parented storage slots —
	// wiring_kanban.go only emits "consume" demand signals when a bin
	// arrives at a child-of-LANE node (see isStorageSlot). Rejecting
	// here keeps operators from silently configuring dead claims.
	// Produce-role claims are unconstrained: the departure check in
	// wiring_kanban is intentionally LANE-gated too, but producers
	// also have lineside trigger points that are legitimately
	// non-storage (the loader emits fulls back into the supermarket),
	// so leave produce permissive and only guardrail consume.
	if in.Role == protocol.ClaimRoleConsume {
		if info, ok := h.engine.CoreNodes()[in.CoreNodeName]; ok {
			// Empty ParentNodeType means an older Core that hasn't
			// been upgraded yet — skip the check rather than false-
			// reject on a missing field so rolling upgrades work.
			if info.ParentNodeType != "" && info.ParentNodeType != "LANE" {
				writeError(w, http.StatusBadRequest, fmt.Sprintf(
					"consume claims require a LANE-parented storage slot; %s is parented by %s",
					in.CoreNodeName, info.ParentNodeType))
				return
			}
		}
	}
	// Press-index pairing requires the back position(s) to exist as
	// process_nodes so the fleet manager has wait/pickup/dropoff
	// coordinates for R2's leg. The back nodes hold no claim of their
	// own, but their process_node rows must exist. Auto-provision them
	// here using the front node's operator station so the operator
	// doesn't have to add them by hand in a separate step.
	if in.SwapMode == protocol.SwapModeTwoRobotPressIndex {
		if in.PairedCoreNode != "" {
			if err := h.ensurePressIndexBackNode(in, in.PairedCoreNode); err != nil {
				log.Printf("press-index back-node provisioning for %s (paired %s): %v",
					in.CoreNodeName, in.PairedCoreNode, err)
			}
		}
		if in.SecondPairedCoreNode != "" {
			if err := h.ensurePressIndexBackNode(in, in.SecondPairedCoreNode); err != nil {
				log.Printf("press-index second-back-node provisioning for %s (second paired %s): %v",
					in.CoreNodeName, in.SecondPairedCoreNode, err)
			}
		}
	}
	id, err := h.engine.StyleService().UpsertClaim(in)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Transitional-loader flag is loader-wide (keyed by core_node_name) and
	// Edge-only — not a claim column — so it's applied here against the
	// transitional_loaders set rather than persisted by UpsertClaim. Only a
	// produce manual_swap (bin loader) claim can carry it, and only when the
	// request actually included the field (pointer non-nil) so saving an
	// unrelated claim never clears a loader's flag. A failure here is logged
	// but doesn't fail the claim save the operator already committed to.
	if in.TransitionalLoader != nil &&
		in.Role == protocol.ClaimRoleProduce &&
		in.SwapMode == protocol.SwapModeManualSwap {
		username, _ := h.sessions.getUser(r)
		if err := h.engine.StyleService().SetTransitionalLoader(in.CoreNodeName, *in.TransitionalLoader, username); err != nil {
			log.Printf("WARNING api apiUpsertStyleNodeClaim: set transitional loader %s: %v", in.CoreNodeName, err)
		}
	}
	h.requestBackup("style-node-claim-updated")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "node-claim-updated"}})
	// Push the refreshed claim set to Core so demand_registry stays in sync
	// with what the operator just edited. Fire-and-forget — SendClaimSync
	// logs its own failures and the outbox will retry transient send errors.
	h.requestClaimSync()
	writeJSON(w, map[string]int64{"id": id})
}

// ensurePressIndexBackNode creates a process_node row for the given back
// position (B or C) when one doesn't already exist on the same process as
// the front node. Idempotent — does nothing when the back node is already
// a process_node.
func (h *Handlers) ensurePressIndexBackNode(in domain.NodeClaimInput, backCoreNodeName string) error {
	// Defense-in-depth: callers should pass an already-trimmed
	// backCoreNodeName (the API ingress at apiUpsertStyleNodeClaim
	// does this), but trim again here so a future caller from a
	// non-API path can't accidentally bypass it.
	backCoreNodeName = strings.TrimSpace(backCoreNodeName)
	style, err := h.engine.StyleService().Get(in.StyleID)
	if err != nil || style == nil {
		return fmt.Errorf("style lookup: %w", err)
	}
	nodes, err := h.engine.ProcessService().ListNodesByProcess(style.ProcessID)
	if err != nil {
		return fmt.Errorf("list process nodes: %w", err)
	}
	var frontNode *domain.Node
	for i := range nodes {
		if nodes[i].CoreNodeName == backCoreNodeName {
			return nil // already exists
		}
		if nodes[i].CoreNodeName == in.CoreNodeName {
			n := nodes[i]
			frontNode = &n
		}
	}
	if frontNode == nil {
		return fmt.Errorf("front node %s not found in process %d", in.CoreNodeName, style.ProcessID)
	}
	backInput := domain.NodeInput{
		ProcessID:         style.ProcessID,
		OperatorStationID: frontNode.OperatorStationID,
		CoreNodeName:      backCoreNodeName,
		Name:              backCoreNodeName,
		Enabled:           true,
	}
	newID, err := h.engine.ProcessService().CreateNode(backInput)
	if err != nil {
		return fmt.Errorf("create back node: %w", err)
	}
	if _, err := h.engine.ProcessService().EnsureNodeRuntime(newID); err != nil {
		return fmt.Errorf("ensure back-node runtime: %w", err)
	}
	return nil
}

func (h *Handlers) apiDeleteStyleNodeClaim(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid id")
		return
	}
	if err := h.engine.StyleService().DeleteClaim(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-node-claim-deleted")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "node-claim-deleted"}})
	// Claim removed → push the refreshed (shorter) claim set to Core so
	// demand_registry drops the corresponding row. Without this push the
	// registry drifts and Core keeps sending demand signals to a node
	// whose claim is gone.
	h.requestClaimSync()
	writeJSON(w, map[string]string{"status": "ok"})
}
