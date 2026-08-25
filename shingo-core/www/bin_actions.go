package www

import (
	"encoding/json"
	"fmt"
	"log"
	"strconv"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/engine"
)

// binActionFunc is the handler signature for individual bin actions.
type binActionFunc func(b *domain.Bin, params json.RawMessage) error

func (h *Handlers) executeBinAction(b *domain.Bin, action string, params json.RawMessage) error {
	actions := map[string]binActionFunc{
		"activate":           h.binActivate,
		"flag":               h.binFlag,
		"quality_hold":       h.binQualityHold,
		"maintenance":        h.binMaintenance,
		"retire":             h.binRetire,
		"release":            h.binRelease,
		"stage":              h.binStage,
		"lock":               h.binLock,
		"unlock":             h.binUnlock,
		"load_payload":       h.binLoadPayload,
		"clear":              h.binClear,
		"confirm_manifest":   h.binConfirmManifest,
		"unconfirm_manifest": h.binUnconfirmManifest,
		"move":               h.binMove,
		"record_count":       h.binRecordCount,
		"add_note":           h.binAddNote,
		"update":             h.binUpdate,
	}
	fn, ok := actions[action]
	if !ok {
		return fmt.Errorf("unknown action: %s", action)
	}
	return fn(b, params)
}

// --- Bin action handlers (bound method values used by executeBinAction) ---

func (h *Handlers) binActivate(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, domain.BinStatusAvailable); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusAvailable), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binFlag(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, domain.BinStatusFlagged); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusFlagged), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binQualityHold(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Reason string `json:"reason"`
		Actor  string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	svc := h.engine.BinService()
	if err := svc.ChangeStatus(b.ID, domain.BinStatusQualityHold); err != nil {
		return err
	}
	actor := h.resolveActor(p.Actor)
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusQualityHold), actor)
	if p.Reason != "" {
		svc.AddNote(b.ID, "hold", p.Reason, actor)
	}
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binMaintenance(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().ChangeStatus(b.ID, domain.BinStatusMaintenance); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusMaintenance), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binRetire(b *domain.Bin, _ json.RawMessage) error {
	// Retire sets status='retired' AND node_id=NULL atomically so the
	// carrier vacates production immediately. Pre-Round-3 this handler
	// only changed status; the bin stayed at its old node and continued
	// to occupy that slot in node-tile / capacity readers — operators
	// had to manually move retired bins to an out-of-line node to free
	// the slot. Routing through BinService.Retire closes that gap.
	if err := h.engine.BinService().Retire(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusRetired), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binRelease(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Release(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", string(domain.BinStatusStaged), string(domain.BinStatusAvailable), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binStage(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Stage(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "status", b.Status.String(), string(domain.BinStatusStaged), protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) binLock(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Actor string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	if err := h.engine.BinService().Lock(b.ID, actor); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "locked", "", actor, actor)
	h.emitBinUpdate(b, "locked", actor)
	return nil
}

func (h *Handlers) binUnlock(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Unlock(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "unlocked", b.LockedBy, "", protocol.AuditActorUI)
	h.emitBinUpdate(b, "unlocked", "")
	return nil
}

func (h *Handlers) binLoadPayload(b *domain.Bin, params json.RawMessage) error {
	// UOPOverride is *int so an explicit zero survives the wire: absent =
	// template capacity; present (0 included) = the operator's declared
	// count. The old int-zero sentinel forced every "labeled but empty"
	// load to full capacity (HK 2026-07-16).
	var p struct {
		PayloadCode string `json:"payload_code"`
		UOPOverride *int   `json:"uop_override"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	// Epoch return discarded — admin "Load Payload" lands directly on
	// Core via the bin detail modal, with no Edge response carrying the
	// new value. The station is told anyway: the bump announces itself.
	// This used to say the Edge picked it up "on its next bin-state
	// refresh", which never existed.
	if _, err := h.engine.BinService().LoadPayload(b.ID, p.PayloadCode, p.UOPOverride); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "loaded", "", p.PayloadCode, protocol.AuditActorUI)
	fresh, _ := h.engine.BinService().GetBin(b.ID)
	if fresh != nil {
		b = fresh
	}
	h.emitBinUpdate(b, "loaded", p.PayloadCode)
	return nil
}

func (h *Handlers) binClear(b *domain.Bin, _ json.RawMessage) error {
	oldCode := b.PayloadCode
	epoch, err := h.engine.BinService().Manifest().ClearForReuse(b.ID, nil)
	if err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "cleared", oldCode, "", protocol.AuditActorUI)
	h.emitBinUpdate(b, "cleared", "")
	// Tell Edge the bin at this node is now EMPTY — not that it left.
	//
	// This used to send Released=true, borrowed from binMove. Released means "the
	// bin was moved OFF this node", so Edge unbinds active_bin_id and stops
	// attributing PLC ticks to it. Clear moves nothing: the carrier is still
	// physically on the node, it is merely empty now. Unbinding it stranded the
	// slot — ticks piled into pending_uop_delta against no bin — and on a node
	// that was ALREADY unbound the correction was rejected outright ("bin N not
	// active at node X — bound elsewhere, ignoring"), so Clear could not even
	// repair what it had broken. That is the trap behind HK 2026-07-28: the
	// operator's instinctive fix was the one action that guaranteed no recovery.
	//
	// A plain count correction to 0 is right in both states: a bound node keeps
	// its binding and zeroes the tile, and an unbound node BINDS the staged
	// carrier through the count-correction repair path. Released stays in use by
	// binMove, where the bin genuinely did leave.
	if b.NodeName != "" {
		if err := h.orchestration.SendDataToEdge(protocol.SubjectUOPAdjustment, protocol.StationBroadcast, &protocol.UOPAdjustment{
			BinID:        b.ID,
			CoreNodeName: b.NodeName,
			NewRemaining: 0,
			Epoch:        epoch,
			Actor:        protocol.AuditActorUI,
			AdjustedAt:   time.Now().UTC(),
		}); err != nil {
			log.Printf("bin_clear: zero-count broadcast bin %d (node %s): %v", b.ID, b.NodeName, err)
		}
	}
	return nil
}

func (h *Handlers) binConfirmManifest(b *domain.Bin, _ json.RawMessage) error {
	if b.Manifest == nil {
		return fmt.Errorf("bin has no manifest to confirm")
	}
	if err := h.engine.BinService().Manifest().Confirm(b.ID, ""); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "confirmed", "unconfirmed", "confirmed", protocol.AuditActorUI)
	h.emitBinUpdate(b, "loaded", "")
	return nil
}

func (h *Handlers) binUnconfirmManifest(b *domain.Bin, _ json.RawMessage) error {
	if err := h.engine.BinService().Manifest().Unconfirm(b.ID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "unconfirmed", "confirmed", "unconfirmed", protocol.AuditActorUI)
	h.emitBinUpdate(b, "loaded", "")
	return nil
}

func (h *Handlers) binMove(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		NodeID int64 `json:"node_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	res, err := h.engine.BinService().Move(b, p.NodeID)
	if err != nil {
		return err
	}
	// RE-ARM THE LOG, for the same reason apiClearTransitAnomaly does: a bin
	// moved off `_TRANSIT` or off a deck has its anomaly cleared in the same
	// statement (bins.MoveOffTransit), and the engine's silencer holds the note
	// it last printed for that bin. Unconditional on purpose — the map is a
	// record of what was said, not state, and forgetting a note that was not
	// stale only means the next genuine stranding announces itself, which is
	// what it is supposed to do.
	h.engine.Recovery().ForgetStrandedNote(b.ID)
	h.engine.AuditService().Append("bin", b.ID, "moved", b.NodeName, res.DestNode.Name, protocol.AuditActorUI)
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
		BinID:       b.ID,
		NodeID:      p.NodeID,
		Action:      "moved",
		PayloadCode: b.PayloadCode,
		FromNodeID:  derefInt64(b.NodeID),
		ToNodeID:    p.NodeID,
	}})

	// Tell Edge to release the bin from its OLD node's runtime so that node
	// stops attributing PLC consumption to a bin that has moved away (the
	// "moved bin keeps counting down" bug). Reuses the UOP-adjustment Core→Edge
	// channel with Released=true; the Edge handler's active-bin guard ensures
	// only the node still pointing at this bin clears it. Broadcast like
	// binRecordCount — Core shouldn't pre-resolve a node-scoped change to one
	// station. (b.NodeName is still the OLD node here; Move() doesn't mutate b.)
	if b.NodeName != "" {
		if err := h.orchestration.SendDataToEdge(protocol.SubjectUOPAdjustment, protocol.StationBroadcast, &protocol.UOPAdjustment{
			BinID:        b.ID,
			CoreNodeName: b.NodeName,
			Released:     true,
			Actor:        protocol.AuditActorUI,
			AdjustedAt:   time.Now().UTC(),
		}); err != nil {
			log.Printf("bin_move: release-from-edge broadcast bin %d (old node %s): %v", b.ID, b.NodeName, err)
		}
	}

	// Tell Edge to BIND the moved bin onto its NEW node's runtime so that node
	// resumes counting it. A Core admin Move mirrors a physical relocation the
	// robot-delivery path never recorded (manual fork-truck recovery, a failed
	// delivery that left the bin unregistered) — without this the destination's
	// active_bin_id stays nil and its PLC ticks attribute to nothing. The dual
	// of the release broadcast above. Move() already rejected the relocation if
	// the destination held another bin, so there is no live bin to clobber. Seed
	// remaining + epoch from the bin's authoritative values so the operator
	// screen and the first tick's delta are correct. Skip synthetic dests
	// (transit / group nodes aren't Edge process nodes).
	if res.DestNode != nil && !res.DestNode.IsSynthetic {
		if err := h.orchestration.SendDataToEdge(protocol.SubjectUOPAdjustment, protocol.StationBroadcast, &protocol.UOPAdjustment{
			BinID:        b.ID,
			CoreNodeName: res.DestNode.Name,
			NewRemaining: b.UOPRemaining,
			Epoch:        b.DeltaEpoch,
			Bound:        true,
			Actor:        protocol.AuditActorUI,
			AdjustedAt:   time.Now().UTC(),
		}); err != nil {
			log.Printf("bin_move: bind-to-edge broadcast bin %d (new node %s): %v", b.ID, res.DestNode.Name, err)
		}
	}
	return nil
}

func (h *Handlers) binRecordCount(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		ActualUOP int    `json:"actual_uop"`
		Actor     string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	svc := h.engine.BinService()
	res, err := svc.RecordCount(b, p.ActualUOP, actor)
	if err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "counted", strconv.Itoa(res.Expected), strconv.Itoa(res.Actual), actor)
	if res.Discrepancy {
		svc.AddNote(b.ID, "count",
			fmt.Sprintf("Cycle count discrepancy: expected %d, actual %d (%+d)", res.Expected, res.Actual, res.Actual-res.Expected),
			actor)
	}
	// Over-capacity counts are accepted but flagged: the note surfaces on the bin
	// tab and the service already wrote a distinct over-capacity audit row.
	if res.Warning != "" {
		svc.AddNote(b.ID, "count", res.Warning, actor)
	}
	h.emitBinUpdate(b, "counted", "")

	// Broadcast the corrected UOP to every Edge station. The station that
	// models this Core node applies it: if the bin is already bound there it
	// updates the count; if the bin is staged-but-unbound (the SNF3 detachment,
	// where a delivered bin never bound so its ticks stranded), the correction
	// now BINDS it with the corrected value (P2-C5). Epoch carries the bin's
	// delta_epoch so that bind seeds active_bin_epoch correctly and the resumed
	// BinUOPDeltas carry the right generation for Core's epoch-aware dedup —
	// without it a freshly bound bin would emit epoch-0 deltas that Core drops,
	// re-detaching the very stream this correction is meant to reconnect. Every
	// other station no-ops (it doesn't model this Core node). This mirrors the
	// broadcast pattern already used for SubjectNodeStructureChanged: a
	// node-scoped change Core shouldn't try to pre-resolve to one station.
	if b.NodeID != nil {
		if err := h.orchestration.SendDataToEdge(protocol.SubjectUOPAdjustment, protocol.StationBroadcast, &protocol.UOPAdjustment{
			BinID:        b.ID,
			CoreNodeName: b.NodeName,
			NewRemaining: res.Actual,
			Epoch:        b.DeltaEpoch,
			Actor:        actor,
			AdjustedAt:   time.Now().UTC(),
		}); err != nil {
			log.Printf("uop_adjustment: broadcast bin %d (node %s): %v", b.ID, b.NodeName, err)
		}
	}

	return nil
}

func (h *Handlers) binAddNote(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		NoteType string `json:"note_type"`
		Message  string `json:"message"`
		Actor    string `json:"actor"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	actor := h.resolveActor(p.Actor)
	return h.engine.BinService().AddNote(b.ID, p.NoteType, p.Message, actor)
}

func (h *Handlers) binUpdate(b *domain.Bin, params json.RawMessage) error {
	var p struct {
		Label       *string `json:"label"`
		Description *string `json:"description"`
		BinTypeID   *int64  `json:"bin_type_id"`
	}
	if err := json.Unmarshal(params, &p); err != nil && len(params) > 0 {
		return fmt.Errorf("invalid params: %w", err)
	}
	if err := h.engine.BinService().Update(b, p.Label, p.Description, p.BinTypeID); err != nil {
		return err
	}
	h.engine.AuditService().Append("bin", b.ID, "updated", "", "", protocol.AuditActorUI)
	h.emitBinUpdate(b, "status_changed", "")
	return nil
}

func (h *Handlers) emitBinUpdate(b *domain.Bin, action, detail string) {
	h.engine.EventBus().Emit(engine.Event{Type: engine.EventBinUpdated, Payload: engine.BinUpdatedEvent{
		BinID:       b.ID,
		NodeID:      derefInt64(b.NodeID),
		Action:      action,
		PayloadCode: b.PayloadCode,
		Detail:      detail,
		Actor:       protocol.AuditActorUI,
	}})
}

func (h *Handlers) resolveActor(actor string) string {
	if actor != "" {
		return actor
	}
	return protocol.AuditActorUI
}
