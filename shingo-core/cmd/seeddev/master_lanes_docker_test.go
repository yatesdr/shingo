//go:build docker

package main

import (
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/plantspec"
	"shingocore/store/orders"
)

// master_lanes_docker_test.go — the lanes family: the lane-stress rig's
// certified properties, asserted on the master's [corridor] (SYN_STAMP and
// SYN_COMP, the 4x2 cell's market), seeded by seedCore and walked on the
// simulator with no compose stack. The rig's runs certified figures on its own
// plant (orders per run, the closing run's 113); these assert what those
// figures stood for, which holds on any layout:
//
//	THE MOUTH GATE  every corridor lane is gated by its zone's wait points: a
//	                dig holds its lane's mouth, and each leg waits at one of the
//	                zone's spots before it enters.
//	THE DIG WALK    FIFO to a part buried at depth 5 plans a dig of the four
//	                blockers, the legs take turns, and the buried part lands.
//	NO AIR BUBBLES  after the walk, no lane of either corridor zone has an empty
//	                slot with a cart in front of it (the census, over live carts).
//	TWO DIGS        two four-blocker digs at once in one group both walk: the
//	                headroom is two lanes' worth (the closing run of 2026-08-13
//	                deadlocked with one).
//
// The rig's engine is started and stopped, as the kanban family's is, so every
// step is a synchronous call from the test.

const (
	corridorStamp = "SYN_STAMP"
	corridorComp  = "SYN_COMP"
)

// rigWindows are the kanban press's unloader windows, the snapshot the rig watches.
var rigWindows = []string{"ULN_021", "ULN_022", "ULN_023", "ULN_024"}

// masterPlant loads plants/master.yaml, validated.
func masterPlant(t *testing.T) *plantspec.Plant {
	t.Helper()
	p, err := plantspec.Load("../../../plants/master.yaml")
	testutil.MustNoErr(t, err, "load plants/master.yaml")
	testutil.MustNoErr(t, p.Validate(), "validate plants/master.yaml")
	return p
}

// newMasterRig seeds the master into a fresh database with a stopped engine on
// the simulator: the kanban family's rig, on the whole plant.
func newMasterRig(t *testing.T) (*kanbanRig, *plantspec.Plant) {
	t.Helper()
	p := masterPlant(t)
	return newKanbanRigOf(t, p, rigWindows), p
}

// liveBubbles is the census of the corridor zones over where the carts stand now.
func liveBubbles(t *testing.T, r *kanbanRig, p *plantspec.Plant) []plantspec.LaneBubble {
	t.Helper()
	live := *p
	live.Zones = nil
	for _, z := range p.Zones {
		if z.Name == corridorStamp || z.Name == corridorComp {
			live.Zones = append(live.Zones, z)
		}
	}
	live.Bins = nil
	rows, err := r.db.Query(`SELECT b.label, n.name FROM bins b JOIN nodes n ON n.id = b.node_id
		JOIN nodes l ON l.id = n.parent_id JOIN nodes g ON g.id = l.parent_id WHERE g.name IN ($1, $2)`,
		corridorStamp, corridorComp)
	testutil.MustNoErr(t, err, "corridor carts")
	defer rows.Close()
	for rows.Next() {
		var b plantspec.Bin
		testutil.MustNoErr(t, rows.Scan(&b.Name, &b.Slot), "scan")
		live.Bins = append(live.Bins, b)
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	return live.CensusAtBirth().Bubbles
}

// digFor sends the Edge's retrieve of a part to a stage, through the wire door,
// and returns the parent and its legs, oldest first.
func digFor(t *testing.T, r *kanbanRig, part, group, stage string) (*orders.Order, []*orders.Order) {
	t.Helper()
	parent := r.order(protocol.OrderRequest{OrderType: protocol.OrderTypeRetrieve, PayloadCode: part,
		SourceNode: group, DeliveryNode: stage})
	var legs []*orders.Order
	for _, o := range r.allOrders() {
		if o.ParentOrderID != nil && *o.ParentOrderID == parent.ID {
			legs = append(legs, o)
		}
	}
	return parent, legs
}

// allOrders is every order, oldest first.
func (r *kanbanRig) allOrders() []*orders.Order {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT id FROM orders ORDER BY id`)
	testutil.MustNoErr(r.t, err, "orders")
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		testutil.MustNoErr(r.t, rows.Scan(&id), "scan")
		ids = append(ids, id)
	}
	testutil.MustNoErr(r.t, rows.Err(), "rows")
	out := make([]*orders.Order, 0, len(ids))
	for _, id := range ids {
		o, err := r.db.GetOrder(id)
		testutil.MustNoErr(r.t, err, "order")
		out = append(out, o)
	}
	return out
}

// reservationsOf is an order's live reservations, as kind:state:name.
func (r *kanbanRig) reservationsOf(id int64) []string {
	r.t.Helper()
	rows, err := r.db.Query(`SELECT r.resource_kind || ':' || r.state || ':' ||
		COALESCE((SELECT label FROM bins WHERE id = r.bin_id), (SELECT name FROM nodes WHERE id = r.node_id), '')
		FROM reservations r WHERE r.order_id = $1 AND r.state <> 'released' ORDER BY 1`, id)
	testutil.MustNoErr(r.t, err, "reservations")
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		testutil.MustNoErr(r.t, rows.Scan(&s), "scan")
		out = append(out, s)
	}
	return out
}

// TestLanesFamily_AtBirthTheCorridorIsPackedAndGated: the census over the seeded
// carts finds no bubble, and every corridor lane sits under its zone's spots.
func TestLanesFamily_AtBirthTheCorridorIsPackedAndGated(t *testing.T) {
	t.Parallel()
	r, p := newMasterRig(t)
	if b := liveBubbles(t, r, p); len(b) != 0 {
		t.Errorf("the corridor is born with %d air bubble(s): %+v", len(b), b)
	}
	for _, zone := range []string{corridorStamp, corridorComp} {
		var points string
		testutil.MustNoErr(t, r.db.QueryRow(`SELECT COALESCE(p.value, '') FROM nodes n
			LEFT JOIN node_properties p ON p.node_id = n.id AND p.key = 'group_wait_points' WHERE n.name = $1`, zone).Scan(&points),
			"wait points of "+zone)
		if points == "" {
			t.Errorf("%s carries no group_wait_points: its lanes are ungated", zone)
		}
	}
}

// TestLanesFamily_TheDigWalksThroughTheGateAndLeavesNoBubble: the oldest
// COMMON-1 is at the back of LS_D1, depth 5. The retrieve plans a dig of the four
// blockers in front of it, holds LS_D1's mouth, sends each leg through one of
// SYN_STAMP's spots, walks to the end with the buried cart at WELD-6's stage,
// and leaves no bubble anywhere in the corridor.
func TestLanesFamily_TheDigWalksThroughTheGateAndLeavesNoBubble(t *testing.T) {
	t.Parallel()
	r, p := newMasterRig(t)
	target := r.cartAt("LSD_005")
	parent, legs := digFor(t, r, "COMMON-1", corridorStamp, "SLN_061")
	if parent.Status != protocol.StatusReshuffling {
		t.Fatalf("the retrieve is %s (%s), want a dig in progress", parent.Status, parent.QueueCause)
	}
	if len(legs) != 5 {
		t.Fatalf("the dig has %d legs, want the four blockers and the buried cart: %s", len(legs), r.said(legs))
	}
	if got := strings.Join(r.reservationsOf(parent.ID), ","); !strings.Contains(got, "mouth:confirmed:LS_D1") {
		t.Errorf("the dig holds %s, want LS_D1's mouth", got)
	}
	var spots []string
	for _, z := range p.Zones {
		if z.Name == corridorStamp {
			spots = z.WaitPoints
		}
	}
	first := legs[0]
	if !strings.Contains(first.StepsJSON, `"wait"`) || !containsAny(first.StepsJSON, spots) {
		t.Errorf("the first leg's plan %s waits at none of %s's spots %v", first.StepsJSON, corridorStamp, spots)
	}
	for _, leg := range legs[1:] {
		if leg.Status != protocol.StatusPending || leg.QueueCause != "dig-leg-turn" {
			t.Errorf("leg %d is %s (%s), want pending its turn", leg.ID, leg.Status, leg.QueueCause)
		}
	}

	r.walk()
	if left := r.live(); len(left) != 0 {
		t.Fatalf("the dig did not walk to its end: %s", r.said(left))
	}
	if got := r.cartAt("SLN_061"); got != target {
		t.Errorf("SLN_061 holds cart %d, want the buried COMMON-1 %d", got, target)
	}
	if got, err := r.db.GetOrder(parent.ID); err != nil || got.Status != protocol.StatusConfirmed && got.Status != protocol.StatusDelivered {
		t.Errorf("the retrieve ended %v (%v), want delivered", got.Status, err)
	}
	if b := liveBubbles(t, r, p); len(b) != 0 {
		t.Errorf("the walk left %d air bubble(s): %+v", len(b), b)
	}
}

// TestLanesFamily_TwoDigsInOneGroupBothWalk: the two oldest COMMON-1 are each at
// the back of a depth-5 lane, so two retrieves are two four-blocker digs in
// SYN_STAMP at once. Both land and the corridor stays packed.
func TestLanesFamily_TwoDigsInOneGroupBothWalk(t *testing.T) {
	t.Parallel()
	r, p := newMasterRig(t)
	first, second := r.cartAt("LSD_005"), r.cartAt("LSD_015")
	a, _ := digFor(t, r, "COMMON-1", corridorStamp, "SLN_061")
	b, _ := digFor(t, r, "COMMON-1", corridorStamp, "SLN_071")
	for _, o := range []*orders.Order{a, b} {
		if o.Status != protocol.StatusReshuffling && o.Status != protocol.StatusQueued {
			t.Fatalf("retrieve %d is %s (%s), want a dig or its turn for one", o.ID, o.Status, o.QueueCause)
		}
	}
	r.walk()
	if left := r.live(); len(left) != 0 {
		t.Fatalf("two digs in one group did not both walk: %s", r.said(left))
	}
	landed := map[int64]bool{r.cartAt("SLN_061"): true, r.cartAt("SLN_071"): true}
	if !landed[first] || !landed[second] {
		t.Errorf("the stages hold %v, want the two buried COMMON-1 %d and %d", landed, first, second)
	}
	if bs := liveBubbles(t, r, p); len(bs) != 0 {
		t.Errorf("the two walks left %d air bubble(s): %+v", len(bs), bs)
	}
}

func containsAny(s string, subs []string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// walk runs every live order the way the fleet runs a mission, waits included:
// a robot reaches each wait and reports WAITING (Core's gate decides and appends
// the next blocks), completes each block Core has given it, and reports FINISHED
// once its mission is complete; the Edge confirms each delivery. It ends when a
// pass moves nothing. fly is the same without the waits, for ungated work.
func (r *kanbanRig) walk() {
	r.t.Helper()
	done := map[string]bool{} // vendor id | block id completed
	for pass := 0; pass < 60; pass++ {
		r.eng.RunFulfillmentScan()
		moved := false
		for _, o := range r.live() {
			if o.Status == protocol.StatusDelivered {
				r.eng.Dispatcher().HandleOrderReceipt(rigEnvelope(o.StationID),
					&protocol.OrderReceipt{OrderUUID: o.EdgeUUID, ReceiptType: "confirmed", FinalCount: 1})
				moved = true
				continue
			}
			if o.VendorOrderID == "" || !r.sim.HasOrder(o.VendorOrderID) {
				continue
			}
			v := r.sim.GetOrder(o.VendorOrderID)
			for _, b := range v.Blocks {
				key := o.VendorOrderID + "|" + b.BlockID
				if done[key] || b.BinTask == "" {
					continue
				}
				done[key] = true
				r.sim.CompleteBlock(o.VendorOrderID, b.BlockID, b.Location, b.BinTask, 0, 0)
				moved = true
			}
			if v.Complete {
				r.sim.DriveState(o.VendorOrderID, "RUNNING")
				r.sim.DriveState(o.VendorOrderID, "FINISHED")
				moved = true
				continue
			}
			before := len(v.Blocks)
			r.sim.DriveState(o.VendorOrderID, "RUNNING")
			r.sim.DriveState(o.VendorOrderID, "WAITING")
			if after := r.sim.GetOrder(o.VendorOrderID); after != nil && (len(after.Blocks) != before || after.Complete) {
				moved = true
			}
		}
		if !moved {
			return
		}
	}
	r.t.Fatalf("the fleet did not settle in 60 passes: %s", r.said(r.live()))
}
