package dispatch

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"

	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// persistedBlocker is the JSON wire shape stored in
// pending_restocks.restock_plan_json. Captures everything needed to
// rebuild a restock step at fire time without re-querying the
// resolver.
type persistedBlocker struct {
	BinID          int64  `json:"bin_id"`
	OriginalNodeID int64  `json:"original_node_id"`
	OriginalName   string `json:"original_name"`
	ShuffleNodeID  int64  `json:"shuffle_node_id"`
	ShuffleName    string `json:"shuffle_name"`
}

type persistedRestockPlan struct {
	LaneID         int64              `json:"lane_id"`
	GroupID        int64              `json:"group_id"`
	TargetSlotID   int64              `json:"target_slot_id"`
	TargetSlotName string             `json:"target_slot_name"`
	Blockers       []persistedBlocker `json:"blockers"`
}

// restoreBlocker captures one blocker's original (lane) position and
// the shuffle slot it landed in during the unbury pass. The restock
// compound moves the bin from shuffle back to original.
type restoreBlocker struct {
	bin      *bins.Bin
	original *nodes.Node // lane slot the bin was buried at originally
	shuffle  *nodes.Node // shuffle slot the unbury step moved it to
}

// restoreEntry is the in-memory state for a single pending restore-
// blockers compound. Crash-volatile (same shape as LaneLock); a Core
// restart drops the registry and the lane stays in its post-unbury
// state, which is correct — the parent retried after restart will see
// the new geometry and pick up from wherever the bin currently is.
type restoreEntry struct {
	syntheticParent  *orders.Order
	complexParentID  int64
	targetBinID      int64
	expectedFromNode int64       // node ID we expect to see the bin LEAVING
	laneID           int64       // for unlocking on listener fire
	groupID          int64       // for the restock plan
	targetSlot       *nodes.Node // target bin's original lane slot — used by restockDestinations for bubble-free packing
	blockers         []restoreBlocker
}

// restoreRegistry tracks pending restore-blockers listeners keyed by
// (1) target bin ID for the EventBinEnteredTransit dispatch path and
// (2) complex parent ID for the EventOrderCancelled/Failed deregister
// path. Both indexes are mutex-guarded.
type restoreRegistry struct {
	mu        sync.Mutex
	byBin     map[int64]*restoreEntry
	byComplex map[int64]int64 // complexParentID -> targetBinID
}

func newRestoreRegistry() *restoreRegistry {
	return &restoreRegistry{
		byBin:     make(map[int64]*restoreEntry),
		byComplex: make(map[int64]int64),
	}
}

// Register stores a pending restore. Returns false if a registration
// for the same complex parent already exists (idempotency guard).
func (r *restoreRegistry) Register(entry *restoreEntry) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.byComplex[entry.complexParentID]; exists {
		return false
	}
	r.byBin[entry.targetBinID] = entry
	r.byComplex[entry.complexParentID] = entry.targetBinID
	return true
}

// ConsumeByBin atomically removes and returns the entry keyed by bin
// ID. Used by the EventBinEnteredTransit handler; one-shot semantics.
// Returns nil if no entry matches.
func (r *restoreRegistry) ConsumeByBin(binID int64) *restoreEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	entry, ok := r.byBin[binID]
	if !ok {
		return nil
	}
	delete(r.byBin, binID)
	delete(r.byComplex, entry.complexParentID)
	return entry
}

// ConsumeByComplexParent removes the entry for a complex parent.
// Used by EventOrderCancelled / EventOrderFailed handlers — no restock
// runs in those cases.
func (r *restoreRegistry) ConsumeByComplexParent(complexParentID int64) *restoreEntry {
	r.mu.Lock()
	defer r.mu.Unlock()
	binID, ok := r.byComplex[complexParentID]
	if !ok {
		return nil
	}
	entry := r.byBin[binID]
	delete(r.byBin, binID)
	delete(r.byComplex, complexParentID)
	return entry
}

// ScheduleRestoreIfEnabled checks the group's restore-blockers toggle.
// When on, creates the synthetic-restore parent at StatusReshuffling
// (no lifecycle transition because the helper writes the initial row
// directly) and registers a listener.
//
// Called by handleComplexBuriedAtIntake / handleComplexBuriedOnReplay
// after CreateCompoundOrder schedules the unbury (or unbury+retrieve)
// compound for the complex parent.
//
// expectedFromNode is the node ID the target bin should leave when
// the parent picks it up: the original lane slot in expose mode, or
// the configured target node in target-node mode.
func (d *Dispatcher) scheduleRestoreIfEnabled(
	complexParent *orders.Order,
	groupID, laneID int64,
	plan *ReshufflePlan,
	expectedFromNode int64,
) {
	if !ReshuffleRestoreBlockersEnabled(d.db, laneID, groupID) {
		return
	}
	if d.restoreListeners == nil {
		// Defensive: registry should be initialized by NewDispatcher.
		log.Printf("dispatch: restoreListeners registry nil; restore-blockers skipped for order %d", complexParent.ID)
		return
	}

	// Capture blockers' shuffle destinations from the plan's unbury
	// steps so the restock compound can move them back.
	var blockers []restoreBlocker
	for _, s := range plan.Steps {
		if s.StepType != protocol.StepUnbury {
			continue
		}
		blockers = append(blockers, restoreBlocker{
			bin:      &bins.Bin{ID: s.BinID},
			original: s.FromNode,
			shuffle:  s.ToNode,
		})
	}
	if len(blockers) == 0 {
		// No blockers — no restock needed. Default-off means this is
		// the typical case and we don't want to thrash a no-op
		// synthetic parent.
		return
	}

	syn := &orders.Order{
		EdgeUUID:    fmt.Sprintf("restore-%d-%d", complexParent.ID, plan.TargetBin.ID),
		StationID:   complexParent.StationID,
		OrderType:   OrderTypeReshuffleRestore,
		Status:      StatusReshuffling,
		PayloadDesc: fmt.Sprintf("reshuffle restore: %d blockers for parent %d", len(blockers), complexParent.ID),
	}
	if err := d.db.CreateOrder(syn); err != nil {
		log.Printf("dispatch: create synthetic restore parent for complex %d: %v", complexParent.ID, err)
		return
	}
	// CreateOrder writes status='pending' via the existing INSERT —
	// override to Reshuffling so the scanner's ListQueuedOrders
	// never picks it up. Goes through MarkReshuffling (the typed
	// initial-write helper on LifecycleService, mirroring MarkPending)
	// rather than a direct UpdateOrderStatus call — direct status
	// writes are forbidden by the lint guard against state-machine
	// bypass.
	if err := d.lifecycle.MarkReshuffling(syn, "synthetic restore parent"); err != nil {
		log.Printf("dispatch: set synthetic restore parent %d to Reshuffling: %v", syn.ID, err)
	}

	entry := &restoreEntry{
		syntheticParent:  syn,
		complexParentID:  complexParent.ID,
		targetBinID:      plan.TargetBin.ID,
		expectedFromNode: expectedFromNode,
		laneID:           laneID,
		groupID:          groupID,
		targetSlot:       plan.TargetSlot,
		blockers:         blockers,
	}
	if !d.restoreListeners.Register(entry) {
		// Race: a duplicate registration would shadow the prior one.
		// The reshuffle path doesn't register twice for the same
		// complex parent in correct operation; defensive only.
		log.Printf("dispatch: duplicate restore registration for complex parent %d; dropping new entry", complexParent.ID)
		return
	}
	// Persist for crash recovery. The in-memory registry is the live
	// dispatch path; the DB row exists only so a Core restart can
	// re-register the listener instead of dropping it on the floor
	// (which would strand blockers in shuffle slots).
	if err := d.persistPendingRestock(entry); err != nil {
		log.Printf("dispatch: persist pending_restock for complex %d: %v", complexParent.ID, err)
		// Don't bail — the in-memory entry is still valid for the
		// current process lifetime. We just lose crash resilience.
	}
	d.dbg("complex: restore-blockers armed for complex %d (target bin %d, expected from-node %d, %d blockers)",
		complexParent.ID, plan.TargetBin.ID, expectedFromNode, len(blockers))
}

// persistPendingRestock encodes the entry as a pending_restocks row.
func (d *Dispatcher) persistPendingRestock(entry *restoreEntry) error {
	pp := persistedRestockPlan{
		LaneID:  entry.laneID,
		GroupID: entry.groupID,
	}
	if entry.targetSlot != nil {
		pp.TargetSlotID = entry.targetSlot.ID
		pp.TargetSlotName = entry.targetSlot.Name
	}
	for _, b := range entry.blockers {
		pb := persistedBlocker{BinID: b.bin.ID}
		if b.original != nil {
			pb.OriginalNodeID = b.original.ID
			pb.OriginalName = b.original.Name
		}
		if b.shuffle != nil {
			pb.ShuffleNodeID = b.shuffle.ID
			pb.ShuffleName = b.shuffle.Name
		}
		pp.Blockers = append(pp.Blockers, pb)
	}
	planJSON, err := json.Marshal(pp)
	if err != nil {
		return fmt.Errorf("marshal restock plan: %w", err)
	}
	_, err = d.db.InsertPendingRestock(&store.PendingRestock{
		ComplexParentID:    entry.complexParentID,
		SyntheticParentID:  entry.syntheticParent.ID,
		TargetBinID:        entry.targetBinID,
		ExpectedFromNodeID: entry.expectedFromNode,
		RestockPlanJSON:    string(planJSON),
	})
	return err
}

// RecoverPendingRestocks runs at Core boot. Scans the pending_restocks
// table; for each row whose complex parent is still in a non-terminal
// status, re-registers an in-memory listener. Rows whose parent is
// terminal (cancelled, failed, confirmed) resolve the synthetic parent
// via resolveTerminalRestoreParent rather than leaving it stranded at
// `reshuffling` with no listener.
//
// Called once from the engine startup path (after the dispatcher is
// constructed but before event subscriptions start firing).
func (d *Dispatcher) RecoverPendingRestocks() error {
	if d.db == nil || d.restoreListeners == nil {
		return nil
	}
	rows, err := d.db.ListPendingRestocks()
	if err != nil {
		return fmt.Errorf("list pending_restocks: %w", err)
	}
	for _, row := range rows {
		parent, err := d.db.GetOrder(row.ComplexParentID)
		if err != nil || parent == nil {
			log.Printf("dispatch: recover pending_restock %d: complex parent %d missing; deleting row", row.ID, row.ComplexParentID)
			if dErr := d.db.DeletePendingRestockByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete stale pending_restock for complex %d: %v", row.ComplexParentID, dErr)
			}
			continue
		}
		if protocol.IsTerminal(parent.Status) {
			log.Printf("dispatch: recover pending_restock %d: complex parent %d already terminal (%s); resolving synthetic %d",
				row.ID, parent.ID, parent.Status, row.SyntheticParentID)
			d.resolveTerminalRestoreParent(row.SyntheticParentID, row.ComplexParentID)
			continue
		}
		var pp persistedRestockPlan
		if err := json.Unmarshal([]byte(row.RestockPlanJSON), &pp); err != nil {
			log.Printf("dispatch: recover pending_restock %d: malformed JSON for complex %d: %v; deleting row",
				row.ID, parent.ID, err)
			if dErr := d.db.DeletePendingRestockByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete malformed pending_restock for complex %d: %v", row.ComplexParentID, dErr)
			}
			continue
		}
		// Synthetic parent must still exist for the dispatch path.
		syn, err := d.db.GetOrder(row.SyntheticParentID)
		if err != nil || syn == nil {
			log.Printf("dispatch: recover pending_restock %d: synthetic parent %d missing; deleting row",
				row.ID, row.SyntheticParentID)
			if dErr := d.db.DeletePendingRestockByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete orphaned pending_restock for complex %d: %v", row.ComplexParentID, dErr)
			}
			continue
		}
		blockers := make([]restoreBlocker, 0, len(pp.Blockers))
		for _, pb := range pp.Blockers {
			blockers = append(blockers, restoreBlocker{
				bin:      &bins.Bin{ID: pb.BinID},
				original: &nodes.Node{ID: pb.OriginalNodeID, Name: pb.OriginalName},
				shuffle:  &nodes.Node{ID: pb.ShuffleNodeID, Name: pb.ShuffleName},
			})
		}
		var targetSlot *nodes.Node
		if pp.TargetSlotID != 0 {
			targetSlot = &nodes.Node{ID: pp.TargetSlotID, Name: pp.TargetSlotName}
		}
		entry := &restoreEntry{
			syntheticParent:  syn,
			complexParentID:  row.ComplexParentID,
			targetBinID:      row.TargetBinID,
			expectedFromNode: row.ExpectedFromNodeID,
			laneID:           pp.LaneID,
			groupID:          pp.GroupID,
			targetSlot:       targetSlot,
			blockers:         blockers,
		}
		if !d.restoreListeners.Register(entry) {
			// Duplicate in-memory entry (shouldn't happen on a fresh
			// boot but defend against double-recovery calls).
			continue
		}
		log.Printf("dispatch: recovered pending_restock for complex %d (synthetic %d, %d blockers)",
			row.ComplexParentID, syn.ID, len(blockers))
	}
	return nil
}

// HandleBinEnteredTransit is called by engine wiring when an
// EventBinEnteredTransit fires. If the bin matches a pending restore
// listener AND the FromNodeID matches the expected slot, dispatch the
// restock compound. Both checks must pass — a bin moved by an
// unrelated order shouldn't trigger restock.
func (d *Dispatcher) HandleBinEnteredTransit(binID, fromNodeID int64) {
	if d.restoreListeners == nil {
		return
	}
	// Peek first without consuming — if the FromNodeID doesn't match,
	// we'd re-register the entry. Cheaper to do a manual lookup that
	// only consumes on match.
	d.restoreListeners.mu.Lock()
	entry, ok := d.restoreListeners.byBin[binID]
	if !ok {
		d.restoreListeners.mu.Unlock()
		return
	}
	if entry.expectedFromNode != 0 && entry.expectedFromNode != fromNodeID {
		// Bin moved but not from the expected slot — wait for the
		// real pickup. (This happens e.g. when a restock from a
		// prior sibling order moves the bin first.)
		d.restoreListeners.mu.Unlock()
		d.dbg("complex: restore-blockers — bin %d transited from node %d, expecting %d; ignoring",
			binID, fromNodeID, entry.expectedFromNode)
		return
	}
	delete(d.restoreListeners.byBin, binID)
	delete(d.restoreListeners.byComplex, entry.complexParentID)
	d.restoreListeners.mu.Unlock()

	// Dispatch first; only drop the durable recovery row once the restore has
	// actually been created. If dispatch fails, keep the pending_restocks row so
	// a Core restart can recover it (RecoverPendingRestocks re-registers) instead
	// of stranding the displaced bins in shuffle slots with no record.
	if err := d.dispatchRestoreCompound(entry); err != nil {
		log.Printf("dispatch: restore compound for complex %d failed; keeping pending_restock for recovery: %v", entry.complexParentID, err)
		return
	}
	if err := d.db.DeletePendingRestockByComplexParent(entry.complexParentID); err != nil {
		log.Printf("dispatch: delete pending_restock after restore fired for complex %d: %v", entry.complexParentID, err)
	}
}

// HandleComplexParentTerminal is called when a complex parent reaches a
// terminal status (Cancelled, Failed, Skipped, Completed) before the bin-
// transit event arrives — or after, for the Completed path where the listener
// was never consumed (expectedFromNode mismatch). Drops the listener; no
// restock runs.
//
// Delegates to resolveTerminalRestoreParent for the lifecycle decision
// (confirm or cancel the synthetic parent + delete the pending_restocks row).
func (d *Dispatcher) HandleComplexParentTerminal(complexParentID int64) {
	if d.restoreListeners == nil {
		return
	}
	entry := d.restoreListeners.ConsumeByComplexParent(complexParentID)
	if entry == nil {
		// In-memory entry already consumed / never registered. Even
		// so, sweep the DB row defensively — a row could exist from a
		// previous process if RecoverPendingRestocks ran but the
		// in-memory side was cleaned up early (cancel-during-boot).
		if err := d.db.DeletePendingRestockByComplexParent(complexParentID); err != nil {
			log.Printf("dispatch: delete pending_restock on parent terminal (no-entry) for complex %d: %v", complexParentID, err)
		}
		return
	}

	d.resolveTerminalRestoreParent(entry.syntheticParent.ID, complexParentID)
}

// resolveTerminalRestoreParent resolves a synthetic reshuffle_restore parent
// when its complex parent is terminal — confirm (parent completed) or cancel
// (parent failed/cancelled). The persisted pending_restocks row is deleted on
// successful resolution so boot recovery won't re-register a stale listener.
//
// Called from three paths:
//   - HandleComplexParentTerminal (live event handler, has an in-memory entry)
//   - RecoverPendingRestocks (boot recovery, no in-memory entry)
//   - ResolveOrphanedRestoreSynthetic (periodic sweep, defense-in-depth)
//
// The synthetic parent ID is enough — the complex parent ID is only needed
// for the pending_restocks delete (keyed on complex_parent_id).
func (d *Dispatcher) resolveTerminalRestoreParent(syntheticParentID, complexParentID int64) {
	syn, err := d.db.GetOrder(syntheticParentID)
	switch {
	case errors.Is(err, sql.ErrNoRows) || (err == nil && syn == nil):
		// The synthetic genuinely does not exist — there is nothing to resolve, and
		// leaving the row would make boot recovery re-register a listener for an
		// order that is gone. Safe to drop.
		log.Printf("dispatch: resolve restore parent %d: synthetic %d does not exist; deleting pending_restock for complex %d",
			syntheticParentID, syntheticParentID, complexParentID)
		if dErr := d.db.DeletePendingRestockByComplexParent(complexParentID); dErr != nil {
			log.Printf("dispatch: delete pending_restock on synthetic-gone for complex %d: %v", complexParentID, dErr)
		}
		return
	case err != nil:
		// The lookup FAILED. That is not evidence the synthetic is gone, and
		// pending_restocks is the only durable record of this restock — deleting it
		// on a transient DB error destroys the recovery path (R04-3). Keep the row
		// and let boot recovery or the periodic sweep retry.
		log.Printf("dispatch: resolve restore parent %d: synthetic lookup failed (%v); keeping pending_restock for complex %d",
			syntheticParentID, err, complexParentID)
		return
	}

	parent, pErr := d.db.GetOrder(complexParentID)
	if pErr == nil && parent != nil && parent.Status == protocol.StatusConfirmed {
		// Parent completed — work happened, listener just never matched.
		_ = d.lifecycle.CompleteCompound(syn)
		updated, err := d.db.GetOrder(syn.ID)
		if err != nil {
			log.Printf("dispatch: re-check synthetic parent %d after confirm for complex %d: %v", syn.ID, complexParentID, err)
			return
		}
		if !protocol.IsTerminal(updated.Status) {
			log.Printf("dispatch: synthetic parent %d not terminal after confirm (status %s) for complex %d; keeping pending_restock for recovery", syn.ID, updated.Status, complexParentID)
			return
		}
		if err := d.db.DeletePendingRestockByComplexParent(complexParentID); err != nil {
			log.Printf("dispatch: delete pending_restock on parent confirmed for complex %d: %v", complexParentID, err)
		}
		d.dbg("complex: restore-blockers deregistered for complex %d (parent confirmed, synthetic confirmed)", complexParentID)
		return
	}

	// Parent cancelled / failed / skipped / unresolvable — didn't pick up.
	// CancelOrder is best-effort (it only logs on a failed transition), so
	// re-check the persisted status rather than assume success — only drop the
	// durable recovery row once the synthetic parent is actually terminal;
	// otherwise keep it so the next pass can retry.
	d.lifecycle.CancelOrder(syn, syn.StationID, "complex parent terminated before pickup")
	updated, err := d.db.GetOrder(syn.ID)
	if err != nil {
		log.Printf("dispatch: re-check synthetic parent %d after cancel for complex %d: %v", syn.ID, complexParentID, err)
		return
	}
	if !protocol.IsTerminal(updated.Status) {
		log.Printf("dispatch: synthetic parent %d not terminal after cancel (status %s) for complex %d; keeping pending_restock for recovery", syn.ID, updated.Status, complexParentID)
		return
	}
	if err := d.db.DeletePendingRestockByComplexParent(complexParentID); err != nil {
		log.Printf("dispatch: delete pending_restock on parent terminal for complex %d: %v", complexParentID, err)
	}
	d.dbg("complex: restore-blockers deregistered for complex %d (parent terminal)", complexParentID)
}

// ResolveOrphanedRestoreSynthetic resolves a stranded reshuffle_restore
// synthetic parent by looking up its associated complex parent and applying
// the terminal resolution. Called from the ReconciliationService periodic
// sweep as defense-in-depth for the gap between "delete pending_restocks row"
// and "resolve synthetic parent." The complex parent ID is parsed from
// edge_uuid, which has format "restore-<complexParentID>-<binID>" (set in
// scheduleRestoreIfEnabled). If the complex parent is missing or still
// non-terminal, returns ErrRestoreParentNotResolvable so the sweep can
// distinguish "skip, retry later" from "resolved."
//
// The edge_uuid format dependency is intentional — the sweep and the format
// string live in the same package. See scheduleRestoreIfEnabled.
func (d *Dispatcher) ResolveOrphanedRestoreSynthetic(syntheticParentID int64, edgeUUID string) error {
	// Parse complex parent ID from edge_uuid: "restore-%d-%d"
	var complexParentID int64
	if _, err := fmt.Sscanf(edgeUUID, "restore-%d-", &complexParentID); err != nil || complexParentID == 0 {
		return fmt.Errorf("%w: cannot parse complex parent from edge_uuid %q: %v",
			ErrRestoreParentNotResolvable, edgeUUID, err)
	}

	parent, err := d.db.GetOrder(complexParentID)
	if err != nil {
		return fmt.Errorf("lookup complex parent %d: %w", complexParentID, err)
	}
	if parent == nil || !protocol.IsTerminal(parent.Status) {
		// Parent gone or still live — can't resolve yet. Not an error,
		// just not actionable this pass.
		return fmt.Errorf("%w: complex parent %d not terminal (status=%s)",
			ErrRestoreParentNotResolvable, complexParentID, parent.Status)
	}

	d.resolveTerminalRestoreParent(syntheticParentID, complexParentID)
	return nil
}

// ErrRestoreParentNotResolvable is returned by ResolveOrphanedRestoreSynthetic
// when the complex parent is missing or non-terminal — the sweep should skip
// and retry next pass rather than treat it as a permanent failure.
var ErrRestoreParentNotResolvable = fmt.Errorf("restore parent not resolvable")

// dispatchRestoreCompound builds a deepest-first restock plan and
// dispatches it as children of the already-created synthetic parent.
// Called from HandleBinEnteredTransit when both bin ID and FromNodeID
// match the pending entry.
//
// Restocks blockers deepest-first via slot rotation (restockDestinations,
// same algorithm the simple-retrieve PlanReshuffle uses) so the lane ends
// with depths 2..N filled and the mouth (depth 1) empty — bubble-free.
func (d *Dispatcher) dispatchRestoreCompound(entry *restoreEntry) error {
	plan := &ReshufflePlan{
		TargetBin:  &bins.Bin{ID: entry.targetBinID},
		TargetSlot: entry.targetSlot,
		Lane:       nil,
	}

	// Convert restoreBlockers to reshuffleBlockers so restockDestinations
	// can compute the packed (deepest-first, bubble-free) destination list.
	// Blockers are in shallowest-first order from the unbury plan.
	rb := make([]reshuffleBlocker, len(entry.blockers))
	for i, b := range entry.blockers {
		rb[i] = reshuffleBlocker{
			bin:  b.bin,
			slot: b.original, // original lane-slot position
		}
	}
	// Get packed destinations: deepest blocker → target's old slot (depth N),
	// each shallower blocker → next-deeper blocker's original slot (depth 2..N-1),
	// mouth (depth 1) stays empty. Matches PlanReshuffle's bubble-free restock.
	//
	// restockDestinations puts targetSlot at dests[n-1], so a nil targetSlot would
	// dispatch the deepest restock leg with a nil destination. That happens for
	// pending_restocks rows persisted before TargetSlotID existed, which boot
	// recovery re-registers with targetSlot=nil. Fall back to each blocker's own
	// original slot: it leaves the mouth bubble the packing avoids, but it is a
	// valid lane state and every leg has a real destination.
	var dests []*nodes.Node
	if entry.targetSlot != nil {
		dests = restockDestinations(rb, entry.targetSlot)
	} else {
		log.Printf("dispatch: restore compound for complex %d has no target slot (pre-TargetSlotID pending_restock); restocking to original slots", entry.complexParentID)
		dests = make([]*nodes.Node, len(rb))
		for i := range rb {
			dests[i] = rb[i].slot
		}
	}

	// Execute deepest-first (reverse order).
	seq := 1
	for i := len(entry.blockers) - 1; i >= 0; i-- {
		b := entry.blockers[i]
		if dests[i] == nil {
			// No recoverable destination for this blocker — dispatching a leg with a
			// nil ToNode would panic downstream. Skip it and leave the bin parked at
			// its shuffle node for the operator rather than emit a malformed order.
			log.Printf("dispatch: restore compound for complex %d: blocker bin %d has no restock destination; skipping leg", entry.complexParentID, b.bin.ID)
			continue
		}
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: protocol.StepRestock,
			BinID:    b.bin.ID,
			FromNode: b.shuffle,
			ToNode:   dests[i],
		})
		seq++
	}
	if len(plan.Steps) == 0 {
		// Nothing to restock — mark the synthetic parent confirmed.
		_ = d.lifecycle.CompleteCompound(entry.syntheticParent)
		return nil
	}
	// CreateCompoundChildrenOnly (not CreateCompoundOrder) — the
	// synthetic parent is already at StatusReshuffling from the
	// scheduling path; BeginReshuffle would log a spurious illegal
	// transition warning.
	if err := d.CreateCompoundChildrenOnly(entry.syntheticParent, plan); err != nil {
		log.Printf("dispatch: create restore compound for complex %d: %v",
			entry.complexParentID, err)
		return err
	}
	d.dbg("complex: restore-blockers fired — %d restock steps for complex %d",
		len(plan.Steps), entry.complexParentID)
	return nil
}
