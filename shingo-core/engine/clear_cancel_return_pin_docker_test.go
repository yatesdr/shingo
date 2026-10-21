//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// clear_cancel_return_pin_docker_test.go — while a home's clear is live, the
// empty it lifted off the home never returns to the home (brief P2, the
// cancel-return clause). The two places a cancel-return chooses a node for a
// carried empty are the store-group arm (storeInto → the group's slot chooser)
// and the loader arm (storeIntoLoader). The clear's partial move is a compound
// child delivering to the home. The group arm refuses the home because its
// resolver counts every live order delivering to a slot (binresolver,
// CountActiveOrdersByDeliveryNodeExcluding), which is broader than the
// in-flight arm: with the dig-leg arm removed from orders.InFlightForDropoffSQL
// this still holds (checked by mutation). The loader arm never offers a pinned
// home to an empty at all (storeIntoLoader's first loop matches the payload).
// Pinned on both arms, with the control: the same home with no child is chosen.

// clearReturnFixture is a home H that is a slot of an NGRP group and a pinned
// home of a dedicated loader with one buffer, and an empty carried off it.
func clearReturnFixture(t *testing.T, db *store.DB, prefix string) (grp, home, buffer *nodes.Node, e *bins.Bin) {
	t.Helper()
	grp, home = storeGroupWithSlot(t, db, prefix)
	grp, err := db.GetNode(grp.ID) // NodeTypeCode is filled on read
	testutil.MustNoErr(t, err, "reload group")
	buffer = &nodes.Node{Name: prefix + "-BUF", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(buffer), "buffer")
	id, err2 := db.CreateLoader(loaders.Loader{Name: prefix, Role: "produce",
		Layout: protocol.LoaderLayoutDedicatedPositions, Replenishment: "operator"})
	testutil.MustNoErr(t, err2, "loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: home.ID,
		PayloadCode: prefix + "-P", Kind: loaders.HomeKindHome}), "pin the home")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{LoaderID: id, PositionNodeID: buffer.ID,
		Kind: loaders.HomeKindBuffer}), "buffer")
	bt, err := db.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "bin type")
	e = &bins.Bin{BinTypeID: bt.ID, Label: prefix + "-E", Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(e), "empty")
	e, err = db.GetBin(e.ID)
	testutil.MustNoErr(t, err, "reload empty")
	return grp, home, buffer, e
}

// clearPartialChild births the clear's partial move as a compound child
// delivering to home: a parent, and one plain-move child naming its bin.
func clearPartialChild(t *testing.T, db *store.DB, prefix string, home *nodes.Node) {
	t.Helper()
	src := &nodes.Node{Name: prefix + "-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "partial's node")
	p := testdb.CreateBinAtNode(t, db, prefix+"-P", src.ID, prefix+"-PBIN")
	parent := &orders.Order{EdgeUUID: prefix + "-parent", StationID: "edge.ccr", OrderType: protocol.OrderTypeMove,
		Status: protocol.StatusReshuffling, Quantity: 1, SourceNode: home.Name, DeliveryNode: home.Name}
	testutil.MustNoErr(t, db.CreateOrder(parent), "parent")
	_, err := db.CreateCompoundChildren([]store.CompoundChild{{BinID: p.ID, Order: &orders.Order{
		EdgeUUID: prefix + "-p", StationID: "edge.ccr", OrderType: protocol.OrderTypeMove,
		Status: protocol.StatusPending, Quantity: 1, ParentOrderID: &parent.ID, Sequence: 2, BinID: &p.ID,
		SourceNode: src.Name, DeliveryNode: home.Name, SourceIntent: "local"}}}, nil)
	testutil.MustNoErr(t, err, "partial's child")
}

// TestClearCancelReturn_TheEmptyNeverReturnsToTheHomeWhileTheClearIsLive: the
// store-group arm refuses the home while the partial's child is born (the
// group's only slot, so it holds), and chooses it without one (control). The
// loader arm never offers a pinned home to an empty, and never the home.
func TestClearCancelReturn_TheEmptyNeverReturnsToTheHomeWhileTheClearIsLive(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, testdb.NewTrackingBackend())

	grp, home, buffer, e := clearReturnFixture(t, db, "CCR")
	if dest, why := eng.storeInto(grp, e); dest == nil || dest.ID != home.ID {
		t.Fatalf("control: no clear live, the group arm chose %v (%s), want the free home %s", dest, why, home.Name)
	}
	clearPartialChild(t, db, "CCR", home)
	if dest, _ := eng.storeInto(grp, e); dest != nil && dest.ID == home.ID {
		t.Errorf("group arm: the empty returns to %s while the clear's partial is coming to it", home.Name)
	}
	lh, err := db.GetLoaderHomeByPositionNode(home.ID)
	testutil.MustNoErr(t, err, "home row")
	if dest, why := eng.storeIntoLoader(lh, e); dest == nil || dest.ID != buffer.ID {
		t.Errorf("loader arm chose %v (%s), want the loader's buffer %s — never the home", dest, why, buffer.Name)
	}
}
