// handlers_styles.go — style CRUD + style-node-claim endpoints. The
// ensurePressIndexBackNode helper is colocated because it's only called
// from apiUpsertStyleNodeClaim and exists to auto-provision back nodes
// when the operator configures a press-index claim.

package www

import (
	"encoding/json"
	"errors"
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
		Name          string `json:"name"`
		Description   string `json:"description"`
		ProcessID     int64  `json:"process_id"`
		ExpectedCATID string `json:"expected_catid"`
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
	// expected_catid is a separate, optional field (own setter) so the many
	// Create call sites stay untouched. Trim + persist alongside the create.
	if err := h.engine.StyleService().SetExpectedCATID(id, strings.TrimSpace(req.ExpectedCATID)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-created")
	// A new style changes what the process can run, and PlantClaimsReport
	// carries every style with its Active flag — so Core's mirror is wrong
	// until this lands. Missing here until 2026-08-22; the 5-minute snapshot
	// timer hid it by catching up within a tick, which stops being true now
	// the safety snapshot is hourly.
	h.requestSpecChangePublish()
	writeJSON(w, map[string]int64{"id": id})
}

func (h *Handlers) apiUpdateStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name          string `json:"name"`
		Description   string `json:"description"`
		ProcessID     int64  `json:"process_id"`
		ExpectedCATID string `json:"expected_catid"`
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
	// expected_catid is written by its own setter (see apiCreateStyle) so the
	// Update signature and its call sites stay unchanged. Trim + persist.
	if err := h.engine.StyleService().SetExpectedCATID(id, strings.TrimSpace(req.ExpectedCATID)); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-updated")
	// Same as apiCreateStyle: the style NAME is what rides the wire as
	// PlantClaimsStyle.StyleID, so a rename that never republishes leaves Core
	// mirroring a style that no longer exists under that name.
	h.requestSpecChangePublish()
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiStyleDeleteImpact answers "what is behind this delete button".
//
// The delete itself is a soft delete and destroys nothing, so this is not a
// warning — it is the confirmation being honest about scope. On the Springfield
// edge the same button covers a style carrying one row and a style carrying
// 91,581, and the operator currently cannot tell those apart. It is also the
// exact cost of a future hard purge, which is the number that had never been
// put in front of anybody.
func (h *Handlers) apiStyleDeleteImpact(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	imp, err := h.engine.StyleService().DeleteImpact(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, imp)
}

// apiDeleteStyle RETIRES a style. The row survives, so changeover history keeps
// resolving its name and nothing that points at it is left dangling.
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
	// A retired style's claims must leave Core's demand_registry, exactly as a
	// hard delete's would have: ListStylesByProcess (which is what the publisher
	// walks) no longer returns it.
	h.requestSpecChangePublish()
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiRestoreStyle un-retires a style. Soft delete without an undo is just a
// slower delete.
func (h *Handlers) apiRestoreStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	if err := h.engine.StyleService().Restore(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-restored")
	h.requestSpecChangePublish()
	writeJSON(w, map[string]string{"status": "ok"})
}

// apiCloneStyle scaffolds one new style from an existing one, copying its
// node-claim choreography verbatim. The operator edits the per-payload fields
// on the result afterward (in the Node Claims compare grid). The clone starts
// inactive — it never triggers a changeover.
func (h *Handlers) apiCloneStyle(w http.ResponseWriter, r *http.Request) {
	id, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid ID")
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	newID, err := h.engine.StyleService().Clone(id, strings.TrimSpace(req.Name), strings.TrimSpace(req.Description))
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("style-cloned")
	// Cloned claims must reach Core's demand_registry just like edits do.
	h.requestSpecChangePublish()
	writeJSON(w, map[string]int64{"id": newID})
}

// apiGenerateStyles scaffolds a whole family of styles from one base style in
// a single atomic batch — each variant is a clone of the base with its
// per-claim payload overrides applied. The {id} path param is the base style.
// Used by the "Generate variants" action to stand up a press's part-number
// styles (which share node layout and swap choreography) in one shot.
func (h *Handlers) apiGenerateStyles(w http.ResponseWriter, r *http.Request) {
	baseID, err := parseID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid base style ID")
		return
	}
	var req struct {
		Variants []domain.StyleVariant `json:"variants"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	if len(req.Variants) == 0 {
		writeError(w, http.StatusBadRequest, "at least one variant is required")
		return
	}
	ids, err := h.engine.StyleService().GenerateVariants(baseID, req.Variants)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.requestBackup("styles-generated")
	// One sync for the whole batch — requestSpecChangePublish coalesces, and
	// SendClaimSync emits a full snapshot, so a single request covers every
	// new style's claims.
	h.requestSpecChangePublish()
	writeJSON(w, map[string][]int64{"ids": ids})
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
		// A swap_mode rejection (blank, the retired "simple", a typo, a stale
		// import value) is a client input problem — surface it as 400 with the
		// store's message. Genuine DB faults stay 500.
		status := http.StatusInternalServerError
		if errors.Is(err, protocol.ErrInvalidSwapMode) {
			status = http.StatusBadRequest
		}
		writeError(w, status, err.Error())
		return
	}
	// Operator-driven flag is loader-wide (keyed by core_node_name) and
	// Home-location LAYOUT flag — loader-wide / nil-safe like the transitional flag
	// above, but role-NEUTRAL: it's a layout axis (dedicated per-payload node vs one
	// shared window), orthogonal to role per the home_location_loaders data model, so
	// a consume manual_swap (unloader) carries it too. Any manual_swap claim qualifies.
	if in.HomeLocationLoader != nil &&
		in.SwapMode == protocol.SwapModeManualSwap {
		username, _ := h.sessions.getUser(r)
		if err := h.engine.StyleService().SetHomeLocationLoader(in.CoreNodeName, *in.HomeLocationLoader, username); err != nil {
			log.Printf("WARNING api apiUpsertStyleNodeClaim: set home-location loader %s: %v", in.CoreNodeName, err)
		}
	}
	h.requestBackup("style-node-claim-updated")
	h.eventHub.Broadcast(SSEEvent{Type: "material-refresh", Data: map[string]string{"action": "node-claim-updated"}})
	// Push the refreshed claim set to Core so demand_registry stays in sync
	// with what the operator just edited. Fire-and-forget — SendClaimSync
	// logs its own failures and the outbox will retry transient send errors.
	h.requestSpecChangePublish()
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
	// registry drifts and Core keeps threshold bindings for a node whose
	// claim is gone.
	h.requestSpecChangePublish()
	writeJSON(w, map[string]string{"status": "ok"})
}
