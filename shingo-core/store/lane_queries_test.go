//go:build docker

package store_test

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// TestFindStoreSlot_SkipsReservedSlot: FindStoreSlotInLane is deepest-UNRESERVED —
// a slot another order has reserved (even while physically empty + unclaimed) is
// skipped, so two stores pack into distinct tiered slots.
func TestFindStoreSlot_SkipsReservedSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	laneID, _, slot2ID, slot3ID := laneFixture(t, db, "RSV")
	o := testdb.CreateOrder(t, db)

	// Reserve the deepest slot (slot3, depth 3) — empty and unclaimed, but held.
	if err := reservations.AcquireSlot(db.DB, o.ID, slot3ID, "test"); err != nil {
		t.Fatalf("AcquireSlot: %v", err)
	}
	got, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if got.ID != slot2ID {
		t.Fatalf("store slot = %d (depth %v), want slot2=%d — the reserved deepest slot must be skipped",
			got.ID, got.Depth, slot2ID)
	}
}

// TestFindStoreSlot_SkipsStrandedDeepSlot: with a bin at depth 2, the empty
// depth-3 slot is stranded behind it — a robot entering at the mouth cannot
// reach it. The accessibility guard must skip the deepest-empty stranded slot
// and return the reachable depth-1 slot instead (never picks into an air bubble).
func TestFindStoreSlot_SkipsStrandedDeepSlot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	laneID, slot1ID, slot2ID, _ := laneFixture(t, db, "BUBBLE")

	// Occupy depth-2, leaving depth-1 and depth-3 empty. The deep empty (depth-3)
	// is walled off behind the depth-2 bin.
	testdb.CreateBinAtNode(t, db, "PART-A", slot2ID, "BIN-BUBBLE")

	got, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if got.ID != slot1ID {
		t.Fatalf("store slot = %d (depth %v), want slot1=%d — the depth-3 slot stranded behind the depth-2 bin must be skipped",
			got.ID, got.Depth, slot1ID)
	}
}

// TestLaneAcceptsInbound: the resolve-around compatibility read mirrors the mouth
// gate. An empty lane and an inbound-only lane are compatible for a store (same-
// mode sharing is legal, §2); an outbound hold or a dig makes the lane
// incompatible.
func TestLaneAcceptsInbound(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	laneID, _, _, _ := laneFixture(t, db, "ACCEPT")

	// Empty lane → compatible.
	if ok, err := db.LaneAcceptsInbound(laneID); err != nil || !ok {
		t.Fatalf("empty lane: got ok=%v err=%v, want compatible", ok, err)
	}

	// (C1.5: each lane holder is built at dispatched; a confirmed mouth is a hard hold.)

	// An inbound hold shares fine — still compatible.
	oIn := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	if err := reservations.AcquireLanes(db.DB, oIn.ID, reservations.ModeInbound, "test", laneID); err != nil {
		t.Fatalf("AcquireLanes inbound: %v", err)
	}
	if ok, err := db.LaneAcceptsInbound(laneID); err != nil || !ok {
		t.Fatalf("inbound-held lane: got ok=%v err=%v, want compatible (same-mode share)", ok, err)
	}
	if _, err := reservations.ReleaseLanesByOwner(db.DB, oIn.ID); err != nil {
		t.Fatalf("release inbound: %v", err)
	}

	// An outbound hold conflicts — incompatible for an inbound store.
	oOut := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	if err := reservations.AcquireLanes(db.DB, oOut.ID, reservations.ModeOutbound, "test", laneID); err != nil {
		t.Fatalf("AcquireLanes outbound: %v", err)
	}
	if ok, err := db.LaneAcceptsInbound(laneID); err != nil || ok {
		t.Fatalf("outbound-held lane: got ok=%v err=%v, want incompatible", ok, err)
	}
	if _, err := reservations.ReleaseLanesByOwner(db.DB, oOut.ID); err != nil {
		t.Fatalf("release outbound: %v", err)
	}

	// A dig excludes everyone — incompatible.
	oDig := testdb.CreateOrder(t, db, func(o *orders.Order) { o.Status = protocol.StatusDispatched })
	if err := reservations.AcquireLanes(db.DB, oDig.ID, reservations.ModeDig, "test", laneID); err != nil {
		t.Fatalf("AcquireLanes dig: %v", err)
	}
	if ok, err := db.LaneAcceptsInbound(laneID); err != nil || ok {
		t.Fatalf("dig-held lane: got ok=%v err=%v, want incompatible", ok, err)
	}
}

// laneFixture builds a NGRP > LANE > 3 slots hierarchy and returns the lane ID
// plus the three slot IDs (front→back).
func laneFixture(t *testing.T, db *store.DB, prefix string) (laneID int64, slot1ID, slot2ID, slot3ID int64) {
	t.Helper()

	grpType := &nodes.NodeType{Code: prefix + "-NGRP", Name: "nodes.Node Group", IsSynthetic: true}
	db.CreateNodeType(grpType)
	laneType := &nodes.NodeType{Code: prefix + "-LANE", Name: "Lane", IsSynthetic: true}
	db.CreateNodeType(laneType)

	grp := &nodes.Node{Name: prefix + "-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &grpType.ID}
	db.CreateNode(grp)

	lane := &nodes.Node{Name: prefix + "-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
	db.CreateNode(lane)

	d1, d2, d3 := 1, 2, 3
	s1 := &nodes.Node{Name: prefix + "-SLOT-01", Enabled: true, ParentID: &lane.ID, Depth: &d1}
	db.CreateNode(s1)
	s2 := &nodes.Node{Name: prefix + "-SLOT-02", Enabled: true, ParentID: &lane.ID, Depth: &d2}
	db.CreateNode(s2)
	s3 := &nodes.Node{Name: prefix + "-SLOT-03", Enabled: true, ParentID: &lane.ID, Depth: &d3}
	db.CreateNode(s3)

	return lane.ID, s1.ID, s2.ID, s3.ID
}

func TestFindOldestBuriedBin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	laneID, slot1ID, slot2ID, slot3ID := laneFixture(t, db, "OLD")

	bt := &bins.BinType{Code: "OLD-BT", Description: "tote"}
	db.CreateBinType(bt)

	// Two buried bins at depth 2 and 3, both blocked by a shallower bin at depth 1.
	// The shallower bin doesn't need to match the payload — just needs to exist.

	// Blocker at depth 1 (different payload so it doesn't match the query).
	blocker := &bins.Bin{BinTypeID: bt.ID, Label: "OLD-BLOCK", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(blocker)
	db.SetBinManifest(blocker.ID, `{"items":[]}`, "OTHER", 10)
	db.ConfirmBinManifest(blocker.ID, "2024-01-01 00:00:00")

	// Buried bins at depth 2 (newer) and 3 (older) — FindOldestBuriedBin picks the oldest.
	buriedNewer := &bins.Bin{BinTypeID: bt.ID, Label: "OLD-BUR-NEW", NodeID: &slot2ID, Status: "available"}
	db.CreateBin(buriedNewer)
	db.SetBinManifest(buriedNewer.ID, `{"items":[]}`, "PAY-BUR", 10)
	db.ConfirmBinManifest(buriedNewer.ID, "2024-12-01 00:00:00")

	buriedOlder := &bins.Bin{BinTypeID: bt.ID, Label: "OLD-BUR-OLD", NodeID: &slot3ID, Status: "available"}
	db.CreateBin(buriedOlder)
	db.SetBinManifest(buriedOlder.ID, `{"items":[]}`, "PAY-BUR", 10)
	db.ConfirmBinManifest(buriedOlder.ID, "2024-02-01 00:00:00")

	gotBin, gotSlot, err := db.FindOldestBuriedBin(laneID, "PAY-BUR")
	if err != nil {
		t.Fatalf("FindOldestBuriedBin: %v", err)
	}
	if gotBin.ID != buriedOlder.ID {
		t.Errorf("oldest buried bin = %d (%s), want %d (%s)",
			gotBin.ID, gotBin.Label, buriedOlder.ID, buriedOlder.Label)
	}
	if gotSlot.ID != slot3ID {
		t.Errorf("oldest buried slot = %d, want %d", gotSlot.ID, slot3ID)
	}

	// Counts covering the lane.
	n, err := db.CountBinsInLane(laneID)
	if err != nil {
		t.Fatalf("CountBinsInLane: %v", err)
	}
	if n != 3 {
		t.Errorf("CountBinsInLane = %d, want 3", n)
	}
}

func TestListLaneSlotsEmpty(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	// Non-existent lane id — list should come back empty, no error.
	slots, err := db.ListLaneSlots(99999)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(slots) != 0 {
		t.Errorf("unknown lane slots len = %d, want 0", len(slots))
	}

	// CountBinsInLane on unknown lane = 0
	c, err := db.CountBinsInLane(99999)
	if err != nil {
		t.Fatalf("CountBinsInLane: %v", err)
	}
	if c != 0 {
		t.Errorf("unknown lane count = %d, want 0", c)
	}
}

func TestFindSourceBinInLane_NoMatch(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	laneID, _, _, _ := laneFixture(t, db, "NOM")

	// No bins at all — should error (no accessible bin).
	if _, err := db.FindSourceBinInLane(laneID, "PAY-NONE", 0); err == nil {
		t.Error("empty lane should error")
	}

	// FindStoreSlotInLane on an empty lane — deepest is slot3.
	storeSlot, err := db.FindStoreSlotInLane(laneID)
	if err != nil {
		t.Fatalf("FindStoreSlotInLane: %v", err)
	}
	if storeSlot == nil {
		t.Fatal("store slot should not be nil")
	}
	// Should be at depth 3 (deepest empty — back-to-front packing).
	if storeSlot.Depth == nil || *storeSlot.Depth != 3 {
		t.Errorf("store slot depth = %v, want 3 (deepest empty)", storeSlot.Depth)
	}
}

func TestLaneSlotAccessibility(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	laneID, slot1ID, slot2ID, slot3ID := laneFixture(t, db, "ACC")

	bt := &bins.BinType{Code: "ACC-BT"}
	db.CreateBinType(bt)

	// Initially all slots are accessible (no bins blocking).
	for _, id := range []int64{slot1ID, slot2ID, slot3ID} {
		acc, err := db.IsSlotAccessible(id)
		if err != nil {
			t.Fatalf("IsSlotAccessible(%d): %v", id, err)
		}
		if !acc {
			t.Errorf("slot %d should be accessible with empty lane", id)
		}
	}

	// Depths readable.
	d1, _ := db.GetSlotDepth(slot1ID)
	d3, _ := db.GetSlotDepth(slot3ID)
	if d1 != 1 || d3 != 3 {
		t.Errorf("depths = (%d,%d), want (1,3)", d1, d3)
	}

	// Put a bin at slot1 — slot3 should now be inaccessible.
	b := &bins.Bin{BinTypeID: bt.ID, Label: "ACC-B1", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(b)

	acc3, err := db.IsSlotAccessible(slot3ID)
	if err != nil {
		t.Fatalf("IsSlotAccessible(slot3): %v", err)
	}
	if acc3 {
		t.Error("slot3 should be blocked by bin at slot1")
	}

	// slot1 itself is still accessible (nothing at shallower depth).
	acc1, _ := db.IsSlotAccessible(slot1ID)
	if !acc1 {
		t.Error("slot1 should remain accessible")
	}

	// ListLaneSlots returns 3 slots in depth-ascending order.
	slots, err := db.ListLaneSlots(laneID)
	if err != nil {
		t.Fatalf("ListLaneSlots: %v", err)
	}
	if len(slots) != 3 {
		t.Fatalf("slots len = %d, want 3", len(slots))
	}
	if slots[0].ID != slot1ID || slots[2].ID != slot3ID {
		t.Errorf("slot order wrong: first=%d last=%d, want first=%d last=%d",
			slots[0].ID, slots[2].ID, slot1ID, slot3ID)
	}
}

func TestFindBuriedBin_NoBuried(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	laneID, slot1ID, _, _ := laneFixture(t, db, "NOB")

	bt := &bins.BinType{Code: "NOB-BT"}
	db.CreateBinType(bt)

	// Only a front bin — nothing is buried.
	b := &bins.Bin{BinTypeID: bt.ID, Label: "NOB-B1", NodeID: &slot1ID, Status: "available"}
	db.CreateBin(b)
	db.SetBinManifest(b.ID, `{"items":[]}`, "PAY-A", 10)
	db.ConfirmBinManifest(b.ID, "")

	if _, _, err := db.FindBuriedBin(laneID, "PAY-A"); err == nil {
		t.Error("no buried bin should produce error")
	}

	// But a source lookup should succeed and return it.
	src, err := db.FindSourceBinInLane(laneID, "PAY-A", 0)
	if err != nil {
		t.Fatalf("FindSourceBinInLane: %v", err)
	}
	if src.ID != b.ID {
		t.Errorf("source bin = %d, want %d", src.ID, b.ID)
	}
}

// TestGroupSlotState_AnswersAsTheSelectorsDo pins the gate pass's one read of a
// group (nodes.ReadGroupSlotState, RULINGS-C2c-run-through §3.3) to the SQL
// selectors it answers for: for every asker the fixture names — nobody, a
// committed robot holding paper, an unsent plan, a stranger naming a slot, a
// stranger's paper, a holder coming for a deeper bin, a dig — each lane's store
// slot (with the closed-by-claim diagnosis), the group's paper, its unlocked
// children, each lane's slots and bin count come out the same. Then a release
// the pass makes (a committed robot taking a slot as armor, its final drop) is
// made in the store and written into the read, and the store slot every other
// asker is offered still agrees.
func TestGroupSlotState_AnswersAsTheSelectorsDo(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatal(err)
	}
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatal(err)
	}
	grp := &nodes.Node{Name: "GSS-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	mustNo(t, db.CreateNode(grp))
	testdb.MarkGroup(t, db, grp.ID)
	mkLane := func(name string, depth int) (*nodes.Node, []*nodes.Node) {
		lane := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &grp.ID}
		mustNo(t, db.CreateNode(lane))
		var slots []*nodes.Node
		for i := 0; i < depth; i++ {
			d := i
			s := &nodes.Node{Name: name + "-S" + string(rune('0'+i)), Enabled: true, ParentID: &lane.ID, Depth: &d}
			mustNo(t, db.CreateNode(s))
			slots = append(slots, s)
		}
		return lane, slots
	}
	l1, s1 := mkLane("GSS-L1", 4)
	l2, s2 := mkLane("GSS-L2", 3)
	flat := &nodes.Node{Name: "GSS-F", Enabled: true, ParentID: &grp.ID}
	mustNo(t, db.CreateNode(flat))
	away := &nodes.Node{Name: "GSS-AWAY", Enabled: true}
	mustNo(t, db.CreateNode(away))

	order := func(uuid string, status protocol.Status, vendor string, set func(*orders.Order)) *orders.Order {
		o := testdb.CreateOrder(t, db, func(o *orders.Order) {
			o.EdgeUUID, o.Status, o.DeliveryNode = uuid, status, away.Name
			if set != nil {
				set(o)
			}
		})
		if vendor != "" {
			mustNo(t, db.UpdateOrderVendor(o.ID, vendor, "WAITING", "AMR-1"))
		}
		return o
	}
	// A: a robot at the group's spot, holding paper on L1-S2 (committed).
	a := order("gss-a", protocol.StatusStaged, "GSS-VA", func(o *orders.Order) { o.DeliveryNode = s1[2].Name })
	mustNo(t, reservations.AcquireSlot(db.DB, a.ID, s1[2].ID, reservations.GroupRoomTag))
	// B: a plan never sent, holding nothing.
	b := order("gss-b", protocol.StatusQueued, "", nil)
	// N: a dispatched stranger naming L2-S1.
	n := order("gss-n", protocol.StatusInTransit, "GSS-VN", func(o *orders.Order) { o.DeliveryNode = s2[1].Name })
	// P: a never-sent stranger's paper on L2-S2.
	p := order("gss-p", protocol.StatusQueued, "", nil)
	mustNo(t, reservations.AcquireSlot(db.DB, p.ID, s2[2].ID, reservations.GroupRoomTag))
	// H: an unsent demand aiming at the bin at the back of L1 (bin_id + source_node).
	deep := testdb.CreateBinAtNode(t, db, "", s1[3].ID, "GSS-DEEP")
	h := order("gss-h", protocol.StatusQueued, "", func(o *orders.Order) { o.SourceNode = s1[3].Name })
	mustNo(t, reservations.Acquire(db.DB, h.ID, h.ID, deep.ID, "test")) // its aim holds the bin softly
	mustNo(t, db.UpdateOrderBinID(h.ID, deep.ID))
	// D: a dig holding L2's mouth.
	dig := order("gss-d", protocol.StatusInTransit, "GSS-VD", nil)
	if _, err := db.DB.Exec(`INSERT INTO reservations (order_id, resource_kind, node_id, state, mode, reserved_by, created_at)
		VALUES ($1, 'mouth', $2, 'confirmed', 'dig', 'test', NOW())`, dig.ID, l2.ID); err != nil {
		t.Fatalf("the dig's mouth row: %v", err)
	}
	// A bin on the flat child.
	testdb.CreateBinAtNode(t, db, "", flat.ID, "GSS-ONFLAT")

	askers := []int64{0, a.ID, b.ID, n.ID, p.ID, h.ID, dig.ID}
	compare := func(stage string) {
		t.Helper()
		state, err := nodes.ReadGroupSlotState(db.DB, grp.ID)
		if err != nil {
			t.Fatalf("%s: read the group: %v", stage, err)
		}
		for _, owner := range askers {
			for _, lane := range []*nodes.Node{l1, l2} {
				want, wErr := db.FindStoreSlotInLaneExcluding(lane.ID, owner)
				got, open := state.StoreSlot(lane.ID, owner)
				switch {
				case wErr == nil && (got == nil || got.ID != want.ID):
					t.Errorf("%s: owner %d lane %s: the read offers %v, the selector %s", stage, owner, lane.Name, got, want.Name)
				case wErr != nil && got != nil:
					t.Errorf("%s: owner %d lane %s: the read offers %s, the selector none (%v)", stage, owner, lane.Name, got.Name, wErr)
				case wErr != nil && open != errors.Is(wErr, nodes.ErrLaneClosedByClaim):
					t.Errorf("%s: owner %d lane %s: closed-by-claim %v, the selector says %v", stage, owner, lane.Name, open, wErr)
				}
			}
			wantP, err := db.PaperSlotsInGroup(grp.ID, owner)
			if err != nil {
				t.Fatalf("%s: paper: %v", stage, err)
			}
			gotP := state.PaperSlots(owner)
			if len(wantP) != len(gotP) {
				t.Errorf("%s: owner %d: %d paper slot(s) in the read, %d from the selector", stage, owner, len(gotP), len(wantP))
			} else {
				for i := range wantP {
					w, g := wantP[i], gotP[i]
					if w.Slot.ID != g.Slot.ID || w.LaneID != g.LaneID || w.Holder != g.Holder || w.HolderSent != g.HolderSent ||
						w.HolderPriority != g.HolderPriority || w.HolderDemand != g.HolderDemand {
						t.Errorf("%s: owner %d paper %d: read %+v, selector %+v", stage, owner, i, g, w)
					}
				}
			}
			asker := reservations.DigAsker{OrderID: owner, LaneOwner: owner}
			wantC, err := db.ListChildNodesUnlocked(grp.ID, asker)
			if err != nil {
				t.Fatalf("%s: children: %v", stage, err)
			}
			gotC := state.ChildrenUnlocked(asker)
			if names(wantC) != names(gotC) {
				t.Errorf("%s: owner %d: unlocked children %s in the read, %s from the store", stage, owner, names(gotC), names(wantC))
			}
		}
		for _, lane := range []*nodes.Node{l1, l2} {
			wantS, err := db.ListLaneSlots(lane.ID)
			if err != nil {
				t.Fatal(err)
			}
			if names(wantS) != names(state.LaneSlots(lane.ID)) {
				t.Errorf("%s: lane %s slots %s in the read, %s from the store", stage, lane.Name,
					names(state.LaneSlots(lane.ID)), names(wantS))
			}
			wantB, err := db.CountBinsInLane(lane.ID)
			if err != nil {
				t.Fatal(err)
			}
			if wantB != state.LaneBins(lane.ID) {
				t.Errorf("%s: lane %s holds %d bin(s) in the read, %d in the store", stage, lane.Name, state.LaneBins(lane.ID), wantB)
			}
		}
	}
	compare("as built")

	// THE RELEASE, made in the store and written into the read: A takes L2-S0 as
	// armor and lets go of L1-S2; its drop is its last, so its delivery follows.
	state, err := nodes.ReadGroupSlotState(db.DB, grp.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.ConfirmReleaseDrop(l2.ID, s2[0].ID, a.ID, s1[2].ID, nil); err != nil {
		t.Fatalf("A's release hand: %v", err)
	}
	mustNo(t, db.UpdateOrderDeliveryNode(a.ID, s2[0].Name))
	state.NoteReleased(a.ID, s2[0].ID, s1[2].ID, true)
	for _, owner := range askers {
		for _, lane := range []*nodes.Node{l1, l2} {
			want, wErr := db.FindStoreSlotInLaneExcluding(lane.ID, owner)
			got, _ := state.StoreSlot(lane.ID, owner)
			if (wErr == nil) != (got != nil) || (wErr == nil && got.ID != want.ID) {
				t.Errorf("after the release: owner %d lane %s: the noted read offers %v, the selector %v (%v)",
					owner, lane.Name, got, want, wErr)
			}
		}
	}
	compare("after the release")
}

func mustNo(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func names(ns []*nodes.Node) string {
	out := ""
	for _, n := range ns {
		out += n.Name + ","
	}
	return out
}
