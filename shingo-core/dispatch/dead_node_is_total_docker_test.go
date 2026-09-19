//go:build docker

package dispatch

import (
	"errors"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
	"shingocore/store/sourceability"
)

// dead_node_is_total_docker_test.go — "a switched-off node is dead" as a
// property of the whole system rather than of whichever reader remembered it.
//
// THE RULING (2026-09-19): a switched-off node is dead to automation — no
// sourcing, no parking, no digging, no delivering, no counting. Only an
// engineer touches a bin standing there. And DISABLE IS LANE-GRAIN: one switch
// for a lane and everything under it; nobody can switch off slot 2 of 3.
//
// THE PARTS THAT DID NOT HONOUR IT, each pinned below:
//
//	(a) disabling a LANE left every slot under it reading enabled=true — the
//	    toggle was a single-row UPDATE with no cascade, so the lane was off and
//	    nothing under it was.
//	(b) the node editor would switch off one slot inside a lane, producing a
//	    mixed lane the geometry readers and the enabled readers describe
//	    differently.
//	(d) findStoreSlot offered a disabled slot as a delivery candidate, so
//	    automation delivered INTO a dead node.
//	(e) lineUOPByNode counted stock at a dead node, so a line read as supplied
//	    by material no automation will ever fetch.
//	(c) planUnbury emitted an unconditional StepUnbury per blocker, so a dig
//	    planned a ROBOT lifting a bin off a dead node.
//
// ── WHAT DELIBERATELY DOES NOT MOVE ───────────────────────────────────────
//
// THE CORRIDOR GEOMETRY. helpers.LaneBlockerPredicate answers "is something
// physically in front of the target", and a disabled slot with a bin in it is
// still physically in the way. It is NOT gated on enabled, and
// TestDeadNode_ReachabilityIsPhysicalNotConfigured below is the assertion that
// it stays that way: that answer feeds helpers.ReachableSQL —
// bins.AccessibleEmptyOrder, carried_bin_slots, findStoreSlot — every one of
// which needs the physical truth. A walled lane reading as reachable is worse
// than the bug it would be fixing, so the corridor keeps describing itself
// honestly and the PLANNER declines the job (c).

// deadNodeLane is a group with one lane of three slots, packed so that slot 2
// blocks slot 3: the shape every case below varies one flag on.
type deadNodeLane struct {
	db      *store.DB
	groupID int64
	laneID  int64
	slots   []*nodes.Node // depth 0,1,2
}

func setupDeadNodeLane(t *testing.T) deadNodeLane {
	t.Helper()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	grpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "get NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "get LANE type")

	group := &nodes.Node{Name: "DEAD-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &grpType.ID}
	testutil.MustNoErr(t, db.CreateNode(group), "create group")
	lane := &nodes.Node{Name: "DEAD-LANE", IsSynthetic: true, Enabled: true,
		NodeTypeID: &laneType.ID, ParentID: &group.ID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")

	slots := make([]*nodes.Node, 3)
	for i := range slots {
		d := i
		s := &nodes.Node{Name: laneSlotName(i), Enabled: true, ParentID: &lane.ID, Depth: &d}
		testutil.MustNoErr(t, db.CreateNode(s), "create slot")
		slots[i] = s
	}
	_ = std
	return deadNodeLane{db: db, groupID: group.ID, laneID: lane.ID, slots: slots}
}

func laneSlotName(depth int) string {
	return "DEAD-LANE-S" + string(rune('0'+depth))
}

// putBin drops a confirmed bin of PART-A at a slot.
func (f deadNodeLane) putBin(t *testing.T, slot *nodes.Node, label string) *bins.Bin {
	t.Helper()
	bt, err := f.db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "get DEFAULT bin type")
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, Status: "available", NodeID: &slot.ID}
	testutil.MustNoErr(t, f.db.CreateBin(b), "create bin")
	_, err = f.db.DB.Exec(`UPDATE bins SET payload_code='PART-A', uop_remaining=100,
		manifest_confirmed=true, loaded_at=NOW() WHERE id=$1`, b.ID)
	testutil.MustNoErr(t, err, "load bin")
	reread, err := f.db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "re-read bin")
	return reread
}

// disable switches a node off through the ordinary update path, which is what
// the plant's node editor uses.
func (f deadNodeLane) disable(t *testing.T, n *nodes.Node) error {
	t.Helper()
	fresh, err := f.db.GetNode(n.ID)
	testutil.MustNoErr(t, err, "re-read node before disabling")
	fresh.Enabled = false
	return f.db.UpdateNode(fresh)
}

func (f deadNodeLane) enabledOf(t *testing.T, id int64) bool {
	t.Helper()
	n, err := f.db.GetNode(id)
	testutil.MustNoErr(t, err, "re-read node")
	return n.Enabled
}

// --- (a) the cascade -------------------------------------------------------

func TestDeadNode_DisablingALaneDisablesEverySlotUnderIt(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	if err := f.disable(t, &nodes.Node{ID: f.laneID}); err != nil {
		t.Fatalf("disable lane: %v", err)
	}
	for _, s := range f.slots {
		if f.enabledOf(t, s.ID) {
			t.Errorf("%s is still enabled after its lane was switched off.\n\n"+
				"The toggle was a single-row UPDATE with no cascade, so every reader "+
				"that honours `enabled` kept sourcing from, digging in, delivering to "+
				"and counting stock at a slot inside a lane the plant had turned off.",
				s.Name)
		}
	}
}

func TestDeadNode_EnablingALaneHealsAMixedLane(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	// A mixed lane as legacy data holds it: written underneath the grain.
	_, err := f.db.DB.Exec(`UPDATE nodes SET enabled=false WHERE id=$1`, f.slots[1].ID)
	testutil.MustNoErr(t, err, "hand-disable a mid-slot")

	lane, err := f.db.GetNode(f.laneID)
	testutil.MustNoErr(t, err, "read lane")
	lane.Enabled = true
	testutil.MustNoErr(t, f.db.UpdateNode(lane), "re-save the lane")

	if !f.enabledOf(t, f.slots[1].ID) {
		t.Error("re-saving an enabled lane left a hand-disabled slot off. The cascade " +
			"runs on every lane update, not only on a change, precisely so a plant " +
			"that already has a mixed lane is brought back in line.")
	}
}

// --- (b) the grain ---------------------------------------------------------

func TestDeadNode_OneSlotInALaneCannotBeSwitchedOff(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	err := f.disable(t, f.slots[1])
	if err == nil {
		t.Fatal("the node editor switched off one slot inside a lane.\n\n" +
			"Disable is lane-grain. A lane is a physical corridor — a robot reaches " +
			"slot 3 by passing slots 1 and 2 — so a single dead slot in the middle " +
			"is not a state the plant can act on, and it is the only state in which " +
			"the geometry readers (which ignore enabled, because occupancy is " +
			"physical) and the enabled readers describe the same lane differently.")
	}
	if !errors.Is(err, nodes.ErrSlotEnabledFollowsLane) {
		t.Errorf("refused with the wrong error: %v", err)
	}
	if !f.enabledOf(t, f.slots[1].ID) {
		t.Error("the refused update wrote anyway")
	}
}

func TestDeadNode_OrdinaryEditsOfASlotStillPass(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	s, err := f.db.GetNode(f.slots[1].ID)
	testutil.MustNoErr(t, err, "read slot")
	s.Zone = "ZONE-EDITED"
	if err := f.db.UpdateNode(s); err != nil {
		t.Fatalf("an ordinary slot edit was refused: %v\n"+
			"The grain refuses a change to `enabled`, not every edit of a slot.", err)
	}
}

// --- (d) delivery ----------------------------------------------------------

func TestDeadNode_FindStoreSlotSkipsADisabledSlot(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	// Switch the whole lane off — the only way the grain now allows.
	testutil.MustNoErr(t, f.disable(t, &nodes.Node{ID: f.laneID}), "disable lane")

	slot, err := f.db.FindStoreSlotInLane(f.laneID)
	if err == nil && slot != nil {
		t.Errorf("findStoreSlot offered %s inside a switched-off lane.\n\n"+
			"Automation delivering INTO a dead node is the half of the ruling this "+
			"reader was missing: a robot is sent to put a bin somewhere only an "+
			"engineer is allowed to touch.", slot.Name)
	}
}

// --- (e) counting ----------------------------------------------------------

func TestDeadNode_StockAtADeadNodeIsNotCounted(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)
	f.putBin(t, f.slots[0], "DEAD-COUNT")

	before, err := lineUOPAt(t, f.db, f.slots[0].Name)
	testutil.MustNoErr(t, err, "count before")
	if before == 0 {
		t.Fatal("fixture is wrong: the bin is not being counted even while enabled")
	}

	testutil.MustNoErr(t, f.disable(t, &nodes.Node{ID: f.laneID}), "disable lane")

	after, err := lineUOPAt(t, f.db, f.slots[0].Name)
	testutil.MustNoErr(t, err, "count after")
	if after != 0 {
		t.Errorf("stock at a switched-off node is still counted (%d UOP).\n\n"+
			"NodeEnabledSQL's own comment already said nothing automated \"counts "+
			"stock on one\". The number feeds a line's time-to-empty, so counting it "+
			"makes the line look supplied by material no automation will ever fetch — "+
			"the replenishment that should have been raised is not, and the shortfall "+
			"arrives at the line instead of in the ledger.", after)
	}
}

// lineUOPAt reads the counting surface through its real entry point
// (sourceability.BuildInputs -> lineUOPByNode) rather than re-spelling the
// query, so the pin moves with the reader and not with a copy of it.
func lineUOPAt(t *testing.T, db *store.DB, nodeName string) (int, error) {
	t.Helper()
	in, err := sourceability.BuildInputs(db.DB, time.Hour)
	if err != nil {
		return 0, err
	}
	return in.LineUOP[nodeName], nil
}

// --- (c) the dig, and the geometry that must NOT move ----------------------

func TestDeadNode_DigIsRefusedWhenABlockerStandsOnADeadNode(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)

	// The shape the brief named: a disabled mid-slot holding a bin, with a live
	// target behind it. Written under the grain because the editor can no longer
	// produce it — this is the legacy row the refusal is belt-and-braces for.
	blocker := f.putBin(t, f.slots[1], "DEAD-BLOCKER")
	target := f.putBin(t, f.slots[2], "DEAD-TARGET")
	_, err := f.db.DB.Exec(`UPDATE nodes SET enabled=false WHERE id=$1`, f.slots[1].ID)
	testutil.MustNoErr(t, err, "hand-disable the blocker's slot")

	targetSlot, err := f.db.GetNode(f.slots[2].ID)
	testutil.MustNoErr(t, err, "read target slot")
	lane, err := f.db.GetNode(f.laneID)
	testutil.MustNoErr(t, err, "read lane")

	_, _, planErr := planUnbury(f.db, target, targetSlot, lane, f.groupID, reservations.Anyone, nil)
	if planErr == nil {
		t.Fatalf("the dig was planned. It emits one StepUnbury per blocker, so it just "+
			"told a ROBOT to lift bin %d off %s — a node the plant has switched off, "+
			"where the ruling reserves every touch for an engineer.",
			blocker.ID, f.slots[1].Name)
	}
	if !errors.Is(planErr, ErrBlockerAtDisabledNode) {
		t.Fatalf("refused with the wrong error: %v", planErr)
	}
	if got := classifyPlanError(planErr); got != laneClearBlockerAtDisabledNode {
		t.Errorf("classifyPlanError = %v, want laneClearBlockerAtDisabledNode — the "+
			"cause has to reach the dig-parking reasons or the floor sees a stuck "+
			"lane with no sentence attached", got)
	}
}

// TestDeadNode_ReachabilityIsPhysicalNotConfigured is the assertion that the
// corridor geometry did NOT move, and it is as load-bearing as the five fixes.
//
// An occupied slot walls the lane whether or not it is switched on. If this
// ever goes red because `enabled` was added to helpers.LaneBlockerPredicate,
// the consequence is not a stricter dig — it is bins.AccessibleEmptyOrder,
// carried_bin_slots and findStoreSlot all reading a physically walled lane as
// reachable, and routing robots into it.
func TestDeadNode_ReachabilityIsPhysicalNotConfigured(t *testing.T) {
	t.Parallel()
	f := setupDeadNodeLane(t)
	f.putBin(t, f.slots[1], "DEAD-WALL")

	before, err := f.db.IsSlotAccessible(f.slots[2].ID)
	testutil.MustNoErr(t, err, "accessibility before")
	if before {
		t.Fatal("fixture is wrong: the target is not walled even with a bin in front")
	}

	_, err = f.db.DB.Exec(`UPDATE nodes SET enabled=false WHERE id=$1`, f.slots[1].ID)
	testutil.MustNoErr(t, err, "hand-disable the occupied slot")

	after, err := f.db.IsSlotAccessible(f.slots[2].ID)
	testutil.MustNoErr(t, err, "accessibility after")
	if after {
		t.Error("switching a slot off made the bin standing on it stop counting as a " +
			"wall.\n\nOccupancy is PHYSICAL. The bin is still in the corridor. This " +
			"answer feeds helpers.ReachableSQL — AccessibleEmptyOrder, " +
			"carried_bin_slots, findStoreSlot — so a lane that reads reachable here " +
			"gets a robot sent into it. The dig refuses at the PLANNER " +
			"(ErrBlockerAtDisabledNode); the geometry keeps telling the truth.")
	}
}
