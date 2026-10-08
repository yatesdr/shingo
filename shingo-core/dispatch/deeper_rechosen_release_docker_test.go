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

// TestLaneGate_ADeeperStoreReChosenOutOfTheLaneReleasesTheShallowerDweller:
// lane-deeper-pending has two endings, not one. The deeper store places its
// bin, or (never dispatched) it gives up the slot row that made it a wall: its
// choice was re-made into another lane and the allocator's reconcile released
// the old slot. That release woke nobody, so the dweller waited for the floor
// (packed 477 on 297290ba: order 481's drop re-chosen from LSC_036 to another
// lane; 477 was freed by the floor 14 s later). A never-dispatched store walls a
// lane only through that row (stillComingToLane), so the row's release is the
// releaser (wakeLanesOfSlots), not the delivery_node write.
func TestLaneGate_ADeeperStoreReChosenOutOfTheLaneReleasesTheShallowerDweller(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewSuccessBackend()
	d, _ := newTestDispatcher(t, db, backend)

	groupID, laneID, s0 := gatedLane(t, db, "DPMOVE", "")
	var s1 *nodes.Node
	slots, err := db.ListLaneSlots(laneID)
	testutil.MustNoErr(t, err, "list slots")
	for _, s := range slots {
		if s.ID != s0.ID {
			s1 = s
		}
	}
	grp, err := db.GetNode(groupID)
	testutil.MustNoErr(t, err, "read the group")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	other := &nodes.Node{Name: "DPMOVE-LANE2", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: &groupID}
	testutil.MustNoErr(t, db.CreateNode(other), "create the other lane")
	d0 := 0
	elsewhere := &nodes.Node{Name: "DPMOVE-L2-S0", Enabled: true, ParentID: &other.ID, Depth: &d0}
	testutil.MustNoErr(t, db.CreateNode(elsewhere), "create the other lane's slot")
	line := lineNode(t, db, "DPMOVE-LINE")

	// The deeper store: queued, not dispatched, its drop chosen into this lane
	// and its slot held there.
	deep := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "dpmove-deep"
		o.OrderType = OrderTypeComplex
		o.Coordinated = true
		o.DeliveryNode = s1.Name
		o.Status = protocol.StatusQueued
	})
	testutil.MustNoErr(t, db.ReserveSlot(s1.ID, deep.ID), "the deeper store holds its slot")
	shallow := stageGatedStore(t, db, d, line, s0, nil)
	markStaged(t, db, shallow.ID)
	d.EvaluateLaneReleases(laneID)
	held, err := db.GetOrder(shallow.ID)
	testutil.MustNoErr(t, err, "reload the dweller")
	if !IsGateStaged(held) || held.QueueCause != string(CauseLaneDeeperPending) {
		t.Fatalf("fixture: the dweller is staged=%v under %q, want it held behind the deeper store (%q)",
			IsGateStaged(held), held.QueueCause, CauseLaneDeeperPending)
	}
	if n := len(backend.ReleaseCalls()); n != 0 {
		t.Fatalf("fixture: %d append(s) while the deeper store is still coming, want 0", n)
	}

	// Acquiring chooses the deeper store's drop again, in the other lane, and the
	// allocator reconciles its slot rows against the plan: the old slot goes.
	moved := []resolvedStep{{Action: protocol.ActionDropoff, Node: elsewhere.Name, Group: grp.Name, ExclusiveSlot: true}}
	d.persistWidenedPlan(deep, moved)
	if _, err := d.allocator.reserveComplexSlots(deep, moved); err != nil {
		t.Fatalf("reconcile the deeper store's slots: %v", err)
	}

	if n := len(backend.ReleaseCalls()); n != 1 {
		gotRaw, err := db.GetOrder(shallow.ID)
		got := testutil.Must(t, gotRaw, err, "db.GetOrder(shallow.ID)")
		t.Fatalf("%d append(s) after the deeper store's drop moved out of the lane, want the dweller released "+
			"(staged=%v, cause %q). Nothing woke the lane it left, so only the floor would free it",
			n, IsGateStaged(got), got.QueueCause)
	}
}
