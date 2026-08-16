//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// lane_wait_scanner_loops_docker_test.go — the two per-tick loops learn that not
// every wait is the operator's.
//
// Both loops predate lane gating and both keyed on "is this step a wait", which
// was a complete question while every wait was a station's. The splice inserts
// Core-owned waits into plans that previously had none, so both had to learn the
// difference or silently change behaviour on the plans the splice produces.

// TestReResolve_LaneWaitIsExemptEvenWhenItsNameCollides is the sharp half.
//
// A gate point is an RDS map point, not a Core node, so the lookup in
// reResolveComplexSteps cannot find it and a lane wait would ride the "node
// vanished — unrecoverable" arm. That reaches the right OUTCOME by a branch that
// means something else, which is worth fixing on its own; but it also leaves a
// real hazard, which is what this pins.
//
// Nothing stops a plant naming a gate point the same as an existing node — the
// property is free text, set through a generic endpoint. If that name happens to
// be an NGRP, the old code would look it up, find a synthetic group, and
// RE-RESOLVE the wait into a concrete storage slot: the robot's dwell point
// silently becomes a lane slot, and the gate stops being a gate.
//
// MUTATION (verified): remove the WaitKind exemption. This test fires — the
// wait's node comes back rewritten to a storage slot.
func TestReResolve_LaneWaitIsExemptEvenWhenItsNameCollides(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// An NGRP whose name is ALSO what somebody configured as a gate point.
	ngrpType, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	if err != nil {
		t.Fatalf("get NGRP type: %v", err)
	}
	grp := &nodes.Node{Name: "COLLIDE-GRP", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrpType.ID}
	if err := db.CreateNode(grp); err != nil {
		t.Fatalf("create group: %v", err)
	}
	child := &nodes.Node{Name: "COLLIDE-SLOT", Enabled: true, ParentID: &grp.ID}
	if err := db.CreateNode(child); err != nil {
		t.Fatalf("create child: %v", err)
	}

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: sd.StorageNode.Name},
		{Action: protocol.ActionWait, Node: grp.Name, WaitKind: WaitKindLane, WaitLane: 41},
		{Action: protocol.ActionDropoff, Node: sd.LineNode.Name},
	}
	out, _, err := d.reResolveComplexSteps(steps, sd.Payload.Code, reservations.Anyone)
	if err != nil {
		t.Fatalf("reResolveComplexSteps: %v", err)
	}
	if out[1].Node != grp.Name {
		t.Fatalf("the lane wait's node was re-resolved from %q to %q. A gate point is an RDS map "+
			"point, not a Core node — re-resolving it moves the robot's dwell point into the lane "+
			"and the gate stops gating", grp.Name, out[1].Node)
	}
	if out[1].WaitKind != WaitKindLane || out[1].WaitLane != 41 {
		t.Errorf("the wait lost its stamp in re-resolution: %+v", out[1])
	}
}

// TestReResolve_OperatorWaitStillPassesThrough is the narrowness half: the
// exemption is for LANE waits, and an ordinary bare or noded operator wait keeps
// whatever behaviour it had.
func TestReResolve_OperatorWaitStillPassesThrough(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: sd.StorageNode.Name},
		{Action: protocol.ActionWait},                         // bare
		{Action: protocol.ActionWait, Node: sd.LineNode.Name}, // drive-to-and-hold
		{Action: protocol.ActionDropoff, Node: sd.LineNode.Name},
	}
	out, changed, err := d.reResolveComplexSteps(steps, sd.Payload.Code, reservations.Anyone)
	if err != nil {
		t.Fatalf("reResolveComplexSteps: %v", err)
	}
	if changed {
		t.Error("a plan of concrete nodes reported changed; nothing here references an NGRP")
	}
	for i := range steps {
		if out[i].Action != steps[i].Action || out[i].Node != steps[i].Node {
			t.Errorf("step %d changed: %+v -> %+v", i, steps[i], out[i])
		}
	}
}

// TestWiden_DoesNotStopAtALaneWait is the other loop.
//
// widenSupplyPickups re-anchors every full-material supply pickup through the
// node-local finder each tick, and it STOPS at the first wait because post-wait
// steps execute against a future world state an operator is about to create.
// A lane wait is not that kind of boundary — Core is deciding when a corridor is
// free, and the pools on both sides of it are the same pools.
//
// So a supply pickup AFTER a lane wait must still be examined. Without this the
// splice would silently un-widen every such pickup, which is the SPR-74379
// failure this function exists to fix.
//
// WHAT IS OBSERVED, and why it is a dry pool rather than a successful rewrite:
// "the loop reached the pickup" is the property under test, and a DRY anchor
// makes it unambiguous — widening a dry supply need returns a hold, and a loop
// that broke at the wait returns none. A successful rewrite would test the
// finder's pool logic as well, which is not what changed here.
//
// MUTATION (verified): drop the `&& step.WaitKind != WaitKindLane` term. The
// loop breaks at the lane wait, the pickup is never examined, and hold comes
// back nil.
func TestWiden_DoesNotStopAtALaneWait(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// A concrete supply anchor with NO bin of the payload: a dry pool.
	dry := &nodes.Node{Name: "WIDEN-DRY", Enabled: true}
	if err := db.CreateNode(dry); err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "widen-lane-wait"
		o.StationID = "line-1"
		o.PayloadCode = sd.Payload.Code
		o.ProcessNode = sd.LineNode.Name
		o.DeliveryNode = sd.LineNode.Name
	})

	// The supply pickup sits AFTER a lane wait.
	steps := []resolvedStep{
		{Action: protocol.ActionWait, Node: "WIDEN-GATE", WaitKind: WaitKindLane, WaitLane: 5},
		{Action: protocol.ActionPickup, Node: dry.Name},
		{Action: protocol.ActionDropoff, Node: sd.LineNode.Name},
	}
	_, _, hold := d.widenSupplyPickups(order, steps)
	if hold == nil {
		t.Fatal("the supply pickup after a LANE wait was never examined — widening broke at the " +
			"wait. A lane wait separates two states of one corridor, not two states of the world: " +
			"the pools on both sides are the same, and stopping here re-opens SPR-74379 on every " +
			"spliced plan")
	}
}

// TestWiden_StillStopsAtAnOperatorWait is the narrowness assertion: the boundary
// that exists for a real reason is untouched.
//
// Same dry anchor, same position — only the wait's kind differs. A station's wait
// really does separate two worlds, so the steps behind it must not be judged
// against pools that have not been filled yet.
func TestWiden_StillStopsAtAnOperatorWait(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	dry := &nodes.Node{Name: "WIDEN2-DRY", Enabled: true}
	if err := db.CreateNode(dry); err != nil {
		t.Fatalf("create anchor: %v", err)
	}
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "widen-operator-wait"
		o.StationID = "line-1"
		o.PayloadCode = sd.Payload.Code
		o.ProcessNode = sd.LineNode.Name
		o.DeliveryNode = sd.LineNode.Name
	})

	steps := []resolvedStep{
		{Action: protocol.ActionWait}, // the operator's, bare
		{Action: protocol.ActionPickup, Node: dry.Name},
		{Action: protocol.ActionDropoff, Node: sd.LineNode.Name},
	}
	_, _, hold := d.widenSupplyPickups(order, steps)
	if hold != nil {
		t.Fatalf("a supply pickup after an OPERATOR wait was examined and held (%+v). Those steps "+
			"execute against a world the station has not created yet; judging them against current "+
			"pools parks orders whose conditions resolve mid-flight", hold)
	}
}
