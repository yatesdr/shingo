package dispatch

import (
	"encoding/json"
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
	LaneID   int64              `json:"lane_id"`
	GroupID  int64              `json:"group_id"`
	Blockers []persistedBlocker `json:"blockers"`
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
	expectedFromNode int64 // node ID we expect to see the bin LEAVING
	laneID           int64 // for unlocking on listener fire
	groupID          int64 // for the restock plan
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
// terminal (cancelled, failed, confirmed) are deleted — those
// listeners would never fire.
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
			log.Printf("dispatch: recover pending_restock %d: complex parent %d already terminal (%s); deleting row",
				row.ID, parent.ID, parent.Status)
			if dErr := d.db.DeletePendingRestockByComplexParent(row.ComplexParentID); dErr != nil {
				log.Printf("dispatch: delete stale pending_restock for complex %d: %v", row.ComplexParentID, dErr)
			}
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
		entry := &restoreEntry{
			syntheticParent:  syn,
			complexParentID:  row.ComplexParentID,
			targetBinID:      row.TargetBinID,
			expectedFromNode: row.ExpectedFromNodeID,
			laneID:           pp.LaneID,
			groupID:          pp.GroupID,
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

// HandleComplexParentTerminal is called when a complex parent reaches
// Cancelled or Failed before the bin-transit event arrives. Drops
// the listener; no restock runs (parent didn't pick up, so leaving
// the lane in its post-unbury state is correct). Also deletes the
// persisted pending_restocks row so Core boot recovery doesn't
// re-register a stale listener.
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
	// Cancel the synthetic parent so its row doesn't sit at Reshuffling forever.
	// CancelOrder is best-effort (it only logs on a failed transition), so
	// re-check the persisted status rather than assume success — only drop the
	// durable recovery row once the synthetic parent is actually terminal;
	// otherwise keep it so boot recovery can resolve a still-stuck parent.
	syn := entry.syntheticParent
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

// dispatchRestoreCompound builds a deepest-first restock plan and
// dispatches it as children of the already-created synthetic parent.
// Called from HandleBinEnteredTransit when both bin ID and FromNodeID
// match the pending entry.
func (d *Dispatcher) dispatchRestoreCompound(entry *restoreEntry) error {
	// Deepest-first: reverse the blockers (which were captured in
	// shallowest-first order from the unbury plan). The restore
	// compound has no single "target bin" — it's an N-bin restock —
	// so TargetBin is set to the parent's target (the bin whose
	// pickup triggered this) for traceability only. CreateCompoundOrder
	// reads it only for the BeginReshuffle log line; the children's
	// BinIDs come from the steps themselves.
	plan := &ReshufflePlan{
		TargetBin:  &bins.Bin{ID: entry.targetBinID},
		TargetSlot: nil,
		Lane:       nil,
	}
	seq := 1
	// TODO(reshuffle-refactor): restock packs to each blocker's ORIGINAL slot here, which
	// leaves bubbles (the target's freed slot stays empty; blockers' own slots fill at their
	// original depths). PlanReshuffle above was fixed to pack deepest-first via slot rotation
	// (see restockDestinations in reshuffle.go). This restore path needs the same treatment
	// but requires the target's slot, which isn't captured in restoreEntry today (and the
	// persisted persistedRestockPlan would need a TargetSlotName field for crash recovery).
	// Deferred to the reshuffle refactor — expose-mode restore is off by default
	// (restore_blockers toggle), so the demo plant doesn't exercise this path.
	for i := len(entry.blockers) - 1; i >= 0; i-- {
		b := entry.blockers[i]
		plan.Steps = append(plan.Steps, ReshuffleStep{
			Sequence: seq,
			StepType: protocol.StepRestock,
			BinID:    b.bin.ID,
			FromNode: b.shuffle,
			ToNode:   b.original,
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
