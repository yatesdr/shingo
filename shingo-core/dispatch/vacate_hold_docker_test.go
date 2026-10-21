//go:build docker

package dispatch

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/store"
)

// vacate_hold_docker_test.go — the vacate wait against a real database.

// vhLift is the robot's pickup as Core records it: the store's lift, the one
// statement the engine's BinService.Lift runs (bins.LiftToTransitTx).
func vhLift(t *testing.T, db *store.DB, binID int64) {
	t.Helper()
	transit, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "transit node")
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin lift")
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	_, err = db.LiftBinToTransitTx(tx, binID, transit.ID, "", time.Now().UTC())
	testutil.MustNoErr(t, err, "lift")
	testutil.MustNoErr(t, tx.Commit(), "commit lift")
}

// vhPlace is an arrival through THE ONE PLACEMENT (PlaceBinTx), which evicts a
// ghost recorded on the node and clears lifted_at — and never touches
// last_lift_at.
func vhPlace(t *testing.T, db *store.DB, binID int64, node string) {
	t.Helper()
	n, err := db.GetNodeByDotName(node)
	testutil.MustNoErr(t, err, "node "+node)
	tx, err := db.Begin()
	testutil.MustNoErr(t, err, "begin place")
	defer func(tx *sql.Tx) { _ = tx.Rollback() }(tx)
	_, err = db.PlaceBinTx(tx, store.BinPlacement{BinID: binID, ToNodeID: n.ID})
	testutil.MustNoErr(t, err, "place")
	testutil.MustNoErr(t, tx.Commit(), "commit place")
}

// binOnNode is the one bin standing on a node.
func binOnNode(t *testing.T, db *store.DB, node string) int64 {
	t.Helper()
	n, err := db.GetNodeByDotName(node)
	testutil.MustNoErr(t, err, "node "+node)
	bs, err := db.ListBinsByNode(n.ID)
	testutil.MustNoErr(t, err, "bins on "+node)
	if len(bs) != 1 {
		t.Fatalf("%s holds %d bins, want 1", node, len(bs))
	}
	return bs[0].ID
}

func (r releaseRig) appends() int { return len(r.backend.ReleaseCalls()) }

// TestStationReleaseRefused_OnVacateWait: a station release of a wait Core owns
// on the vacate fact is refused with invalid_state and appends nothing — the
// station release refusal asks coreOwnsWait, and both vacate shapes answer
// true. A refused release writes nothing of Core's (L11): wait_index and status
// stay. Control: the same plan with the stamp not held is the station's and goes.
func TestStationReleaseRefused_OnVacateWait(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	held := vsStagedLeg(t, r.db, "vrr-held", "", "VRR-B1", StatusStaged, []resolvedStep{
		voWait("VRR-LINE", WaitKindStation), vsPick("VRR-LINE"), voDrop("VRR-B1", voHeld("VRR-B1"))})
	stored := vsStagedLeg(t, r.db, "vrr-stored", "", "VRR-B2", StatusStaged, []resolvedStep{
		vsPick("VRR-SRC"), voWait("", WaitKindVacate), voDrop("VRR-B2", nil)})
	for _, uuid := range []string{held.EdgeUUID, stored.EdgeUUID} {
		got := r.outcome(t, uuid)
		if !strings.HasPrefix(got, "appends=0 wait_index=0 status=staged errors=[invalid_state]") {
			t.Errorf("%s: a station release of a vacate wait: %s, want refused with invalid_state and nothing moved",
				uuid, got)
		}
	}
	control := vsStagedLeg(t, r.db, "vrr-control", "", "VRR-B3", StatusStaged, []resolvedStep{
		voWait("VRR-LINE3", WaitKindStation), vsPick("VRR-LINE3"), voDrop("VRR-B3", nil)})
	if got := r.outcome(t, control.EdgeUUID); !strings.HasPrefix(got, "appends=1 wait_index=1") {
		t.Errorf("control: a station wait with no held stamp: %s, want it released", got)
	}
}

// vhHeldReturn stages the swap return to the vacated slot and presses its
// release while the partial still stands: Core takes the release and holds it.
// The stamp carries stamped_at, as a grant written by this tree does.
func vhHeldReturn(t *testing.T, r releaseRig, prefix string) (z int64, partial int64) {
	t.Helper()
	zo, _, _, _, p := swapAtVacatedBuffer(t, r, prefix, StatusDispatched)
	stamped := time.Now().UTC().Add(-time.Minute)
	steps, _ := decodeSteps(zo.StepsJSON)
	steps[2].Vacate.StampedAt = &stamped
	testutil.MustNoErr(t, r.db.UpdateOrderStepsJSON(zo.ID, string(mustJSON(t, steps))), "stamp time")
	if got := r.outcome(t, zo.EdgeUUID); got != "appends=0 wait_index=0 status=staged errors=[]" {
		t.Fatalf("fixture: the return's release must be taken and held: %s", got)
	}
	return zo.ID, p
}

// TestVacate_HeldAtTheTapReleasedOnTheLift: the event path. The lift of the
// stamped bin re-drives the hold that names it, and the segment goes, once.
// The station is told the leg is Core's while it is held.
func TestVacate_HeldAtTheTapReleasedOnTheLift(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, partial := vhHeldReturn(t, r, "VHE")
	o := prReload(t, r.db, z)
	steps, _ := decodeSteps(o.StepsJSON)
	if at, kind, _ := stationPoint(steps, o.WaitIndex); at != nil || kind != WaitKindVacate {
		t.Errorf("a held leg tells the station %v/%q, want no ordinal and %q", at, kind, WaitKindVacate)
	}
	if !CoreOwnsWaitAt(o.StepsJSON, o.WaitIndex) {
		t.Error("a held leg is not Core's: the hard-release button would not show")
	}
	before := r.appends()
	if got := r.d.RedriveVacateWaits(partial); len(got) != 0 {
		t.Fatalf("released %v before the lift", got)
	}
	vhLift(t, r.db, partial)
	other := makeEmptyBin(t, r.db, prNode(t, r.db, "VHE-ELSEWHERE").ID, "VHE-OTHER")
	if got := r.d.RedriveVacateWaits(other.ID); len(got) != 0 {
		t.Errorf("a lift of a bin no hold names released %v", got)
	}
	if got := r.d.RedriveVacateWaits(partial); len(got) != 1 || got[0] != z {
		t.Fatalf("the lift re-drive released %v, want [%d]", got, z)
	}
	if n := r.appends() - before; n != 1 {
		t.Errorf("%d appends, want 1", n)
	}
	if fresh := prReload(t, r.db, z); fresh.WaitIndex != 1 || fresh.Status != StatusInTransit {
		t.Errorf("after the release: wait_index %d status %s, want 1 in_transit", fresh.WaitIndex, fresh.Status)
	}
}

// TestVacate_FloorReleasesAfterTheBinLandsElsewhere: the floor reads the lift,
// not a record. The partial is lifted and set down on the staging node before
// any re-drive runs — the arrival clears lifted_at — and the 60 s floor still
// sees the lift (last_lift_at) and releases, recording that it did.
func TestVacate_FloorReleasesAfterTheBinLandsElsewhere(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, partial := vhHeldReturn(t, r, "VHF")
	vhLift(t, r.db, partial)
	vhPlace(t, r.db, partial, "VHF-STAGE")
	before := r.appends()
	r.d.SweepLaneWaiters()
	if n := r.appends() - before; n != 1 || prReload(t, r.db, z).WaitIndex != 1 {
		t.Fatalf("the floor did not release a hold whose bin was lifted and landed elsewhere: %d appends", n)
	}
	acts, err := r.db.ListRecoveryActions(50)
	testutil.MustNoErr(t, err, "recovery actions")
	recorded := false
	for _, a := range acts {
		recorded = recorded || (a.TargetID == z && strings.Contains(a.Detail, string(PopVacateHeld)))
	}
	if !recorded {
		t.Error("the floor released a hold without recording it — a release the event path should have made")
	}
}

// TestVacate_GhostEvictionDoesNotRelease: a delivery lands on the buffer while
// the stamped partial is recorded there; the arrival evicts it to _TRANSIT
// (helpers.EvictStaleGhostBinsTx) with no lift. node_id is no longer the
// buffer, and the hold must still stand: the fact is the lift.
func TestVacate_GhostEvictionDoesNotRelease(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, partial := vhHeldReturn(t, r, "VHG")
	intruder := makeEmptyBin(t, r.db, prNode(t, r.db, "VHG-ELSEWHERE").ID, "VHG-INTRUDER")
	vhPlace(t, r.db, intruder.ID, "VHG-B1")
	if b, err := r.db.GetBin(partial); err != nil || b.NodeName == "VHG-B1" {
		t.Fatalf("fixture: the partial was not evicted (%v)", err)
	}
	before := r.appends()
	r.d.RedriveVacateWaits(partial)
	r.d.SweepLaneWaiters()
	if n := r.appends() - before; n != 0 || prReload(t, r.db, z).WaitIndex != 0 {
		t.Fatalf("a ghost eviction released the hold: %d appends", n)
	}
}

// TestVacate_LiftThenReturnDoesNotRelease: the bin is lifted and set back down
// on the node it was to leave. Lifted since the stamp, but standing there.
func TestVacate_LiftThenReturnDoesNotRelease(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, partial := vhHeldReturn(t, r, "VHR")
	vhLift(t, r.db, partial)
	vhPlace(t, r.db, partial, "VHR-B1")
	before := r.appends()
	r.d.SweepLaneWaiters()
	if n := r.appends() - before; n != 0 || prReload(t, r.db, z).WaitIndex != 0 {
		t.Fatalf("a bin lifted and returned to the node released the hold: %d appends", n)
	}
}

// TestVacate_RestartReplayNoDoubleNoFalseRelease: a Core restart forgets the
// poller's block memory and re-fires FINISHED blocks (rds/poller.go). The lift
// event fires again for a bin already at _TRANSIT: the lift writes nothing
// (Moved false, last_lift_at untouched) and the re-drive appends nothing a
// second time (ClaimAppend's CAS on wait_index). And a replay that names the
// stamped bin while it was never lifted — evicted as a ghost, so already at
// _TRANSIT — releases nothing.
func TestVacate_RestartReplayNoDoubleNoFalseRelease(t *testing.T) {
	t.Parallel()
	t.Run("no double release", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		z, partial := vhHeldReturn(t, r, "VHD")
		vhLift(t, r.db, partial)
		r.d.RedriveVacateWaits(partial)
		after := r.appends()
		vhLift(t, r.db, partial) // the replayed pickup: already at _TRANSIT
		r.d.RedriveVacateWaits(partial)
		r.d.SweepLaneWaiters()
		if n := r.appends() - after; n != 0 || prReload(t, r.db, z).WaitIndex != 1 {
			t.Fatalf("a replayed lift appended again: %d appends", n)
		}
	})
	t.Run("no false release", func(t *testing.T) {
		t.Parallel()
		r := newReleaseRig(t)
		z, partial := vhHeldReturn(t, r, "VHN")
		intruder := makeEmptyBin(t, r.db, prNode(t, r.db, "VHN-ELSEWHERE").ID, "VHN-INTRUDER")
		vhPlace(t, r.db, intruder.ID, "VHN-B1") // the partial goes to _TRANSIT, unlifted
		before := r.appends()
		vhLift(t, r.db, partial) // the replay: already at _TRANSIT, writes nothing
		r.d.RedriveVacateWaits(partial)
		if n := r.appends() - before; n != 0 || prReload(t, r.db, z).WaitIndex != 0 {
			t.Fatalf("a replayed event for a bin never lifted released the hold: %d appends", n)
		}
	})
}

// vhHeldSibling presses the supply (Y) first while the return (Z) is staged
// with its grant: Y is taken and held on Z's bin on the line.
func vhHeldSibling(t *testing.T, r releaseRig, prefix string) (z, y, resident int64) {
	t.Helper()
	zo, yo, _, _, _ := swapAtVacatedBuffer(t, r, prefix, StatusStaged)
	if got := r.outcome(t, yo.EdgeUUID); got != "appends=0 wait_index=0 status=staged errors=[]" {
		t.Fatalf("fixture: the supply's release must be taken and held on the sibling stamp: %s", got)
	}
	steps, _ := decodeSteps(prReload(t, r.db, yo.ID).StepsJSON)
	st := steps[len(steps)-1].Vacate
	resident = binOnNode(t, r.db, prefix+"-LINE")
	if st == nil || st.Kind != "sibling" || st.Bin != resident || st.Partner != zo.ID || st.HeldAt == nil || st.StampedAt == nil {
		t.Fatalf("fixture: the supply's line drop carries %+v, want a held sibling stamp on bin %d partner %d",
			st, resident, zo.ID)
	}
	return zo.ID, yo.ID, resident
}

// TestVacate_MootPartnerReleasesTheSupply (STOP 5, ruled 2026-10-18): the
// return the supply is held on ends skipped — it found no bin to lift — and the
// supply's hold is released, as the death rule leaves the supply alive.
func TestVacate_MootPartnerReleasesTheSupply(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, y, _ := vhHeldSibling(t, r, "VHM")
	// The return is the leg that takes the line's bin (legTakesLineBin reads it
	// off the plan against the line, the order's process node).
	mustExecDispatch(t, r.db, `UPDATE orders SET process_node=$1 WHERE id=$2`, "VHM-LINE", z)
	testdb_SeedStatus(t, r.db, z, StatusSkipped)
	before := r.appends()
	r.d.RedriveVacateWaits(0)
	if n := r.appends() - before; n != 1 || prReload(t, r.db, y).WaitIndex != 1 {
		t.Fatalf("the supply was not released on its partner's moot end: %d appends", n)
	}
}

// TestVacate_CancelledPartnerDoesNotRelease (STOP 5): a return cancelled with
// the line's bin still on it does not release the supply. The row says the
// lifter is gone, and the death rule takes the supply down; it is never
// appended onto a line still holding the bin.
func TestVacate_CancelledPartnerDoesNotRelease(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, y, _ := vhHeldSibling(t, r, "VHX")
	testdb_SeedStatus(t, r.db, z, StatusCancelled)
	before := r.appends()
	r.d.RedriveVacateWaits(0)
	if n := r.appends() - before; n != 0 {
		t.Fatalf("a cancelled partner released the supply onto a line still holding its bin: %d appends", n)
	}
	if o := prReload(t, r.db, y); QueueCause(o.QueueCause) != CauseVacateLifterGone ||
		!strings.Contains(o.QueueReason, "has ended") {
		t.Errorf("the row says cause %q reason %q, want %q", o.QueueCause, o.QueueReason, CauseVacateLifterGone)
	}
	r.d.HandleSwapPeerTerminal(z, SwapTerminalCancelled)
	if o := prReload(t, r.db, y); !protocol.IsTerminal(o.Status) {
		t.Errorf("the death rule left the held supply %s", o.Status)
	}
	r.d.RedriveVacateWaits(0)
	r.d.SweepLaneWaiters()
	if n := r.appends() - before; n != 0 {
		t.Errorf("the supply was appended after its partner's cancel: %d appends", n)
	}
}

// TestVacate_HardReleaseOverridesAHold: the Core hard release is the hatch for
// a hold whose lift will not come, and it says what it bypassed.
func TestVacate_HardReleaseOverridesAHold(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	z, _ := vhHeldReturn(t, r, "VHH")
	before := r.appends()
	if err := r.d.HardReleaseStagedOrder(z, "test-engineer"); err != nil {
		t.Fatalf("hard release of a held leg: %v", err)
	}
	if n := r.appends() - before; n != 1 {
		t.Errorf("%d appends, want 1", n)
	}
}

// TestVacate_StoredWaitFenceHoldsBothWays is TestSplice_FenceHoldsOnASplicedPlan's
// vacate direction, on a plan of the shape Core will author for P2's children
// ([pickup, wait(vacate), drop{stamp}]; no splice produces one). The station
// must not advance it; the re-drive does, once the stamped bin has lifted.
func TestVacate_StoredWaitFenceHoldsBothWays(t *testing.T) {
	t.Parallel()
	r := newReleaseRig(t)
	home := prNode(t, r.db, "VHS-HOME")
	empty := makeEmptyBin(t, r.db, home.ID, "VHS-EMPTY")
	stamped := time.Now().UTC().Add(-time.Minute)
	o := vsStagedLeg(t, r.db, "vhs-move", "", home.Name, StatusStaged, []resolvedStep{
		vsPick("VHS-SRC"), voWait("", WaitKindVacate),
		voDrop(home.Name, &vacateStamp{Node: home.Name, Bin: empty.ID, StampedAt: &stamped})})
	if got := r.outcome(t, o.EdgeUUID); !strings.HasPrefix(got, "appends=0 wait_index=0 status=staged errors=[invalid_state]") {
		t.Fatalf("a station released a stored vacate wait: %s", got)
	}
	before := r.appends()
	vhLift(t, r.db, empty.ID)
	r.d.RedriveVacateWaits(empty.ID)
	if n := r.appends() - before; n != 1 || prReload(t, r.db, o.ID).WaitIndex != 1 {
		t.Fatalf("the re-drive did not release the stored vacate wait on the lift: %d appends", n)
	}
}

// testdb_SeedStatus writes a terminal status straight onto a row, as the
// partner's own end would have left it.
func testdb_SeedStatus(t *testing.T, db *store.DB, id int64, st protocol.Status) {
	t.Helper()
	mustExecDispatch(t, db, `UPDATE orders SET status=$1 WHERE id=$2`, string(st), id)
}
