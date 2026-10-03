//go:build docker

package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/binresolver"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// staleFirstAnswer is the resolver's read taken just before another order
// reserved the slot it names: its first answer is that slot, after which it is
// the real resolver.
type staleFirstAnswer struct {
	inner binresolver.NodeResolver
	first *nodes.Node
	used  bool
}

func (r *staleFirstAnswer) Resolve(n *nodes.Node, mode binresolver.ResolveMode, payload string,
	stated binresolver.BinTypeStatement, asker reservations.DigAsker, accept binresolver.BinFilter) (*binresolver.ResolveResult, error) {
	if !r.used {
		r.used = true
		return &binresolver.ResolveResult{Node: r.first}, nil
	}
	return r.inner.Resolve(n, mode, payload, stated, asker, accept)
}

// A PLAIN STORE THAT LOSES THE SLOT ITS GROUP RESOLVED TO GOES BACK TO THE
// GROUP. The resolved slot is written onto the order before it is claimed. When
// the claim loses a real race, the slot goes to the winner, whose bin then lands
// there; an order still aimed at that slot waits for it to empty, which for a
// storage slot may be never, while its group has room.
func TestStore_ALostSlotGoesBackToItsGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)

	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	stor, err := db.GetNodeTypeByCode(protocol.NodeClassSTOR)
	testutil.MustNoErr(t, err, "STOR type")
	grp := &nodes.Node{Name: "SLC-GRP", Enabled: true, IsSynthetic: true, NodeTypeID: &ngrp.ID}
	testutil.MustNoErr(t, db.CreateNode(grp), "group")
	s1 := &nodes.Node{Name: "SLC-S1", Enabled: true, ParentID: &grp.ID, NodeTypeID: &stor.ID}
	testutil.MustNoErr(t, db.CreateNode(s1), "slot 1")
	s2 := &nodes.Node{Name: "SLC-S2", Enabled: true, ParentID: &grp.ID, NodeTypeID: &stor.ID}
	testutil.MustNoErr(t, db.CreateNode(s2), "slot 2")

	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status = "SLC-store", OrderTypeStore, protocol.StatusQueued
		o.DeliveryNode, o.PayloadCode = grp.Name, sd.Payload.Code
	})
	winner := testdb.CreateOrder(t, db, func(w *orders.Order) {
		w.EdgeUUID, w.OrderType, w.Status = "SLC-winner", OrderTypeStore, protocol.StatusSourcing
		w.DeliveryNode, w.PayloadCode = s1.Name, sd.Payload.Code
	})

	// The race: the resolver read slot 1 free, and the winner reserved it
	// before this order's claim.
	d.resolver = &staleFirstAnswer{inner: d.resolver, first: s1}
	testutil.MustNoErr(t, db.ReserveSlot(s1.ID, winner.ID), "the winner reserves slot 1")
	if got := d.ReserveStorageDropoff(o); !got.Refused() {
		t.Fatalf("the claim on slot 1 was not lost: %+v", got)
	}

	// The winner's bin lands on slot 1 and its order ends.
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, s1.ID, "SLC-WINNER-BIN")
	testutil.MustNoErr(t, reservations.ReleaseByOrder(db.DB, winner.ID), "the winner is done")

	// The next pass reads the order afresh, as the scanner does.
	again, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "re-read the order")
	got := d.ReserveStorageDropoff(again)
	if got.Refused() || got.Node == nil || got.Node.Name != s2.Name {
		t.Fatalf("next pass: %+v (order aimed at %q), want slot 2 of %s: slot 1 is full and will stay full",
			got, again.DeliveryNode, grp.Name)
	}
}
