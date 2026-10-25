//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// clear_fence_pin_docker_test.go — the fence a compound child's drop puts on
// its node from birth, pinned before the home's clear leans on it.
//
// The clear's children are plain moves under a parent (STOP 1): the partial's
// move P S→H and, on the spot arm, the empty's move E H→X. Nothing reserves H
// or X for them. What keeps the loader's own empty pull (core-l1-, through
// LoaderWindows) and every plain mover (CheckDropoffCapacity) off those nodes
// from the moment the children are born is the in-flight count's dig-leg arm
// (orders.InFlightForDropoffSQL): a child naming its bin and not yet handed to
// the fleet counts as inbound to its delivery node. This pins that, on an EMPTY
// node, so occupancy cannot be the answer.

// TestClearFence_ChildDropCountsInboundFromBirth: a pending child naming its
// bin makes its (empty) delivery node read in-flight to the plain capacity gate
// and to the loader-window gate's first question; the same node with no child
// is free. Mutation: drop the dig-leg arm from InFlightForDropoffSQL and both
// gates read the node free.
func TestClearFence_ChildDropCountsInboundFromBirth(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	setupTestData(t, db)
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n
	}
	src, home, spot, free := mk("CFP-S"), mk("CFP-H"), mk("CFP-X"), mk("CFP-FREE")
	partial := makeEmptyBin(t, db, src.ID, "CFP-P")
	emptyOnHome := makeEmptyBin(t, db, mk("CFP-ELSE").ID, "CFP-E")

	parent := &orders.Order{EdgeUUID: "cfp-parent", StationID: "edge.cfp", OrderType: OrderTypeMove,
		Status: StatusReshuffling, Quantity: 1, SourceNode: home.Name, DeliveryNode: home.Name}
	testutil.MustNoErr(t, db.CreateOrder(parent), "parent")
	child := func(uuid string, seq int, bin int64, from, to string) store.CompoundChild {
		return store.CompoundChild{Order: &orders.Order{EdgeUUID: uuid, StationID: "edge.cfp",
			OrderType: OrderTypeMove, Status: StatusPending, Quantity: 1, ParentOrderID: &parent.ID,
			Sequence: seq, BinID: &bin, SourceNode: from, DeliveryNode: to,
			SourceIntent: SourceIntentForType(OrderTypeMove)}}
	}
	_, err := testdb.CreateCompoundChildren(t, db, []store.CompoundChild{
		child("cfp-e", 1, emptyOnHome.ID, home.Name, spot.Name),
		child("cfp-p", 2, partial.ID, src.Name, home.Name),
	}, nil)
	testutil.MustNoErr(t, err, "children")

	for _, n := range []*nodes.Node{home, spot} {
		if blocked, b := CheckDropoffCapacity(db, n.Name, 0); !blocked || b.Cause != CauseDropoffInflight {
			t.Errorf("%s: plain gate blocked=%v cause=%q, want blocked by dropoff-inflight (a child's drop, from birth)",
				n.Name, blocked, b.Cause)
		}
		if blocked, v := plan.DropoffCapacity(db, n.Name, 0); !blocked || v.Cause != plan.CauseDropoffInflight {
			t.Errorf("%s: the loader-window gate's capacity question blocked=%v cause=%q, want in-flight",
				n.Name, blocked, v.Cause)
		}
	}
	if blocked, b := CheckDropoffCapacity(db, free.Name, 0); blocked {
		t.Errorf("control: %s with no child reads blocked (%q)", free.Name, b.Cause)
	}
}
