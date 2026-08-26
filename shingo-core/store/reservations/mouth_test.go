//go:build docker

package reservations_test

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// laneWithSlots creates a LANE node with slotCount depth-ordered child slots and
// returns the lane's node id. slotCount==1 makes a depth-1 (mouth-exempt) lane.
func laneWithSlots(t *testing.T, db *store.DB, laneName string, slotCount int) int64 {
	t.Helper()
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE node type: %v", err)
	}
	lane := &nodes.Node{Name: laneName, IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane %s: %v", laneName, err)
	}
	for i := 0; i < slotCount; i++ {
		d := i
		slot := &nodes.Node{
			Name:     fmt.Sprintf("%s-S%d", laneName, i),
			Enabled:  true,
			ParentID: &lane.ID,
			Depth:    &d,
		}
		if err := db.CreateNode(slot); err != nil {
			t.Fatalf("create slot %s-S%d: %v", laneName, i, err)
		}
	}
	return lane.ID
}

func mouthCount(t *testing.T, db *store.DB, laneID int64) int {
	t.Helper()
	rows, err := reservations.ActiveMouthRows(db.DB, laneID)
	if err != nil {
		t.Fatalf("ActiveMouthRows(lane=%d): %v", laneID, err)
	}
	return len(rows)
}

// TestMouth_FreeLaneAdmits: a free lane admits an acquirer and one row lands.
func TestMouth_FreeLaneAdmits(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-FREE", 3)
	o := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, o.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("acquire on free lane: %v", err)
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after free acquire = %d, want 1", got)
	}
}

// TestMouth_SameModeShares: two different orders in the SAME mode both admit and
// both rows coexist — the press-left+right / store-behind-store case.
func TestMouth_SameModeShares(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-SHARE", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("A acquire inbound: %v", err)
	}
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("B acquire same-mode inbound must share, got: %v", err)
	}
	if got := mouthCount(t, db, lane); got != 2 {
		t.Fatalf("mouth rows after same-mode share = %d, want 2", got)
	}
}

// TestMouth_DifferentModeConflicts: an outbound acquirer is refused while an
// inbound holds the lane, and the refused take leaves no row.
func TestMouth_DifferentModeConflicts(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-MIX", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("A acquire inbound: %v", err)
	}
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeOutbound, "test", lane); err != reservations.ErrReservationConflict {
		t.Fatalf("B acquire outbound: want ErrReservationConflict, got %v", err)
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after refused mix = %d, want 1 (only A)", got)
	}
}

// TestMouth_DigExcludesEveryone: a dig admits alone and refuses any other mode,
// and a dig is refused into an already-held lane — dig is exclusive both ways.
func TestMouth_DigExcludesEveryone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	// dig admits on a free lane, then refuses an inbound.
	digLane := laneWithSlots(t, db, "LANE-DIG", 3)
	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeDig, "test", digLane); err != nil {
		t.Fatalf("A acquire dig on free lane: %v", err)
	}
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeInbound, "test", digLane); err != reservations.ErrReservationConflict {
		t.Fatalf("inbound into dig-held lane: want conflict, got %v", err)
	}

	// A held inbound refuses a dig trying to enter.
	inLane := laneWithSlots(t, db, "LANE-INB", 3)
	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", inLane); err != nil {
		t.Fatalf("A acquire inbound: %v", err)
	}
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeDig, "test", inLane); err != reservations.ErrReservationConflict {
		t.Fatalf("dig into inbound-held lane: want conflict, got %v", err)
	}
}

// TestMouth_Idempotent: re-acquiring the same (owner, mode, lane) does not add a
// second row.
func TestMouth_Idempotent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-IDEM", 3)
	a := testdb.CreateOrder(t, db)

	for i := 0; i < 3; i++ {
		if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
			t.Fatalf("acquire #%d: %v", i, err)
		}
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after 3 idempotent acquires = %d, want 1", got)
	}
}

// TestMouth_AllOrNothingMultiLane: a multi-lane acquire that conflicts on one
// lane takes NONE of them (all-or-nothing), and the rolled-back tx leaves no
// advisory lock — a fresh acquire on the clean lane succeeds immediately.
func TestMouth_AllOrNothingMultiLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	clean := laneWithSlots(t, db, "LANE-CLEAN", 3)
	blocked := laneWithSlots(t, db, "LANE-BLOCKED", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	// B holds the blocked lane outbound.
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeOutbound, "test", blocked); err != nil {
		t.Fatalf("B acquire outbound on blocked lane: %v", err)
	}
	// A wants both clean+blocked inbound → conflict on blocked → neither taken.
	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", clean, blocked); err != reservations.ErrReservationConflict {
		t.Fatalf("A multi-lane acquire: want conflict, got %v", err)
	}
	if got := mouthCount(t, db, clean); got != 0 {
		t.Fatalf("clean lane rows after rolled-back multi-acquire = %d, want 0 (all-or-nothing)", got)
	}
	// The rolled-back tx must have released its advisory lock on the clean lane —
	// a fresh single-lane acquire completes (would hang/fail if the lock leaked).
	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", clean); err != nil {
		t.Fatalf("acquire clean lane after rollback (lock leak?): %v", err)
	}
	if got := mouthCount(t, db, clean); got != 1 {
		t.Fatalf("clean lane rows after clean re-acquire = %d, want 1", got)
	}
}

// TestMouth_ConcurrentRaceOneWinner: two orders race one lane in conflicting
// modes; the advisory lock serializes them so exactly one wins. Run under -race.
func TestMouth_ConcurrentRaceOneWinner(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-RACE", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	var wg sync.WaitGroup
	errs := make([]error, 2)
	wg.Add(2)
	go func() {
		defer wg.Done()
		errs[0] = reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane)
	}()
	go func() {
		defer wg.Done()
		errs[1] = reservations.AcquireLanes(db.DB, b.ID, reservations.ModeOutbound, "test", lane)
	}()
	wg.Wait()

	wins, conflicts := 0, 0
	for _, e := range errs {
		switch e {
		case nil:
			wins++
		case reservations.ErrReservationConflict:
			conflicts++
		default:
			t.Fatalf("unexpected acquire error: %v", e)
		}
	}
	if wins != 1 || conflicts != 1 {
		t.Fatalf("race outcome: wins=%d conflicts=%d, want 1/1", wins, conflicts)
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after race = %d, want 1", got)
	}
}

// TestMouth_ReleaseIsOwnerScoped: the G3 class, structurally. A wrong-owner
// release is a no-op; the owner's own release frees the lane.
func TestMouth_ReleaseIsOwnerScoped(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-REL", 3)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("A acquire: %v", err)
	}
	// B releasing A's lane deletes nothing.
	if err := reservations.ReleaseLane(db.DB, b.ID, lane); err != nil {
		t.Fatalf("B release (no-op): %v", err)
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows after foreign release = %d, want 1 (A still holds)", got)
	}
	// A releasing its own lane frees it.
	if err := reservations.ReleaseLane(db.DB, a.ID, lane); err != nil {
		t.Fatalf("A release: %v", err)
	}
	if got := mouthCount(t, db, lane); got != 0 {
		t.Fatalf("mouth rows after owner release = %d, want 0", got)
	}
}

// TestMouth_Depth1Exempt: a single-slot lane takes NO mouth row, so even
// conflicting modes both "succeed" (the slot reservation serializes it).
// TestMouth_Depth1IsGatedLikeAnyOtherLane INVERTS TestMouth_Depth1Exempt, which
// asserted that a single-slot lane took no mouth row and admitted opposing modes
// freely: "B acquire opposite mode on depth-1 lane must be exempt (no
// conflict)", "mouth rows on depth-1 lane = %d, want 0 (exempt)".
//
// The exemption's justification was "digs never touch depth-1 lanes: nothing can
// be buried in a lane with one slot" — true of the lane a dig EXCAVATES, false
// of the lane it PARKS A BLOCKER IN. The lane-stress seed's LS_S1..S3 are
// single-slot and it calls them "the cheapest shuffle destinations in the
// group", so digs touch them constantly and every visit was ungated.
//
// Inverted in place rather than deleted and rewritten: the assertion that
// changed sign is the record of what changed.
func TestMouth_Depth1IsGatedLikeAnyOtherLane(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-D1", 1)
	a := testdb.CreateOrder(t, db)
	b := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("A acquire on depth-1 lane: %v", err)
	}
	if got := mouthCount(t, db, lane); got != 1 {
		t.Fatalf("mouth rows on depth-1 lane = %d, want 1 — a one-slot lane still has a corridor, "+
			"and the row arbitrates the corridor rather than the slot", got)
	}
	if err := reservations.AcquireLanes(db.DB, b.ID, reservations.ModeOutbound, "test", lane); err != reservations.ErrReservationConflict {
		t.Fatalf("B acquire opposite mode on a depth-1 lane: want ErrReservationConflict, got %v — "+
			"a robot parking a blocker meets a robot coming out with a bin regardless of how many "+
			"slots the lane has", err)
	}

	// A DIG TAKES ONE TOO, which is the arm the exemption actually cost: a dig
	// parking a blocker in a single-slot lane held nothing there at all.
	digLane := laneWithSlots(t, db, "LANE-D1-DIG", 1)
	dig := testdb.CreateOrder(t, db)
	if err := reservations.AcquireLanes(db.DB, dig.ID, reservations.ModeDig, "test", digLane); err != nil {
		t.Fatalf("dig acquire on depth-1 lane: %v", err)
	}
	if got := mouthCount(t, db, digLane); got != 1 {
		t.Fatalf("dig mouth rows on depth-1 lane = %d, want 1", got)
	}
}

// TestMouth_TerminalizeOrderDeletesRow (C2): the kind-blind per-order release in
// TerminalizeOrder deletes a mouth row alongside everything else.
func TestMouth_TerminalizeOrderDeletesRow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-TERM", 3)
	a := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, a.ID, reservations.ModeInbound, "test", lane); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	// TerminalizeOrder became a compare-and-swap on main (it reports whether THIS
	// call landed the terminal write). The order is live and uncontended here, so
	// a declined swap would mean the terminal path never ran — and the row-count
	// assertion below would then be passing for the wrong reason.
	swapped, err := db.TerminalizeOrder(a.ID, protocol.StatusConfirmed, "")
	if err != nil {
		t.Fatalf("TerminalizeOrder: %v", err)
	}
	if !swapped {
		t.Fatal("TerminalizeOrder declined the swap on a live, uncontended order")
	}
	if got := mouthCount(t, db, lane); got != 0 {
		t.Fatalf("mouth rows after TerminalizeOrder = %d, want 0", got)
	}
}

// TestLaneForNode: the one-hop parent walk resolves a lane slot to its LANE,
// and returns nil for non-lane-slot nodes.
func TestLaneForNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	// A real lane with slots: a slot resolves to the lane.
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	if err != nil {
		t.Fatalf("get LANE type: %v", err)
	}
	lane := &nodes.Node{Name: "LANE-WALK", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(lane); err != nil {
		t.Fatalf("create lane: %v", err)
	}
	d0 := 0
	slot := &nodes.Node{Name: "LANE-WALK-S0", Enabled: true, ParentID: &lane.ID, Depth: &d0}
	if err := db.CreateNode(slot); err != nil {
		t.Fatalf("create slot: %v", err)
	}
	got, err := db.LaneForNode(slot.ID)
	if err != nil {
		t.Fatalf("LaneForNode(slot): %v", err)
	}
	if got == nil || got.ID != lane.ID {
		t.Fatalf("LaneForNode(slot) = %v, want lane %d", got, lane.ID)
	}

	// A node parented by a non-LANE (NGRP) resolves to nil.
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	ngrp := &nodes.Node{Name: "NGRP-WALK", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	if err := db.CreateNode(ngrp); err != nil {
		t.Fatalf("create ngrp: %v", err)
	}
	direct := &nodes.Node{Name: "NGRP-WALK-DIRECT", Enabled: true, ParentID: &ngrp.ID}
	if err := db.CreateNode(direct); err != nil {
		t.Fatalf("create ngrp-direct: %v", err)
	}
	got, err = db.LaneForNode(direct.ID)
	if err != nil {
		t.Fatalf("LaneForNode(ngrp-direct): %v", err)
	}
	if got != nil {
		t.Fatalf("LaneForNode(ngrp-direct) = %v, want nil", got)
	}

	// A parentless node resolves to nil.
	got, err = db.LaneForNode(ngrp.ID)
	if err != nil {
		t.Fatalf("LaneForNode(root): %v", err)
	}
	if got != nil {
		t.Fatalf("LaneForNode(root) = %v, want nil", got)
	}
}

// TestAuditLaneGeometry: a clean LANE+slots draws no warning; an NGRP-direct
// tiered node and a lane nested under a lane each draw one.
func TestAuditLaneGeometry(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	// Clean lane — must NOT appear in the audit.
	laneWithSlots(t, db, "LANE-OK", 3)
	warnings, err := db.AuditLaneGeometry()
	if err != nil {
		t.Fatalf("audit (clean): %v", err)
	}
	for _, w := range warnings {
		if strings.Contains(w, "LANE-OK") {
			t.Fatalf("clean lane flagged by audit: %q", w)
		}
	}

	// NGRP-direct tiered node — a depth on a non-lane child.
	ngrpType, _ := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	ngrp := &nodes.Node{Name: "NGRP-BAD", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	if err := db.CreateNode(ngrp); err != nil {
		t.Fatalf("create ngrp: %v", err)
	}
	d0 := 0
	tiered := &nodes.Node{Name: "NGRP-BAD-TIER", Enabled: true, ParentID: &ngrp.ID, Depth: &d0}
	if err := db.CreateNode(tiered); err != nil {
		t.Fatalf("create tiered ngrp child: %v", err)
	}

	// Lane nested under a lane.
	laneType, _ := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	outer := &nodes.Node{Name: "LANE-OUTER", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	if err := db.CreateNode(outer); err != nil {
		t.Fatalf("create outer lane: %v", err)
	}
	inner := &nodes.Node{Name: "LANE-INNER", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &outer.ID}
	if err := db.CreateNode(inner); err != nil {
		t.Fatalf("create nested lane: %v", err)
	}

	warnings, err = db.AuditLaneGeometry()
	if err != nil {
		t.Fatalf("audit (bad): %v", err)
	}
	var sawTier, sawNest bool
	for _, w := range warnings {
		if strings.Contains(w, "NGRP-BAD-TIER") {
			sawTier = true
		}
		if strings.Contains(w, "LANE-INNER") {
			sawNest = true
		}
	}
	if !sawTier {
		t.Errorf("audit did not flag the NGRP-direct tiered node; warnings=%v", warnings)
	}
	if !sawNest {
		t.Errorf("audit did not flag the nested lane; warnings=%v", warnings)
	}
}

// TestMouth_BeneficiaryOwnHoldDoesNotRefuseItsOwnDig is the LS_C5 rule at the
// row layer: a dig raised ON BEHALF OF an order is not refused by that order's
// own mouth hold, and IS still refused by anybody else's.
//
// Two owners, two rows, one lane, and that pairing is deliberate — the dweller
// is staged at the mouth, not inside the lane, and HandOffLaneToPicker is
// already written for finding the picker holding the lane when the dig ends.
//
// MUTATION (verified): widen the exemption to every holder — the owner-blind
// precondition fires ("the fixture is not reproducing LS_C5"). The narrowness
// assertion at the bottom of this test is the same rule stated positively;
// TestDigAdmissible_ below is where that widening trips a real assertion.
func TestMouth_BeneficiaryOwnHoldDoesNotRefuseItsOwnDig(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-BENEF", 3)

	dweller := testdb.CreateOrder(t, db)
	digParent := testdb.CreateOrder(t, db)
	stranger := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, dweller.ID, reservations.ModeOutbound, "test", lane); err != nil {
		t.Fatalf("dweller acquire outbound: %v", err)
	}

	// Owner-blind, this is the refusal that ran 16,947 times.
	if err := reservations.AcquireLanes(db.DB, digParent.ID, reservations.ModeDig, "test", lane); err != reservations.ErrReservationConflict {
		t.Fatalf("owner-blind dig into the dweller's lane: want conflict, got %v — the fixture is not "+
			"reproducing LS_C5", err)
	}

	// On the dweller's behalf, it admits.
	benef := reservations.AskerFor(dweller.ID, dweller.ID)
	if err := reservations.AcquireLanesFor(db.DB, digParent.ID, reservations.ModeDig, benef, "test", lane); err != nil {
		t.Fatalf("dig raised FOR the dweller must admit past the dweller's own row, got: %v", err)
	}
	if got := mouthCount(t, db, lane); got != 2 {
		t.Fatalf("mouth rows = %d, want 2 (the dweller's outbound and the dig) — the exemption admits "+
			"the dig beside the hold, it does not evict it", got)
	}

	// And the exemption is exactly one order wide: a dig for a THIRD order is
	// still refused by the dweller's row.
	other := laneWithSlots(t, db, "LANE-BENEF2", 3)
	if err := reservations.AcquireLanes(db.DB, dweller.ID, reservations.ModeOutbound, "test", other); err != nil {
		t.Fatalf("dweller acquire outbound on second lane: %v", err)
	}
	if err := reservations.AcquireLanesFor(db.DB, digParent.ID, reservations.ModeDig,
		reservations.AskerFor(stranger.ID, stranger.ID), "test", other); err != reservations.ErrReservationConflict {
		t.Fatalf("dig for an unrelated order: want conflict, got %v — only the rescued order's own "+
			"holds are exempt", err)
	}
}

// TestMouth_PlainDigUpgradesTheOwnersOwnHold is the same rule where the owner
// and the beneficiary are ONE order — planBuriedReshuffle re-parents the demand
// onto its own excavation, so the order asking for the dig row is the order
// already holding an outbound one.
//
// Owner-blind this was not a wait but a WEDGE: admitMouth refuses one owner two
// modes on one lane, so the demand could never dig the lane it was holding and
// the answer would never change. The row is upgraded instead — one owner, one
// lane, one row, strongest mode wins.
//
// MUTATION (verified): disable the admitUpgrade arm — the acquire comes back
// "order N already holds lane L as outbound, cannot also hold as dig", which is
// the wedge itself.
func TestMouth_PlainDigUpgradesTheOwnersOwnHold(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-UPGRADE", 3)
	demand := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, demand.ID, reservations.ModeOutbound, "test", lane); err != nil {
		t.Fatalf("demand acquire outbound: %v", err)
	}
	// Owner-blind, its own row wedges it out of its own dig.
	err := reservations.AcquireLanes(db.DB, demand.ID, reservations.ModeDig, "test", lane)
	if err == nil {
		t.Fatal("owner-blind, one owner holding one lane in two modes must be refused — the fixture " +
			"is not reproducing the wedge")
	}
	if !strings.Contains(err.Error(), "cannot also hold as") {
		t.Fatalf("owner-blind refusal should be the loud two-modes error, got: %v", err)
	}

	self := reservations.AskerFor(demand.ID, demand.ID)
	if err := reservations.AcquireLanesFor(db.DB, demand.ID, reservations.ModeDig, self, "lanelock", lane); err != nil {
		t.Fatalf("a demand digging the lane it holds must upgrade its own row, got: %v", err)
	}
	rows, err := reservations.ActiveMouthRows(db.DB, lane)
	if err != nil {
		t.Fatalf("read mouth rows: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("mouth rows = %d, want 1 — one owner on one lane is ONE row, not two", len(rows))
	}
	if rows[0].OrderID != demand.ID || rows[0].Mode != reservations.ModeDig {
		t.Fatalf("row = %+v, want order %d in mode dig", rows[0], demand.ID)
	}
	// And the upgraded row still excludes everyone else, which is what makes it
	// a real dig claim rather than a relabelled outbound hold.
	other := testdb.CreateOrder(t, db)
	if err := reservations.AcquireLanes(db.DB, other.ID, reservations.ModeOutbound, "test", lane); err != reservations.ErrReservationConflict {
		t.Fatalf("outbound into the upgraded dig's lane: want conflict, got %v", err)
	}
}

// TestDigAdmissible_AgreesWithTheAcquireAboutTheBeneficiary pins the two
// readers together. The pre-check exists because they used to ask different
// questions; a pre-check that refuses what the acquire would admit is the same
// defect with the answers swapped round.
//
// MUTATION (verified): revert DigAdmissible to `len(holders) == 0` — the
// beneficiary arm fires. MUTATION (verified): widen the exemption to every
// holder — the stranger arm fires. This test is where the exemption's WIDTH is
// pinned, because both directions are plain assertions here.
func TestDigAdmissible_AgreesWithTheAcquireAboutTheBeneficiary(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	lane := laneWithSlots(t, db, "LANE-PRECHECK", 3)
	dweller := testdb.CreateOrder(t, db)
	stranger := testdb.CreateOrder(t, db)

	if err := reservations.AcquireLanes(db.DB, dweller.ID, reservations.ModeOutbound, "test", lane); err != nil {
		t.Fatalf("dweller acquire outbound: %v", err)
	}

	blind, err := reservations.DigAdmissible(db.DB, lane, reservations.Anyone)
	if err != nil {
		t.Fatalf("DigAdmissible(Anyone): %v", err)
	}
	if blind {
		t.Error("Anyone must keep the owner-blind answer: a held lane is not admissible")
	}

	forDweller, err := reservations.DigAdmissible(db.DB, lane, reservations.AskerFor(dweller.ID, dweller.ID))
	if err != nil {
		t.Fatalf("DigAdmissible(dweller): %v", err)
	}
	if !forDweller {
		t.Error("a dig raised for the order holding the lane must be admissible — this is the " +
			"pre-check half of the LS_C5 two-cycle")
	}

	forStranger, err := reservations.DigAdmissible(db.DB, lane, reservations.AskerFor(stranger.ID, stranger.ID))
	if err != nil {
		t.Fatalf("DigAdmissible(stranger): %v", err)
	}
	if forStranger {
		t.Error("a dig raised for an unrelated order must still be refused")
	}
}
