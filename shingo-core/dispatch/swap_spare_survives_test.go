//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"

	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// THE SPARE MUST OUTLIVE THE PASS THAT RECORDS IT.
//
// peerIsParkedWaitingForMaterial spares a supply parked on a dry source when its
// evac dies, so the operator can restock and the pair resumes. It exists because
// of Springfield 2026-07-21 (74577-6SA0A.06, zero system stock): without it the
// changeover re-armed, the planner recreated the pair, the supply parked, the
// evac died, and the handler cancelled it again — hundreds of doomed swaps per
// changeover.
//
// The chain that defeated it fits inside two scanner passes, and every step is
// something the tree does on purpose:
//
//	pass 1  applySwapGates re-runs the peer-terminal unwind from the surviving
//	        side (the SPR 2424/2425 race close). peerIsParkedWaitingForMaterial
//	        is true — the supply still carries waiting_for_material — so it is
//	        spared. Correct.
//	        The same call then reaches swapLegHoldVerdict, whose Face 2
//	        dead-clearer arm holds the supply because its clearer died without
//	        clearing the line. Also correct.
//	        setQueueReason then writes QueueWaitingForPartner over the supply's
//	        queue code.
//	pass 2  the unwind runs again. peerIsParkedWaitingForMaterial now reads
//	        waiting_for_partner, returns false, and the supply falls through to
//	        resolveSwapPeer and is CANCELLED.
//
// So the spare survived exactly one pass and the churn it was built to stop can
// recur. The law it breaks is one writer per fact: "this leg is spared" is a
// decision owned by the peer-terminal handler, recorded in a field the verdict
// pass owns.
//
// TestSwapPeerTerminal_EvacFails_LeavesParkedSupply pins pass 1 and passes
// today. This pins the pass after it.
func TestSwapSpare_SurvivesTheVerdictThatOverwritesItsQueueCode(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, bp := setupTestData(t, db)
	away := &nodes.Node{Name: "SWAP-STORE-SPARE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(away), "create store node")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	// The famine pair: a supply parked on a dry source — sourcing +
	// waiting_for_material, exactly what widenSupplyPickups leaves behind when the
	// payload has no bins — and an evac that has genuinely died.
	mkSwapLeg(t, db, "supSpare", "evacSpare", protocol.StatusSourcing, lineNode.Name, lineNode.Name, bp.Code)
	supplyID := mustOrderID(t, db, "supSpare")
	testutil.MustNoErr(t, orders.SetQueueDetail(db.DB, supplyID,
		"Waiting for material: "+bp.Code, string(protocol.QueueWaitingForMaterial), "finder-node-empty"),
		"park supply waiting_for_material")
	mkSwapLeg(t, db, "evacSpare", "supSpare", StatusFailed, lineNode.Name, away.Name, bp.Code)

	// The supply's own steps, as the scanner would resolve them: fetch a fresh
	// bin, set it on the line. placesLine && !takesLine is what puts it in front
	// of Face 2.
	supplySteps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SWAP-SRC"},
		{Action: protocol.ActionDropoff, Node: lineNode.Name},
	}

	pass := func(n int) *orders.Order {
		t.Helper()
		cur, err := db.GetOrderByUUID("supSpare")
		testutil.MustNoErr(t, err, "reload supply")
		if cur == nil {
			t.Fatalf("pass %d: supply row is gone", n)
		}
		d.applySwapGates(cur, supplySteps)
		got, err := db.GetOrderByUUID("supSpare")
		testutil.MustNoErr(t, err, "reload supply after pass")
		t.Logf("after pass %d: status=%q queue_code=%q cause=%q", n, got.Status, got.QueueCode, got.QueueCause)
		return got
	}

	pass(1)

	// THE ASSERTION. Two passes later the operator's restock must still have
	// something to resume.
	got := pass(2)
	if !protocol.IsAcquiring(got.Status) {
		t.Fatalf("after two scanner passes the parked supply is %q — the spare survived one pass and then "+
			"cancelled the wait it was built to protect. That is the Springfield 2026-07-21 re-arm churn "+
			"coming back: the monitor sees the cancel move the in-loop UOP, re-arms the changeover, and the "+
			"pair is recreated to die again", got.Status)
	}
	if got.QueueCode != string(protocol.QueueWaitingForMaterial) {
		t.Errorf("parked supply queue_code = %q, want waiting_for_material — the leg is still parked on a dry "+
			"source and that is what the board must say. waiting_for_partner names a partner that is dead and "+
			"is not what the operator can act on", got.QueueCode)
	}

	// And it must keep saying so — a spare that has to be re-earned every pass is
	// the defect, not the fix.
	got = pass(3)
	if !protocol.IsAcquiring(got.Status) {
		t.Fatalf("parked supply cancelled on the third pass (%q) — the spare is not durable", got.Status)
	}
}
