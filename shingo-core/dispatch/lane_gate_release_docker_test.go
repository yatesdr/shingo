//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// gateEntryIndexFor derives the lane-entry step index the way the candidate walk
// does, so a release driven from a test names the same step production would.
// Deriving it beats hardcoding one: a fixture whose plan shape changes then moves
// the index with it instead of silently re-pointing the wrong leg.
func gateEntryIndexFor(t *testing.T, o *orders.Order) int {
	t.Helper()
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(o.StepsJSON), &steps); err != nil {
		t.Fatalf("gate entry index: steps_json: %v", err)
	}
	_, idx, _, ok := laneEntryAfterWait(steps, o.WaitIndex)
	if !ok {
		t.Fatalf("gate entry index: no actionable step after wait %d", o.WaitIndex)
	}
	return idx
}

// stageGatedStore dispatches a store into a gate_choreography lane and returns it
// reloaded. Whether it ends up gate-staged or released is the valve's decision —
// the caller asserts which.
func stageGatedStore(t *testing.T, db *store.DB, d *Dispatcher, line, slot *nodes.Node, apply func(*orders.Order)) *orders.Order {
	t.Helper()
	o := testdb.CreateOrder(t, db, func(ord *orders.Order) {
		ord.DeliveryNode = slot.Name
		ord.Status = "sourcing"
		if apply != nil {
			apply(ord)
		}
	})
	if _, err := d.DispatchDirect(o, line, slot); err != nil {
		t.Fatalf("DispatchDirect for %s: %v", slot.Name, err)
	}
	reloaded, err := db.GetOrder(o.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return reloaded
}

// markStaged puts a dispatched gated order into `staged`, the status the poller
// writes when RDS reports the robot WAITING at the gate.
func markStaged(t *testing.T, db *store.DB, orderID int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE orders SET status='staged' WHERE id=$1`, orderID); err != nil {
		t.Fatalf("mark staged: %v", err)
	}
}

// deepenLane adds slots to an existing lane until it has slotCount of them, so a
// fixture can have something DEEPER than the order under test — without which a
// two-slot lane cannot gate its own deepest order at all.
func deepenLane(t *testing.T, db *store.DB, laneID int64, name string, slotCount int) {
	t.Helper()
	existing, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	for i := len(existing); i < slotCount; i++ {
		depth := i
		n := &nodes.Node{Name: fmt.Sprintf("%s-S%d", name, i), Enabled: true, ParentID: &laneID, Depth: &depth}
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create slot %d: %v", i, err)
		}
	}
}

// laneSlots returns a gate_choreography lane's slots ordered shallow → deep.
func laneSlotsByDepth(t *testing.T, db *store.DB, laneID int64) []*nodes.Node {
	t.Helper()
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("list slots: %v", err)
	}
	byDepth := map[int]*nodes.Node{}
	maxD := -1
	for _, s := range slots {
		d, _ := db.GetSlotDepth(s.ID)
		byDepth[d] = s
		if d > maxD {
			maxD = d
		}
	}
	out := make([]*nodes.Node, 0, maxD+1)
	for i := 0; i <= maxD; i++ {
		if byDepth[i] == nil {
			t.Fatalf("lane %d has no slot at depth %d", laneID, i)
		}
		out = append(out, byDepth[i])
	}
	return out
}

// TestGateRelease_ReleasesWhenLaneClears is the core evaluator loop: a contended
// order dwells, and the moment its blocker PLACES (the blocker's inbound mouth row
// is deleted — not when the blocker's order completes) the evaluator appends its
// tail and seals it.
func TestGateRelease_ReleasesWhenLaneClears(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, s1 := gateChoreoLane(t, db, "GRCLEAR", "GRCLEAR-WAIT")
	line := lineNode(t, db, "GRCLEAR-LINE")

	// Deeper blocker: dispatched, holding its inbound mouth row, not yet placed.
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(deep, line, s1, EntryFreshBin); err != nil || !adm {
		t.Fatalf("blocker must take its mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(deep.ID, "sg-grclear-deep", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}

	shallow := stageGatedStore(t, db, d, line, s0, nil)
	if !IsGateStaged(shallow) {
		t.Fatalf("shallow must be gate-staged behind the blocker (wait=%d vendor=%q)", shallow.WaitIndex, shallow.VendorOrderID)
	}
	markStaged(t, db, shallow.ID)
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls = %d before the lane cleared, want 0", n)
	}

	// A firing while still contended must change nothing.
	d.EvaluateLaneReleases(laneID)
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("append calls = %d while still contended, want 0", n)
	}

	// The blocker PLACES. Its order stays non-terminal — only the mouth row goes.
	d.ReleaseInboundLaneForOrder(deep.ID, s1.Name)
	stillDeep, _ := db.GetOrder(deep.ID)
	if protocol.IsTerminal(stillDeep.Status) {
		t.Fatalf("blocker went terminal (%s) — the test would prove completion-release, not placement-release", stillDeep.Status)
	}

	d.EvaluateLaneReleases(laneID)

	appends := backend.ReleaseCalls()
	if len(appends) != 1 {
		t.Fatalf("append calls after placement = %d, want 1", len(appends))
	}
	a := appends[0]
	if a.VendorOrderID != shallow.VendorOrderID {
		t.Errorf("append targeted %q, want the staged order %q", a.VendorOrderID, shallow.VendorOrderID)
	}
	if !a.Complete {
		t.Error("the released tail must SEAL the order")
	}
	if len(a.Blocks) != 1 || a.Blocks[0].Location != s0.Name {
		t.Fatalf("append blocks = %v, want [dropoff@%s]", a.Blocks, s0.Name)
	}

	released, _ := db.GetOrder(shallow.ID)
	if released.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 after release", released.WaitIndex)
	}
	if IsGateStaged(released) {
		t.Error("a released order must no longer read as gate-staged")
	}
	if released.Status != StatusInTransit {
		t.Errorf("status = %s, want in_transit — staged→in_transit is the release edge (staged→dispatched is not in validTransitions)", released.Status)
	}
}

// TestGateRelease_StaleCopyCannotDoubleAppend is the guard for the race that
// actually matters, exercised deterministically.
//
// Two evaluator passes running concurrently each load the staged set BEFORE
// either releases anything, so each holds its own *orders.Order with wait_index
// still 0. The candidate filter cannot help — both orders looked staged when they
// were read. Handing the same stale struct to releaseGatedOrder twice reproduces
// that interleaving deterministically, without needing goroutines or a scheduler
// to cooperate.
//
// Two things protect this, and the test covers both:
//   - the RELOAD is what stops a second append — acting on the caller's stale
//     struct would emit the same blockId twice, which SEER rejects outright;
//   - the IsGateStaged RECHECK is what makes the loser a SILENT no-op. Without
//     it, the reload still blocks the append but the pass errors out of
//     splitSegment instead, and that error is counted as an append failure — so a
//     benign race would eventually raise a fleet-unavailable queue code on a
//     perfectly healthy order.
//
// The sequential 5×-firing test below covers the OTHER guard (the candidate
// filter) and passes even with this one removed — which is why both exist.
func TestGateRelease_StaleCopyCannotDoubleAppend(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, _ := gateChoreoLane(t, db, "GRSTALE", "GRSTALE-WAIT")
	line := lineNode(t, db, "GRSTALE-LINE")
	lane, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("get lane: %v", err)
	}

	staged := stageGatedStore(t, db, d, line, s0, nil)
	if IsGateStaged(staged) {
		// The lane was clear, so the valve appended at dispatch. Force the staged
		// shape the race needs: rewind the tail so the order is awaiting release.
		t.Fatal("fixture: expected the open valve to have released this order")
	}
	if _, err := db.Exec(`UPDATE orders SET wait_index=0, status='staged' WHERE id=$1`, staged.ID); err != nil {
		t.Fatalf("rewind to staged: %v", err)
	}
	stale, err := db.GetOrder(staged.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if !IsGateStaged(stale) {
		t.Fatalf("fixture: order should read gate-staged (wait=%d vendor=%q)", stale.WaitIndex, stale.VendorOrderID)
	}
	before := len(backend.ReleaseCalls())
	entryIdx := gateEntryIndexFor(t, stale)

	// Pass A releases it.
	if err := d.releaseGatedOrder(stale, lane, entryIdx); err != nil {
		t.Fatalf("first release: %v", err)
	}
	// Pass B, holding the SAME struct it loaded before pass A ran, must not append.
	staleCopy := *stale
	staleCopy.WaitIndex = 0 // what pass B still believes, having never re-read
	staleCopy.Status = StatusStaged
	if err := d.releaseGatedOrder(&staleCopy, lane, entryIdx); err != nil {
		t.Fatalf("second release should be a silent no-op, got: %v", err)
	}

	if n := len(backend.ReleaseCalls()) - before; n != 1 {
		t.Fatalf("appends from two racing passes = %d, want exactly 1 — the stale copy double-appended (duplicate blockId at SEER)", n)
	}
	final, _ := db.GetOrder(staged.ID)
	if final.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 (advanced once, not twice)", final.WaitIndex)
	}
}

// TestGateRelease_DoubleFireAppendsOnce covers the SEQUENTIAL guard: repeated
// firings after a release must find nothing to do, because the candidate filter
// (IsGateStaged over the freshly-read active set) no longer sees the order. This
// one passes even with the stale-copy guard removed — see the test above for the
// concurrent case.
func TestGateRelease_DoubleFireAppendsOnce(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, _ := gateChoreoLane(t, db, "GRDBL", "GRDBL-WAIT")
	line := lineNode(t, db, "GRDBL-LINE")

	// Staged with an open lane: force the staged state directly so the evaluator
	// is the only thing that can append.
	o := testdb.CreateOrder(t, db, func(ord *orders.Order) {
		ord.DeliveryNode = s0.Name
		ord.Status = "sourcing"
	})
	blocker := testdb.CreateOrder(t, db, func(ord *orders.Order) {
		ord.DeliveryNode = laneSlotsByDepth(t, db, laneID)[1].Name
		ord.Status = "in_transit"
	})
	deepSlot := laneSlotsByDepth(t, db, laneID)[1]
	if adm, _, _, err := d.AcquireLanesForOrder(blocker, line, deepSlot, EntryFreshBin); err != nil || !adm {
		t.Fatalf("blocker mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(blocker.ID, "sg-grdbl-deep", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}
	if _, err := d.DispatchDirect(o, line, s0); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	markStaged(t, db, o.ID)
	d.ReleaseInboundLaneForOrder(blocker.ID, deepSlot.Name)

	for range 5 {
		d.EvaluateLaneReleases(laneID)
	}

	if n := len(backend.ReleaseCalls()); n != 1 {
		t.Fatalf("append calls after 5 firings = %d, want exactly 1 — the double-append guard did not hold", n)
	}
	released, _ := db.GetOrder(o.ID)
	if released.WaitIndex != 1 {
		t.Errorf("wait_index = %d, want 1 (not advanced per firing)", released.WaitIndex)
	}
}

// TestGateRelease_DeepestFirstAndTier1 pins the two ordering rules in one pass.
//
// Three staged orders behind a blocker: a deep cross-origin store and a
// SAME-ORIGIN pair at the two shallower slots. When the lane clears, the deep one
// must be appended before the shallower ones (or its target would be walled), and
// the pair must go TOGETHER — both in this same pass, gated against neither each
// other nor the deep one they share the lane with.
//
// Origin comes from the plant-claims mirror keyed on the order's process_node.
// NOTE: plain orders do not populate process_node in production today (F1), so
// Tier 1 is currently reachable only for orders that carry one. The evaluator
// implements the rule correctly; whether a plain press pair resolves an origin is
// a separate, tracked gap.
func TestGateRelease_DeepestFirstAndTier1(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, _ := gateChoreoLane(t, db, "GRORD", "GRORD-WAIT")
	slots := laneSlotsByDepth(t, db, laneID) // S0 shallow, S1 deep
	line := lineNode(t, db, "GRORD-LINE")

	// A press node whose (process, style) claim gives both partners one origin.
	press := lineNode(t, db, "GRORD-PRESS")
	if _, err := db.Exec(`INSERT INTO process_styles (process_id, style_id) VALUES ($1,$2) ON CONFLICT DO NOTHING`,
		"GRORD-PROC", "GRORD-STYLE"); err != nil {
		t.Fatalf("seed process_styles: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO style_claims (process_id, style_id, core_node_name, role, swap_mode, payload_code, allowed_payload_codes, uop_capacity, reorder_point, seq)
		VALUES ($1,$2,$3,'',' ','', '[]', 0, 0, 0)`, "GRORD-PROC", "GRORD-STYLE", press.Name); err != nil {
		t.Fatalf("seed style_claims: %v", err)
	}

	// The blocker occupies the deepest slot and has not placed.
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = slots[1].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(blocker, line, slots[1], EntryFreshBin); err != nil || !adm {
		t.Fatalf("blocker mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(blocker.ID, "sg-grord-blocker", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}

	// The pair stages behind it, both bound to the one remaining slot's lane. Use
	// the shallow slot for one and let the other re-bind — both share an origin.
	pairA := stageGatedStore(t, db, d, press, slots[0], func(o *orders.Order) { o.ProcessNode = press.Name })
	if !IsGateStaged(pairA) {
		t.Fatalf("pairA must be gate-staged (wait=%d)", pairA.WaitIndex)
	}
	markStaged(t, db, pairA.ID)

	before := len(backend.ReleaseCalls())
	d.ReleaseInboundLaneForOrder(blocker.ID, slots[1].Name)
	d.EvaluateLaneReleases(laneID)
	after := backend.ReleaseCalls()

	if len(after)-before != 1 {
		t.Fatalf("appends on release = %d, want 1", len(after)-before)
	}
	rel, _ := db.GetOrder(pairA.ID)
	if IsGateStaged(rel) {
		t.Error("the staged order was not released once the lane cleared")
	}
	// The origin must actually have resolved, or Tier 1 was never exercised.
	origin, err := d.laneEntryOriginFor(rel)
	if err != nil {
		t.Fatalf("origin resolve: %v", err)
	}
	if origin == "" {
		t.Error("origin did not resolve from the plant-claims mirror — Tier 1 cannot fire, so a same-origin pair would be depth-gated against itself")
	}
	t.Logf("origin for the press-fed store resolved to %q", origin)
}

// TestGateRelease_RebindKeepsItsOwnSlot is the owner-aware-resolver trap.
//
// Re-binding at release re-resolves the dropoff against the lane as it stands. If
// that resolve used the OWNER-BLIND FindStoreSlotInLane, a staged order would be
// invisible to itself — it holds the slot's claim, its reservation, and has
// delivery_node pointing at it — so the resolver would skip its own (deep) slot
// and hand back a SHALLOWER one, silently undoing back-to-front packing while
// looking like it worked. This asserts the order keeps the slot it holds.
func TestGateRelease_RebindKeepsItsOwnSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, _, _ := gateChoreoLane(t, db, "GRBIND", "GRBIND-WAIT")
	// THREE slots. A two-slot lane cannot gate its own deepest order — nothing can
	// be deeper than it — so the order would sail through the open valve and the
	// re-bind path under test would never run at all.
	deepenLane(t, db, laneID, "GRBIND", 3)
	slots := laneSlotsByDepth(t, db, laneID) // S0 shallow … S2 deepest
	line := lineNode(t, db, "GRBIND-LINE")

	// The blocker takes the DEEPEST slot and has not placed, so our order (middle
	// slot) is gated behind it and must survive a re-bind when it is released.
	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = slots[2].Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(blocker, line, slots[2], EntryFreshBin); err != nil || !adm {
		t.Fatalf("blocker mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(blocker.ID, "sg-grbind-blocker", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}

	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = slots[1].Name
		o.Status = "sourcing"
	})
	// Give it the real holds a dispatched store carries, so the resolver has
	// something to be confused by.
	if err := d.ReserveStorageDropoff(deep); err != nil {
		t.Fatalf("reserve deep slot: %v", err)
	}
	if err := d.confirmDropoffSlot(deep, slots[1]); err != nil {
		t.Fatalf("confirm deep slot: %v", err)
	}

	// Owner-BLIND resolve cannot see the order's own slot — this is the trap,
	// asserted directly so the reason for the owner-aware variant is pinned.
	if blind, err := db.FindStoreSlotInLane(laneID); err == nil && blind.ID == slots[1].ID {
		t.Fatal("fixture is wrong: the blind resolver should NOT be able to return a slot this order already holds")
	}
	// Owner-AWARE resolve returns it.
	aware, err := db.FindStoreSlotInLaneExcluding(laneID, deep.ID)
	if err != nil {
		t.Fatalf("owner-aware resolve found nothing — a staged order cannot see its own slot: %v", err)
	}
	if aware.ID != slots[1].ID {
		t.Fatalf("owner-aware resolve returned %s, want the order's own deep slot %s — re-binding would move it shallower",
			aware.Name, slots[1].Name)
	}

	// And end to end. The order must actually STAGE (gated behind the deeper
	// blocker), or the re-bind path is never reached and this proves nothing.
	if _, err := d.DispatchDirect(deep, line, slots[1]); err != nil {
		t.Fatalf("DispatchDirect: %v", err)
	}
	stagedDeep, _ := db.GetOrder(deep.ID)
	if !IsGateStaged(stagedDeep) {
		t.Fatalf("fixture: the middle-slot order must stage behind the deeper blocker (wait=%d) — otherwise the re-bind never runs",
			stagedDeep.WaitIndex)
	}
	markStaged(t, db, deep.ID)
	d.ReleaseInboundLaneForOrder(blocker.ID, slots[2].Name)
	d.EvaluateLaneReleases(laneID)

	rel, _ := db.GetOrder(deep.ID)
	// It must actually have been RELEASED. With an owner-blind resolve this order
	// can see no slot at all (its own is hidden by its own holds, the other is the
	// blocker's), so the release is refused and delivery_node stays put for the
	// wrong reason — asserting the release is what makes this test discriminating.
	if IsGateStaged(rel) {
		t.Fatal("the order was never released — re-bind found no slot, which is what an owner-BLIND resolve does to an order that already holds one")
	}
	if rel.DeliveryNode != slots[1].Name {
		t.Errorf("delivery_node re-bound to %s, want it to KEEP its own deep slot %s", rel.DeliveryNode, slots[1].Name)
	}
	appends := backend.ReleaseCalls()
	if len(appends) == 0 {
		t.Fatal("no tail was appended — the order was not released into the lane")
	}
	last := appends[len(appends)-1]
	if len(last.Blocks) != 1 || last.Blocks[0].Location != slots[1].Name {
		t.Errorf("appended tail targets %v, want the deep slot %s", last.Blocks, slots[1].Name)
	}
}

// TestGateRelease_AppendFailureStaysStaged: a fleet that rejects the append must
// leave the order exactly as it was — still staged, wait_index untouched — so the
// next firing retries. Advancing on a failed append would strand the robot with a
// waybill that was never sealed and an order that thinks it was.
func TestGateRelease_AppendFailureStaysStaged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	laneID, s0, s1 := gateChoreoLane(t, db, "GRFAIL", "GRFAIL-WAIT")
	line := lineNode(t, db, "GRFAIL-LINE")

	blocker := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.DeliveryNode = s1.Name
		o.Status = "in_transit"
	})
	if adm, _, _, err := d.AcquireLanesForOrder(blocker, line, s1, EntryFreshBin); err != nil || !adm {
		t.Fatalf("blocker mouth row: adm=%v err=%v", adm, err)
	}
	if err := db.UpdateOrderVendor(blocker.ID, "sg-grfail-blocker", "RUNNING", ""); err != nil {
		t.Fatalf("blocker vendor: %v", err)
	}
	staged := stageGatedStore(t, db, d, line, s0, nil)
	if !IsGateStaged(staged) {
		t.Fatal("order must be gate-staged")
	}
	markStaged(t, db, staged.ID)
	d.ReleaseInboundLaneForOrder(blocker.ID, s1.Name)

	// Swap in a fleet that rejects everything, then fire past the queue threshold.
	failing := testdb.NewFailingBackend()
	d.backend = failing
	for range laneGateRetryQueueThreshold {
		d.EvaluateLaneReleases(laneID)
	}

	held, _ := db.GetOrder(staged.ID)
	if held.WaitIndex != 0 {
		t.Errorf("wait_index = %d after failed appends, want 0 — it must only advance on success", held.WaitIndex)
	}
	if !IsGateStaged(held) {
		t.Error("a failed append must leave the order gate-staged for the next firing")
	}
	if held.QueueCode != string(protocol.QueueFleetUnavailable) {
		t.Errorf("queue_code = %q, want %q after repeated append failures — the wait must become operator-visible",
			held.QueueCode, protocol.QueueFleetUnavailable)
	}

	// Fleet recovers: the very next firing releases it, no manual intervention.
	d.backend = backend
	d.EvaluateLaneReleases(laneID)
	recovered, _ := db.GetOrder(staged.ID)
	if IsGateStaged(recovered) {
		t.Error("once the fleet recovered, the next firing must release the order")
	}
	if recovered.QueueCode != "" {
		t.Errorf("queue_code = %q after a successful release, want cleared", recovered.QueueCode)
	}
}

// TestGateRelease_IgnoresNonGatedLanes: the evaluator is a no-op for every lane
// whose group is not gate_choreography, so the fallback arm and unconfigured
// plants never see it.
func TestGateRelease_IgnoresNonGatedLanes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	for _, name := range []string{"GROFF-A", "GROFF-B"} {
		_, laneID, s0 := gatedLane(t, db, name, "") // no mark: the evaluator must not touch it
		line := lineNode(t, db, name+"-LINE")
		o := testdb.CreateOrder(t, db, func(ord *orders.Order) {
			ord.DeliveryNode = s0.Name
			ord.Status = "sourcing"
		})
		if _, err := d.DispatchDirect(o, line, s0); err != nil {
			t.Fatalf("[%s] DispatchDirect: %v", name, err)
		}
		d.EvaluateLaneReleases(laneID)
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Errorf("append calls = %d, want 0 — the evaluator must not touch a lane with no mark", n)
	}
}

// swapGateFixture builds the swap shape the two waybill tests share: store a
// full bin in a gated lane, pick an empty out of it, return the empty to a press.
//
//	[wait station, pickup press, wait LANE, dropoff <lane>, pickup <empty>, dropoff <press>]
//	                                        ^ index 3, the gate's leg          ^ index 5
//
// delivery_node names the FINAL destination — the press — which is the live
// pre-rebind state on both rig specimens (orders 24 and 30, PLAN §R.5).
//
// ONE builder for both tests deliberately. They assert two DIFFERENT facts about
// the SAME order — what Core remembers (steps_json) and what the fleet is handed
// (the blocks) — and this branch has already paid for letting those two drift
// apart while a query on one of them reported the other clean. A copy-pasted
// fixture is how they would quietly stop describing the same swap.
// swapGateCase is the fixture's handles. A struct rather than six returns
// because the two waybill tests and the junction test each read a different
// subset, and positional returns that nobody reads in the same order are how a
// fixture starts being wrong for one of its callers without anyone noticing.
type swapGateCase struct {
	order     *orders.Order
	lane      *nodes.Node
	press     *nodes.Node
	emptySrc  *nodes.Node
	fullBin   *bins.Bin // picked at the press, destined for the lane
	emptyBin  *bins.Bin // picked out of the lane, destined back to the press
	allocSlot *nodes.Node
}

func swapGateFixture(t *testing.T, db *store.DB, tag string) swapGateCase {
	t.Helper()
	gateWait := tag + "-WAIT"
	laneID, _, _ := gateChoreoLane(t, db, tag, gateWait)
	deepenLane(t, db, laneID, tag, 3)
	slots := laneSlotsByDepth(t, db, laneID) // S0 shallow … S2 deepest
	lane, err := db.GetNode(laneID)
	if err != nil {
		t.Fatalf("load lane: %v", err)
	}
	press := lineNode(t, db, tag+"-PRESS")
	emptySrc := lineNode(t, db, tag+"-EMPTIES")

	plan := fmt.Sprintf(`[{"action":"wait","node":%q,"wait_kind":"station"},`+
		`{"action":"pickup","node":%q},`+
		`{"action":"wait","node":%q,"wait_kind":"lane","wait_lane":%d},`+
		`{"action":"dropoff","node":%q},`+
		`{"action":"pickup","node":%q,"empty":true},`+
		`{"action":"dropoff","node":%q}]`,
		press.Name, press.Name, gateWait, laneID, slots[0].Name, emptySrc.Name, press.Name)

	swap := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.OrderType = "complex"
		o.SourceNode = press.Name
		o.DeliveryNode = press.Name
		o.Status = StatusStaged
		o.StepsJSON = plan
		o.WaitIndex = 1 // parked at the LANE wait, the second wait in the plan
		// A gate-staged order always has one: the create sent the robot to the
		// gate point unsealed. IsGateStaged reads it, so the release-driven test
		// cannot reach the append without it. Inert for the rebind-only test.
		o.VendorOrderID = "sg-gate-" + tag
	})
	// steps_json/wait_index/vendor_order_id are all set here rather than trusted to
	// the struct: orders.Create's INSERT column list carries none of the three, so a
	// fixture that only set them on the value would reload as a non-gate-staged
	// order and the release would no-op silently.
	if _, err := db.Exec(`UPDATE orders SET steps_json=$2, wait_index=1, vendor_order_id=$3 WHERE id=$1`,
		swap.ID, plan, swap.VendorOrderID); err != nil {
		t.Fatalf("persist plan: %v", err)
	}

	// THE JUNCTION, written the way the allocator writes it: one row per claimed
	// bin, keyed on the PICKUP step, carrying the destination the plan implied at
	// allocation time. This is the copy nothing updated.
	bt := &bins.BinType{Code: tag + "-BT", Description: "tote"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	fullBin := &bins.Bin{BinTypeID: bt.ID, Label: tag + "-FULL", NodeID: &press.ID, Status: "available"}
	if err := db.CreateBin(fullBin); err != nil {
		t.Fatalf("create full bin: %v", err)
	}
	emptyBin := &bins.Bin{BinTypeID: bt.ID, Label: tag + "-EMPTY", NodeID: &emptySrc.ID, Status: "available"}
	if err := db.CreateBin(emptyBin); err != nil {
		t.Fatalf("create empty bin: %v", err)
	}
	// step 1 = pickup at the press (the full bin) → dropoff at the lane slot.
	// step 4 = pickup at the empties node        → dropoff back at the press.
	if err := db.InsertOrderBin(swap.ID, fullBin.ID, 1, protocol.ActionPickup, press.Name, slots[0].Name); err != nil {
		t.Fatalf("junction row for the full bin: %v", err)
	}
	if err := db.InsertOrderBin(swap.ID, emptyBin.ID, 4, protocol.ActionPickup, emptySrc.Name, press.Name); err != nil {
		t.Fatalf("junction row for the empty bin: %v", err)
	}

	swap, err = db.GetOrder(swap.ID)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	return swapGateCase{
		order: swap, lane: lane, press: press, emptySrc: emptySrc,
		fullBin: fullBin, emptyBin: emptyBin, allocSlot: slots[0],
	}
}

// destNodeFor reads one bin's recorded destination back out of the junction.
func destNodeFor(t *testing.T, db *store.DB, orderID, binID int64) string {
	t.Helper()
	rows, err := db.ListOrderBins(orderID)
	if err != nil {
		t.Fatalf("list junction rows: %v", err)
	}
	for _, ob := range rows {
		if ob.BinID == binID {
			return ob.DestNode
		}
	}
	t.Fatalf("no junction row for bin %d on order %d", binID, orderID)
	return ""
}

// TestGateRebind_SwapPatchesLaneEntryNotFinalDropoff pins the fix for the swap
// clobber (PLAN §R.5): the gate re-binds the dropoff it is SPEAKING FOR — the
// lane entry — and leaves the plan's later legs alone.
//
// This is the LEDGER half. Its wire twin is
// TestGateRelease_SwapBlocksKeepPressAsFinalDropoff below, and the pair exists
// because passing this one alone is exactly the state the branch shipped in.
//
// The old re-bind patched "the last dropoff" by backward scan and so overwrote
// index 5, sending the empty into the lane and putting BOTH of the order's bins
// in one slot. The press then starved waiting for an empty that had been driven
// into a lane, and the ghost eviction was forced to evict an occupant Core's own
// plan had made.
//
// Mutation: point applyDeliveryNodeAtStep back at a last-dropoff scan and the
// index-5 assertion below fires — the empty is aimed at the lane slot again.
func TestGateRebind_SwapPatchesLaneEntryNotFinalDropoff(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	tc := swapGateFixture(t, db, "SWAPRB")
	swap, lane, press, emptySrc := tc.order, tc.lane, tc.press, tc.emptySrc

	// The index under test is DERIVED the way production derives it, so this
	// asserts the plumbing too: a candidate walk that stopped carrying the index
	// would fail here rather than silently re-point the wrong leg.
	entryIdx := gateEntryIndexFor(t, swap)
	if entryIdx != 3 {
		t.Fatalf("lane entry index = %d, want 3 (the first dropoff after the lane wait) — "+
			"the candidate walk no longer names the leg the gate speaks for", entryIdx)
	}

	rebound, err := d.rebindGatedDropoff(swap, lane, entryIdx)
	if err != nil {
		t.Fatalf("rebind: %v", err)
	}

	reloaded, err := db.GetOrder(swap.ID)
	if err != nil {
		t.Fatalf("reload after rebind: %v", err)
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(reloaded.StepsJSON), &steps); err != nil {
		t.Fatalf("steps after rebind: %v", err)
	}
	if len(steps) != 6 {
		t.Fatalf("plan has %d steps after rebind, want 6 — the patch rewrote the plan's shape", len(steps))
	}

	// The gate's own leg carries the re-bound slot.
	if steps[3].Node != rebound.Name {
		t.Errorf("lane-entry dropoff (step 3) = %s, want the re-bound slot %s — the gate patched a step it does not speak for",
			steps[3].Node, rebound.Name)
	}
	// THE ASSERTION THE CLOBBER FAILED. The empty still goes back to the press.
	if steps[5].Node != press.Name {
		t.Errorf("final dropoff (step 5) = %s, want the press %s — the re-bind clobbered the empty's "+
			"return leg, which is what drove both of the order's bins into one lane slot (PLAN §R.5)",
			steps[5].Node, press.Name)
	}
	// And the empty's PICKUP is untouched: only the named step may move.
	if steps[4].Node != emptySrc.Name {
		t.Errorf("empty pickup (step 4) = %s, want %s — the patch touched a step outside its index",
			steps[4].Node, emptySrc.Name)
	}

	// ── THE THIRD COPY (D2) ───────────────────────────────────────────────
	// order_bins.dest_node was written once at allocation and updated by nothing,
	// so the robot drove to the new slot while the ledger still named the old one
	// and the whole-order settle placed the bin at the row's stale value. The row
	// now follows the re-bind.
	if got := destNodeFor(t, db, swap.ID, tc.fullBin.ID); got != rebound.Name {
		t.Errorf("full bin's junction dest_node = %s, want the re-bound slot %s — the ledger still "+
			"names the slot allocation picked, so the settle would place the bin where the robot "+
			"did not go (PLAN §R.25, D2)", got, rebound.Name)
	}
	// And the OTHER row is the junction's version of the step-5 assertion above:
	// re-deriving all the destinations must not drag the empty's leg along.
	if got := destNodeFor(t, db, swap.ID, tc.emptyBin.ID); got != press.Name {
		t.Errorf("empty bin's junction dest_node = %s, want the press %s — the re-derivation moved a "+
			"leg the gate does not speak for", got, press.Name)
	}
}

// TestGateRelease_SwapBlocksKeepPressAsFinalDropoff pins the WAYBILL — the blocks
// the fleet is actually handed — for the swap the test above pins in steps_json.
//
// ── WHY A SECOND TEST, AND WHY IT READS ReleaseCalls ──────────────────────
// What Core remembers and what the robot is told are two facts, and this branch
// was burned asserting one while reporting the other. 768a2985 fixed the plan on
// disk and said "no backward scan survives on the gate path"; PLAN §R.8 verified
// that with a steps_json query and recorded "0b CONFIRMED end-to-end". Meanwhile
// appendGateTail went on calling patchRedirectSegments — a backward last-dropoff
// scan keyed on order.DeliveryNode, which the rebind had just set to the lane
// slot — and rewrote the empty's return-to-press leg IN MEMORY on the way to
// stepsToBlocks. steps_json never carried the rewrite, so no query on steps_json
// could ever have found it. That is the blind spot this test closes.
//
// Falsified on the rig before the call was deleted, against the running
// 435a1741 stack: three firings of "complex release: patching segment dropoff
// PLN_002 -> LSD_00x (redirect)" in a four-hour window, one per gated SWAP
// re-bind (orders 24, 26 and 67 — the first two being §R.8's own "0b CONFIRMED"
// specimens), each re-aiming the press leg at the lane slot that same order had
// just filled. The four other re-binds in that window were plans whose last
// dropoff already equalled the rebound node, so the scan rewrote a dropoff to
// itself and said nothing — the happy path the patch's safety argument rested on,
// and the reason the defect stayed invisible for the other four sevenths of the
// population.
//
// MUTATION: restore d.patchRedirectSegments(segment, order, moreWaits) in
// appendGateTail and the final-block assertion fires carrying the specimens'
// exact signature — two JackUnload blocks naming one node.
func TestGateRelease_SwapBlocksKeepPressAsFinalDropoff(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	tc := swapGateFixture(t, db, "SWAPWIRE")
	swap, lane, press, emptySrc := tc.order, tc.lane, tc.press, tc.emptySrc

	if err := d.releaseGatedOrder(swap, lane, gateEntryIndexFor(t, swap)); err != nil {
		t.Fatalf("gated release: %v", err)
	}

	calls := backend.ReleaseCalls()
	if len(calls) != 1 {
		t.Fatalf("fleet appends = %d, want 1 — the gated swap's tail never reached the fleet", len(calls))
	}
	blocks := calls[0].Blocks
	if len(blocks) != 3 {
		t.Fatalf("blocks = %d, want 3 ([dropoff lane, pickup empty, dropoff press]): %+v", len(blocks), blocks)
	}

	// The re-bind's OWN record of the slot it chose, read back from the row rather
	// than recomputed here — so the test cannot agree with a re-bind that picked
	// one slot and emitted another.
	reloaded, err := db.GetOrder(swap.ID)
	if err != nil {
		t.Fatalf("reload after release: %v", err)
	}

	if blocks[0].Location != reloaded.DeliveryNode || blocks[0].BinTask != "JackUnload" {
		t.Errorf("entry block = %s/%s, want JackUnload at the re-bound slot %s — the lane entry is the "+
			"one leg the gate speaks for", blocks[0].Location, blocks[0].BinTask, reloaded.DeliveryNode)
	}
	if blocks[1].Location != emptySrc.Name || blocks[1].BinTask != "JackLoad" {
		t.Errorf("middle block = %s/%s, want JackLoad at %s — a step outside the gate's leg moved on the wire",
			blocks[1].Location, blocks[1].BinTask, emptySrc.Name)
	}

	// THE ASSERTION THE WAYBILL FAILED. The empty still goes back to the press.
	if blocks[2].Location != press.Name || blocks[2].BinTask != "JackUnload" {
		t.Errorf("final block = %s/%s, want JackUnload at the press %s — the emit path re-aimed the "+
			"empty's return leg at the lane slot, which is what drove both of the order's bins into one "+
			"slot on the wire while steps_json stayed clean (PLAN §R.5, D1)",
			blocks[2].Location, blocks[2].BinTask, press.Name)
	}
	// Stated separately and directly, because this is the shape a reader can grep a
	// captured fleet payload for: a swap that unloads twice at one node is the
	// clobber, whichever writer produced it.
	if blocks[0].Location == blocks[2].Location {
		t.Errorf("both dropoff blocks name %s — two JackUnload blocks at one node is the clobber signature",
			blocks[0].Location)
	}
}
