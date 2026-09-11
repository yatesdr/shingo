//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// pair_rule_docker_test.go — the pair rule, pinned by what it does.
//
// complex_pair.go replaced three dispatch-time holds with one rule: both legs of
// a coordinated swap are admitted in the same scanner pass or neither is, and a
// parked pair holds nothing. Nothing drove two legs through
// DispatchPreparedComplex and read the outcome; the only test naming the rule
// checked the order of two substrings in its source text. Census 3–5 below
// replace that half of TestComplexDispatch_AsksAdmission, which now checks the
// solo path only.
//
// Two kinds of test live here, and each header says which it is:
//
//   - A DEFECT pin fails at the tree it was written against (bcbde0d2), for the
//     reason its header names.
//   - A COVERAGE pin passes there, and its header names the mutation that makes
//     it fail. A test that has never failed is a claim, not a check.
//
// The legs are two_robot and press-index shapes written out by hand, mirroring
// the Edge builders (BuildTwoRobotSwapSteps, BuildTwoRobotPressIndexSwapSteps),
// because Core cannot import the Edge module. The rule itself reads no mode name,
// so the shapes matter only for which leg fetches the replacement and which one
// lifts the resident.

// ── census 3: both legs source, one pass ────────────────────────────────────

// TestPairRule_BothLegsSourceDispatchInOnePass is the happy arm: a two_robot
// pair whose supply can source and whose evac can lift its resident goes to the
// fleet in ONE scanner pass, driven by whichever leg the scan reaches.
//
// COVERAGE PIN. Passes at bcbde0d2. MUTATION: in dispatchPairInOnePass, return
// after the first leg's fleet create. The partner is left acquiring and this
// fails naming it.
func TestPairRule_BothLegsSourceDispatchInOnePass(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR3-STAGE")
	out := prNode(t, db, "PR3-OUT")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PR3-FRESH")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "PR3-RESIDENT")

	prSubmitLeg(d, "pr3-supply", "pr3-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(sd.StorageNode.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	prSubmitLeg(d, "pr3-evac", "pr3-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(out.Name))

	prScanPass(t, d, db, "pr3-evac", "pr3-supply")

	supply, evac := prReloadUUID(t, db, "pr3-supply"), prReloadUUID(t, db, "pr3-evac")
	if supply.VendorOrderID == "" || evac.VendorOrderID == "" {
		t.Fatalf("the pair did not go to the fleet in one pass: supply %s vendor=%q cause=%q, evac %s "+
			"vendor=%q cause=%q. Both legs could source, so both-or-neither must come out BOTH",
			supply.Status, supply.VendorOrderID, supply.QueueCause, evac.Status, evac.VendorOrderID, evac.QueueCause)
	}
	for _, o := range []*orders.Order{supply, evac} {
		if protocol.IsAcquiring(o.Status) {
			t.Errorf("order %s dispatched but is still %q — the fleet create did not move it off the "+
				"acquiring set, so the next scan would acquire for it again", o.EdgeUUID, o.Status)
		}
	}
}

// ── census 4: the evac's outbound is full, so the pair parks ────────────────

// TestPairRule_BlockedEvacParksThePairHoldingNothing is the park arm. The supply
// (the leader, lower id) acquires everything first; then the evac's outbound — a
// lane slot with a carrier already in it — is refused. The pair must come out
// with neither leg dispatched, neither holding a bin, a reservation or a lane,
// and ONE cause on both rows. Then the slot empties and the next pass admits
// both, which is what makes the park a wait rather than a stall.
//
// COVERAGE PIN. Passes at bcbde0d2. MUTATIONS: drop ReleaseOrderHoldings from
// parkPair (the supply keeps its claimed bin and the holds-nothing assertion
// fires); drop the cause-mirroring loop from parkPair (the supply's row keeps no
// cause and the one-cause assertion fires).
func TestPairRule_BlockedEvacParksThePairHoldingNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR4-STAGE")
	// A one-slot lane whose slot is occupied: a concrete storage dropoff with no
	// room, which reserveComplexDestination refuses.
	full := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "PR4", NumSlots: 1})
	outSlot := full.Slots[0]
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PR4-FRESH")
	prResident(t, db, sd.LineNode, sd.BinType.ID, sd.Payload.Code, "PR4-RESIDENT")

	prSubmitLeg(d, "pr4-supply", "pr4-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(sd.StorageNode.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	prSubmitLeg(d, "pr4-evac", "pr4-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(outSlot.Name))

	prScanPass(t, d, db, "pr4-evac", "pr4-supply")

	supply, evac := prReloadUUID(t, db, "pr4-supply"), prReloadUUID(t, db, "pr4-evac")
	if supply.VendorOrderID != "" || evac.VendorOrderID != "" {
		t.Fatalf("a leg reached the fleet while its partner's outbound was full: supply vendor=%q, evac "+
			"vendor=%q. One leg committed without the other is the half-dispatched pair the rule exists "+
			"to make unconstructible", supply.VendorOrderID, evac.VendorOrderID)
	}
	prAssertHoldsNothing(t, db, supply, "the leader after its partner was refused")
	prAssertHoldsNothing(t, db, evac, "the refused leg")
	if evac.QueueCause != string(CauseDropoffOccupied) {
		t.Errorf("evac cause = %q, want %q — the refusal names the thing that is missing",
			evac.QueueCause, CauseDropoffOccupied)
	}
	if supply.QueueCause != evac.QueueCause || supply.QueueCode != evac.QueueCode || supply.QueueReason == "" {
		t.Errorf("one pair, two answers: supply %q/%q (%q), evac %q/%q. A parked pair carries the blocked "+
			"leg's cause on both rows, so the operator is sent to one place",
			supply.QueueCode, supply.QueueCause, supply.QueueReason, evac.QueueCode, evac.QueueCause)
	}

	// The releaser: the slot empties, and the very next pass admits both.
	mustExecDispatch(t, db, `DELETE FROM bins WHERE id=$1`, full.TargetBin.ID)
	prScanPass(t, d, db, "pr4-evac", "pr4-supply")
	supply, evac = prReloadUUID(t, db, "pr4-supply"), prReloadUUID(t, db, "pr4-evac")
	if supply.VendorOrderID == "" || evac.VendorOrderID == "" {
		t.Fatalf("the outbound slot emptied and the pair still did not go (supply %q/%q, evac %q/%q) — "+
			"a park with no releaser is a stall wearing a queue reason",
			supply.Status, supply.QueueCause, evac.Status, evac.QueueCause)
	}
}

// ── census 5: press-index, both creation orders ─────────────────────────────

// TestPairRule_PressIndexBothOrNeitherInEitherCreationOrder runs the happy and
// park arms on press-index shapes, with the pair created in both orders a plant
// produces: R1 (the evac, which also fetches the replacement unflipped) first in
// a steady-state swap, R2 (the index leg) first at a changeover. The leader is
// whichever leg has the lower id, and both-or-neither must hold either way.
//
// The park is a dry supermarket: R1's replacement pickup has nothing to take.
//
// COVERAGE PIN. Passes at bcbde0d2. MUTATION: in dispatchPairInOnePass, dispatch
// each leg as soon as its own phases clear instead of after the loop. The
// r2-first/dry case then commits R2 before R1 is refused and fails on the
// neither-dispatched assertion.
func TestPairRule_PressIndexBothOrNeitherInEitherCreationOrder(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prefix  string
		r2First bool
		dry     bool
	}{
		{"r1 first, both source", "PR5A", false, false},
		{"r2 first, both source", "PR5B", true, false},
		{"r1 first, supermarket dry", "PR5C", false, true},
		{"r2 first, supermarket dry", "PR5D", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
			front := sd.LineNode
			back := prNode(t, db, tc.prefix+"-BACK")
			out := prNode(t, db, tc.prefix+"-OUT")
			inb := prNode(t, db, tc.prefix+"-IN")
			prResident(t, db, front, sd.BinType.ID, sd.Payload.Code, tc.prefix+"-FRONT-BIN")
			prResident(t, db, back, sd.BinType.ID, sd.Payload.Code, tc.prefix+"-BACK-BIN")
			if !tc.dry {
				testdb.CreateBinAtNode(t, db, sd.Payload.Code, inb.ID, tc.prefix+"-FRESH")
			}
			r1, r2 := tc.prefix+"-r1", tc.prefix+"-r2"
			submitR1 := func() {
				prSubmitLeg(d, r1, r2, sd.Payload.Code, front.Name,
					prWait(front.Name), prPick(front.Name), prDrop(out.Name), prPick(inb.Name), prDrop(back.Name))
			}
			submitR2 := func() {
				prSubmitLeg(d, r2, r1, sd.Payload.Code, front.Name,
					prWait(back.Name), prPick(back.Name), prDrop(front.Name))
			}
			if tc.r2First {
				submitR2()
				submitR1()
			} else {
				submitR1()
				submitR2()
			}
			// The non-leader first, so the election's no-op is exercised.
			if tc.r2First {
				prScanPass(t, d, db, r1, r2)
			} else {
				prScanPass(t, d, db, r2, r1)
			}

			a, b := prReloadUUID(t, db, r1), prReloadUUID(t, db, r2)
			if !tc.dry {
				if a.VendorOrderID == "" || b.VendorOrderID == "" {
					t.Fatalf("press-index pair did not go in one pass: R1 %s vendor=%q cause=%q, R2 %s "+
						"vendor=%q cause=%q", a.Status, a.VendorOrderID, a.QueueCause,
						b.Status, b.VendorOrderID, b.QueueCause)
				}
				return
			}
			if a.VendorOrderID != "" || b.VendorOrderID != "" {
				t.Fatalf("a press-index leg reached the fleet with the supermarket dry: R1 vendor=%q, R2 vendor=%q",
					a.VendorOrderID, b.VendorOrderID)
			}
			prAssertHoldsNothing(t, db, a, "R1 in a pair parked on a dry supermarket")
			prAssertHoldsNothing(t, db, b, "R2 in a pair parked on a dry supermarket")
			if a.QueueCause == "" || a.QueueCause != b.QueueCause {
				t.Errorf("one pair, one cause: R1 %q, R2 %q", a.QueueCause, b.QueueCause)
			}
		})
	}
}

// ── census 9: a partner goes moot inside the leader's pass ──────────────────

// TestPairRule_MootPartnerInThePassReleasesTheLeader: the supply (leader)
// acquires, then the evac comes to lift a resident that is not there — its
// reserve goes moot and it skips inside the leader's own pass. A skipped evac is
// not a death (the supply should still put a carrier back), so the supply
// survives. What it must not do is sit out the pass holding the bin and slot it
// acquired for a dispatch that did not happen.
//
// DEFECT PIN. Fails at bcbde0d2: dispatchPairInOnePass returns from the death
// arm without dispatching or releasing the leader, which keeps its claims until
// some later pass re-reads them (U0 census 9; verdigris-otter F10).
func TestPairRule_MootPartnerInThePassReleasesTheLeader(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	stage := prNode(t, db, "PR9-STAGE")
	out := prNode(t, db, "PR9-OUT")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "PR9-FRESH")
	// No resident at the line: the evac's only source is empty, so it is moot.

	prSubmitLeg(d, "pr9-supply", "pr9-evac", sd.Payload.Code, sd.LineNode.Name,
		prPick(sd.StorageNode.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
		prDrop(sd.LineNode.Name))
	prSubmitLeg(d, "pr9-evac", "pr9-supply", sd.Payload.Code, sd.LineNode.Name,
		prWait(sd.LineNode.Name), prPick(sd.LineNode.Name), prDrop(out.Name))

	prScanPass(t, d, db, "pr9-evac", "pr9-supply")

	evac := prReloadUUID(t, db, "pr9-evac")
	if evac.Status != StatusSkipped {
		t.Fatalf("fixture: evac is %q, want skipped — the moot arm did not fire, so this proves nothing "+
			"about what happens to the leader when it does", evac.Status)
	}
	supply := prReloadUUID(t, db, "pr9-supply")
	if protocol.IsTerminal(supply.Status) {
		t.Fatalf("the supply went %q with its moot evac — a skipped evac is not a death; the supply is "+
			"what puts a carrier back on the line", supply.Status)
	}
	if supply.VendorOrderID == "" {
		prAssertHoldsNothing(t, db, supply, "the leader after its partner went moot in the same pass")
	}

	// And it proceeds: the partner is terminal, so the next pass runs it solo.
	prScanPass(t, d, db, "pr9-supply")
	supply = prReloadUUID(t, db, "pr9-supply")
	if supply.VendorOrderID == "" {
		t.Fatalf("the supply never went to the fleet after its evac went moot (status %q, cause %q)",
			supply.Status, supply.QueueCause)
	}
}

// ── census 1: a pair leg pivots into its own dig ────────────────────────────

// TestPairRule_ASupplyDiggingForItsBinHoldsItsEvac: the supply's replacement is
// buried behind one blocker, and the pass that tries to admit the pair is what
// discovers it. The supply takes its own excavation (§R.91: the lane is taken in
// ITS name, it moves to `reshuffling`, the dig's first leg goes out) — a pivot,
// not a park and not a death.
//
// Two things must hold, one per pass:
//   - pass 1: the pivot keeps its mode='dig' mouth row. The lane lock is the
//     one thing keeping every other admission out of the corridor the dig's
//     first robot is already driving into.
//   - pass 2: the evac does not go to the fleet alone. An evac released while
//     its supply is still digging lifts the line's bin with no replacement
//     coming — ALN_003, 2026-06-03.
//
// DEFECT PIN. Fails at bcbde0d2 on both: dispatchPairInOnePass hands the pivoted
// leg to parkPair, whose ReleaseLanesForOrder drops the dig row, and
// coordinatedPairLegs filters a `reshuffling` partner out, so the evac's next
// pass is a one-leg slice that dispatches (U0 census 1; ochre-marten F1/F2,
// verdigris-otter F1).
//
// The rows are built directly rather than through intake: intake would resolve
// the group, find the burial itself and pivot before the pair ever formed, and
// then this would be a test of the second defect only.
func TestPairRule_ASupplyDiggingForItsBinHoldsItsEvac(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: "PR1", NumSlots: 2, NumShuffles: 1})
	d, _ := newTestDispatcherWithResolver(t, db)
	line := sc.LineNode
	stage := prNode(t, db, "PR1-STAGE")
	out := prNode(t, db, "PR1-OUT")
	prResident(t, db, line, sc.BinType.ID, sc.Payload.Code, "PR1-RESIDENT")

	supply := prLegRow(t, db, "pr1-supply", "pr1-evac", line.Name, line.Name, sc.Grp.Name, sc.Payload.Code,
		prResolved(prPick(sc.Grp.Name), prDropExcl(stage.Name), prWait(stage.Name), prPick(stage.Name),
			prDrop(line.Name)))
	evac := prLegRow(t, db, "pr1-evac", "pr1-supply", line.Name, out.Name, line.Name, sc.Payload.Code,
		prResolved(prWait(line.Name), prPick(line.Name), prDrop(out.Name)))

	_ = d.DispatchPreparedComplex(supply)

	supply = prReload(t, db, supply.ID)
	if supply.Status != StatusReshuffling {
		t.Fatalf("fixture: the supply did not pivot into its own dig (status %q, cause %q) — this test is "+
			"about what the pair rule does with a pivot, so without one it proves nothing",
			supply.Status, supply.QueueCause)
	}
	if !prDigRow(t, db, sc.Lane.ID, supply.ID) {
		t.Errorf("pass 1: the supply's dig lost its mode='dig' mouth row on %s in the same pass that took "+
			"it, with the dig's first robot already sent. LaneLock.Unlock is the one path allowed to drop a "+
			"dig claim; a pivot is not a park, and releasing its lanes opens the corridor to every other "+
			"admission while the dig is working it", sc.Lane.Name)
	}
	if e := prReload(t, db, evac.ID); e.VendorOrderID != "" {
		t.Errorf("pass 1: the evac was dispatched (%s) in the pass its supply pivoted into a dig", e.VendorOrderID)
	}

	_ = d.DispatchPreparedComplex(prReload(t, db, evac.ID))

	evac = prReload(t, db, evac.ID)
	if evac.VendorOrderID != "" {
		t.Fatalf("pass 2: the evac went to the fleet ALONE (%s) while its supply is `reshuffling`. Its "+
			"release would lift the line's bin with no replacement committed — ALN_003. A partner that is "+
			"neither acquiring, committed to the fleet, nor terminal is an incomplete pair, not a solo order",
			evac.VendorOrderID)
	}
	if evac.QueueCode != string(protocol.QueueWaitingForPartner) {
		t.Errorf("pass 2: evac parked under code %q (cause %q), want %q — it is waiting on its partner, and "+
			"the board has to say so", evac.QueueCode, evac.QueueCause, protocol.QueueWaitingForPartner)
	}
	prAssertHoldsNothing(t, db, evac, "the evac waiting on a digging supply")
}

// ── census 2: press-index, the pivoting leg as R1 and as R2 ─────────────────

// TestPairRule_PressIndexPivotingLegHoldsItsPartner is census 1 on press-index
// shapes, run twice because the flip moves the supermarket trip between the
// legs: unflipped, R1 fetches the replacement and is the leg that digs; flipped
// (IndexRobotSupplies), R2 does. R1 is created first, as a steady-state swap
// creates it, so it leads the pass either way — which means the pivot lands on
// the leader in one case and on the second leg in the other.
//
// DEFECT PIN. Fails at bcbde0d2, same two reasons as census 1.
func TestPairRule_PressIndexPivotingLegHoldsItsPartner(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		prefix  string
		flipped bool
	}{
		{"unflipped, R1 digs", "PR2A", false},
		{"flipped, R2 digs", "PR2B", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{Prefix: tc.prefix, NumSlots: 2, NumShuffles: 1})
			d, _ := newTestDispatcherWithResolver(t, db)
			front := sc.LineNode
			back := prNode(t, db, tc.prefix+"-BACK")
			out := prNode(t, db, tc.prefix+"-OUT")
			prResident(t, db, front, sc.BinType.ID, sc.Payload.Code, tc.prefix+"-FRONT-BIN")
			prResident(t, db, back, sc.BinType.ID, sc.Payload.Code, tc.prefix+"-BACK-BIN")

			var r1Steps, r2Steps []resolvedStep
			r1Delivery, r2Delivery := back.Name, front.Name
			if tc.flipped {
				r1Steps = prResolved(prWait(front.Name), prPick(front.Name), prDrop(out.Name))
				r2Steps = prResolved(prWait(back.Name), prPick(back.Name), prDrop(front.Name),
					prPick(sc.Grp.Name), prDrop(back.Name))
				r1Delivery, r2Delivery = out.Name, back.Name
			} else {
				r1Steps = prResolved(prWait(front.Name), prPick(front.Name), prDrop(out.Name),
					prPick(sc.Grp.Name), prDrop(back.Name))
				r2Steps = prResolved(prWait(back.Name), prPick(back.Name), prDrop(front.Name))
			}
			r1 := prLegRow(t, db, tc.prefix+"-r1", tc.prefix+"-r2", front.Name, r1Delivery, front.Name,
				sc.Payload.Code, r1Steps)
			r2 := prLegRow(t, db, tc.prefix+"-r2", tc.prefix+"-r1", front.Name, r2Delivery, back.Name,
				sc.Payload.Code, r2Steps)
			pivot, other := r1, r2
			if tc.flipped {
				pivot, other = r2, r1
			}

			_ = d.DispatchPreparedComplex(r1)

			pivot = prReload(t, db, pivot.ID)
			if pivot.Status != StatusReshuffling {
				t.Fatalf("fixture: %s did not pivot into its own dig (status %q, cause %q)",
					pivot.EdgeUUID, pivot.Status, pivot.QueueCause)
			}
			if !prDigRow(t, db, sc.Lane.ID, pivot.ID) {
				t.Errorf("pass 1: %s lost its mode='dig' mouth row on %s in the pass that took it",
					pivot.EdgeUUID, sc.Lane.Name)
			}
			o := prReload(t, db, other.ID)
			if o.VendorOrderID != "" {
				t.Errorf("pass 1: %s was dispatched in the pass its partner pivoted into a dig", o.EdgeUUID)
			}
			prAssertHoldsNothing(t, db, o, "the partner of a pivot, after pass 1")

			_ = d.DispatchPreparedComplex(o)

			o = prReload(t, db, other.ID)
			if o.VendorOrderID != "" {
				t.Fatalf("pass 2: %s went to the fleet ALONE (%s) while its partner is `reshuffling`",
					o.EdgeUUID, o.VendorOrderID)
			}
			if o.QueueCode != string(protocol.QueueWaitingForPartner) {
				t.Errorf("pass 2: %s parked under code %q (cause %q), want %q", o.EdgeUUID, o.QueueCode,
					o.QueueCause, protocol.QueueWaitingForPartner)
			}
		})
	}
}
