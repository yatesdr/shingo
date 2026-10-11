//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestFlatGroupStore_TheAskersOwnDeliveryIsNotACompetitor: a store resolve into a
// group whose slots stand directly under it counted every live order naming a
// slot as its delivery_node, the asker included. An order that had chosen the
// group's only free slot on an earlier pass, and asked again (asIntent's re-choice
// at acquiring), was told the group had no slot: it refused itself and waited
// under ngrp-resolve with its own slot free. The lane branch already exempted the
// asker (FindStoreSlotInLaneExcluding); the flat branches now do the same, for
// like-kind and depth-first alike.
func TestFlatGroupStore_TheAskersOwnDeliveryIsNotACompetitor(t *testing.T) {
	t.Parallel()
	for _, algo := range []string{binresolver.StoreLKND, binresolver.StoreDPTH} {
		t.Run(algo, func(t *testing.T) {
			t.Parallel()
			db := testdb.Open(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcherWithResolver(t, db)
			ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
			testutil.MustNoErr(t, err, "NGRP type")
			grp := &nodes.Node{Name: "FLATOWN-" + algo, IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
			testutil.MustNoErr(t, db.CreateNode(grp), "create the group")
			testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, binresolver.PropStoreAlgorithm, algo), "set the algorithm")
			full := &nodes.Node{Name: "FLATOWN-" + algo + "-A", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
			free := &nodes.Node{Name: "FLATOWN-" + algo + "-B", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
			testutil.MustNoErr(t, db.CreateNode(full), "create a slot")
			testutil.MustNoErr(t, db.CreateNode(free), "create a slot")
			testdb.CreateBinAtNode(t, db, sd.Payload.Code, full.ID, "FLATOWN-"+algo+"-OCC")

			asker := testdb.CreateOrder(t, db, func(o *orders.Order) {
				o.EdgeUUID = "flatown-" + algo
				o.OrderType = OrderTypeComplex
				o.PayloadCode = sd.Payload.Code
				o.DeliveryNode = free.Name // chosen on an earlier pass
				o.Status = protocol.StatusQueued
			})
			res, err := d.resolver.Resolve(grp, binresolver.ResolveModeStore, sd.Payload.Code,
				binresolver.UnknownBinType("test: no carrier stated"), digAskerFor(asker), nil)
			if err != nil || res == nil || res.Node == nil || res.Node.ID != free.ID {
				t.Fatalf("the asker that already names %s as its delivery was answered %+v (err %v): it counted "+
					"itself as the order headed there and refused its own slot", free.Name, res, err)
			}
		})
	}
}

// TestC2cPin_AGroupWithNoWaitingSpotKeepsTodaysMoment is P5 of DESIGN-C2c §9,
// C2c ruling 5: "a group with no waiting spot keeps today's moment (the choice
// at acquiring), through the same spelling. Pinned as unchanged."
//
// No spot means no lanes (a laned group cannot be unmarked, store/nodes/marks.go),
// so the population is exactly the flat groups; on the rigs that is
// SYN_PRESS_EMPTIES. This one is flat and unmarked; it declares no levels, since
// the empties-only and level fences are not what is pinned. A swap's interior
// drop names it: the drop is chosen at acquiring, the order is sent in the same
// pass with no gate wait in its plan, and the hand takes the chosen slot as
// armor at dispatch — a confirmed slot row and nodes.claimed_by — exactly as
// before C2c. C2c's paper-until-release (rulings 2 and 3) is for groups with a
// spot; it must not reach this one (DESIGN-C2c §7.5).
//
// GREEN AT BASE by reading (characterisation, not run here) and GREEN AFTER.
func TestC2cPin_AGroupWithNoWaitingSpotKeepsTodaysMoment(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	grp := &nodes.Node{Name: "C2CP5-FLAT", IsSynthetic: true, Enabled: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "create the flat group (no waiting spot)")
	occ := &nodes.Node{Name: "C2CP5-FLAT-A", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
	free := &nodes.Node{Name: "C2CP5-FLAT-B", Enabled: true, NodeTypeID: sd.StorageNode.NodeTypeID, ParentID: &grp.ID}
	testutil.MustNoErr(t, db.CreateNode(occ), "create a slot")
	testutil.MustNoErr(t, db.CreateNode(free), "create a slot")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, occ.ID, "C2CP5-OCC")
	press, press2, cell := lineNode(t, db, "C2CP5-PRESS"), lineNode(t, db, "C2CP5-PRESS2"), lineNode(t, db, "C2CP5-CELL")
	prResident(t, db, press, sd.BinType.ID, sd.Payload.Code, "C2CP5-FULL")
	testdb.CreateBinAtNode(t, db, "", cell.ID, "C2CP5-EMPTY")

	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "c2cp5", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
		Steps: []protocol.ComplexOrderStep{
			prPick(press.Name), prDrop(grp.Name),
			{Action: protocol.ActionPickup, Node: cell.Name, Empty: true}, prDrop(press2.Name),
		},
	})
	prScanPass(t, d, db, "c2cp5")
	o := prReloadUUID(t, db, "c2cp5")
	if o.VendorOrderID == "" || protocol.IsAcquiring(o.Status) {
		t.Fatalf("the swap is %s under %q, want it sent on the pass that chose its drop", o.Status, o.QueueCause)
	}
	steps, err := c2cReloadSteps(t, o)
	testutil.MustNoErr(t, err, "reload the swap's steps")
	for i, s := range steps {
		if s.Action == protocol.ActionWait && s.WaitKind == WaitKindLane {
			t.Fatalf("step %d is a lane wait (%+v): a group with no spot has nowhere to wait and no gate", i, s)
		}
	}
	if _, drop := c2cInteriorDrop(steps); drop.Node != free.Name {
		t.Fatalf("the interior drop is at %q, want the group's free child %s, chosen at acquiring", drop.Node, free.Name)
	}
	var confirmed int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM reservations WHERE order_id = $1 AND node_id = $2
		AND resource_kind = 'slot' AND state = 'confirmed'`, o.ID, free.ID).Scan(&confirmed), "count the confirmed slot row")
	n, err := db.GetNode(free.ID)
	testutil.MustNoErr(t, err, "reload the slot")
	if confirmed != 1 || n.ClaimedBy == nil || *n.ClaimedBy != o.ID {
		t.Fatalf("at dispatch %s has %d confirmed slot row(s) of order %d and claimed_by %v, want 1 and the "+
			"order: the hand takes a spotless group's slot as armor at dispatch, as before C2c",
			free.Name, confirmed, o.ID, n.ClaimedBy)
	}
}

// c2cReloadSteps reads o's plan.
func c2cReloadSteps(t *testing.T, o *orders.Order) ([]resolvedStep, error) {
	t.Helper()
	var steps []resolvedStep
	err := json.Unmarshal([]byte(o.StepsJSON), &steps)
	testutil.MustNoErr(t, err, "read the plan")
	return steps, err
}
