//go:build docker

package dispatch

import (
	"fmt"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// demand_origin_stamp_docker_test.go — the demand grain's Core half, against a
// real Postgres.
//
// EVERY ASSERTION HERE NAMES A SPECIFIC ID, and that is the whole point of the
// file. The naturally-written propagation test builds a parent with an origin
// and checks the child "has one" — which stays green when the child mints its
// own, when it inherits from the wrong parent, and when a default leaks in from
// the DDL. Verified red by replacing the inheritance with a fresh uuid: the
// presence-shaped assertion passed, these fail naming both ids.

const (
	// A fixed, valid UUID rather than uuid.NewString(): the failure message has
	// to be able to say "got X, want Y" about a value the reader can find in the
	// source, and the id being stable is what makes a mismatch diagnosable.
	testOriginID    = "6ba7b810-9dad-11d1-80b4-00c04fd430c8"
	testOriginIDAlt = "6ba7b811-9dad-11d1-80b4-00c04fd430c8"
)

func wantOrigin(t *testing.T, o *orders.Order, what, wantID, wantClass string) {
	t.Helper()
	if o.OriginID != wantID {
		t.Errorf("%s: origin_id = %q, want %q", what, o.OriginID, wantID)
	}
	if o.OriginClass != wantClass {
		t.Errorf("%s: origin_class = %q, want %q", what, o.OriginClass, wantClass)
	}
}

// TestOriginStamp_SurvivesTheRoundTrip is the floor everything else stands on:
// the columns are in the INSERT column list AND in SelectCols. orders has TWO
// independent INSERT statements (orders.Create and CreateCompoundChildren), so a
// column named in one and not the other is silently dropped on exactly one path;
// both are exercised, here and in the compound test below.
func TestOriginStamp_SurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	db := testDB(t)

	attached := &orders.Order{
		EdgeUUID: "uuid-origin-rt-attached", StationID: "line-1",
		OrderType: OrderTypeRetrieve, Status: StatusPending,
		OriginID: testOriginID, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(attached), "create attached")
	got, err := db.GetOrder(attached.ID)
	testutil.MustNoErr(t, err, "read back attached")
	wantOrigin(t, got, "attached round-trip", testOriginID, protocol.OriginClassAttached)

	// no_demand carries NO id, and it must come back as SQL NULL rather than
	// "" — origin_id is a UUID column that rejects the empty string outright,
	// and the partial index idx_orders_origin_id keys on IS NOT NULL.
	noDemand := &orders.Order{
		EdgeUUID: "uuid-origin-rt-nodemand", StationID: "line-1",
		OrderType: OrderTypeMove, Status: StatusPending,
		OriginClass: protocol.OriginClassNoDemand,
	}
	testutil.MustNoErr(t, db.CreateOrder(noDemand), "create no_demand")
	got, err = db.GetOrder(noDemand.ID)
	testutil.MustNoErr(t, err, "read back no_demand")
	wantOrigin(t, got, "no_demand round-trip", "", protocol.OriginClassNoDemand)

	var isNull bool
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT origin_id IS NULL FROM orders WHERE id=$1`, noDemand.ID).Scan(&isNull), "probe NULL")
	if !isNull {
		t.Error("origin_id stored as '' rather than NULL — the partial index will not cover it")
	}
}

// TestOriginIntake_SimpleOrderStampsFromTheEnvelope covers intake site 1 of 3
// (CreateInboundOrder) across all three classes in one pass.
func TestOriginIntake_SimpleOrderStampsFromTheEnvelope(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, bp := setupTestData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	cases := []struct {
		name      string
		uuid      string
		id, class string
		wantID    string
		wantClass string
	}{
		{"attached", "uuid-in-attached", testOriginID, protocol.OriginClassAttached, testOriginID, protocol.OriginClassAttached},
		{"no_demand", "uuid-in-nodemand", "", protocol.OriginClassNoDemand, "", protocol.OriginClassNoDemand},
		// The skew case: an Edge that predates origins says nothing at all.
		{"orphan", "uuid-in-orphan", "", "", "", protocol.OriginClassOrphan},
	}
	for _, c := range cases {
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
			OrderUUID:    c.uuid,
			OrderType:    OrderTypeRetrieve,
			PayloadCode:  bp.Code,
			Quantity:     1,
			DeliveryNode: lineNode.Name,
			OriginID:     c.id,
			OriginClass:  c.class,
		})
		got, err := db.GetOrderByUUID(c.uuid)
		testutil.MustNoErr(t, err, "read back "+c.name)
		wantOrigin(t, got, "simple intake ("+c.name+")", c.wantID, c.wantClass)
	}
}

// TestOriginIntake_ComplexOrderStampsFromTheEnvelope covers intake site 2 of 3
// (HandleComplexOrderRequest), and with it BOTH LEGS OF A SWAP: Edge sends the
// supply and the evac with the same id because one fire of applyConsumePlan is
// one demand, and Core must store the id twice rather than treat the second
// arrival as a second demand.
func TestOriginIntake_ComplexOrderStampsFromTheEnvelope(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, bp := setupTestData(t, db)
	createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-ORIGIN-CPX")
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	for _, uuid := range []string{"uuid-cpx-supply", "uuid-cpx-evac"} {
		d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
			OrderUUID:   uuid,
			PayloadCode: bp.Code,
			Quantity:    1,
			Steps: []protocol.ComplexOrderStep{
				{Action: "pickup", Node: storageNode.Name},
				{Action: "dropoff", Node: lineNode.Name},
			},
			OriginID:    testOriginID,
			OriginClass: protocol.OriginClassAttached,
		})
		got, err := db.GetOrderByUUID(uuid)
		testutil.MustNoErr(t, err, "read back "+uuid)
		wantOrigin(t, got, "complex intake ("+uuid+")", testOriginID, protocol.OriginClassAttached)
	}
}

// TestOriginPropagation_ReshuffleChildrenInheritFromTheBuriedParent is the
// derivative half of the buried path: the unbury moves exist only because this
// demand needed a bin that was behind another one, so they are part of what the
// demand cost and belong in its child count.
//
// The front-door half — that a complex order arriving on a buried bin still
// gets its origin stamped at all — moved to
// TestComplex_BuriedSourceTriggersReshuffle, which drives a real burial through
// HandleComplexOrderRequest. It belongs there now: the buried arm stopped
// building its own parent, so "the buried parent is stamped" is a claim about
// whether the arm REACHES the shared create, and only the front door shows that.
// What is left here is what happens below the parent, so the parent is seeded
// directly — the same row complex intake writes.
func TestOriginPropagation_ReshuffleChildrenInheritFromTheBuriedParent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-BURIED-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-BURIED-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	parent := &orders.Order{
		EdgeUUID:    "uuid-buried-origin",
		StationID:   "line-1",
		OrderType:   OrderTypeComplex,
		Status:      StatusQueued,
		Quantity:    1,
		PayloadCode: bp.Code,
		Coordinated: true,
		OriginID:    testOriginID,
		OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "seed buried complex parent")
	d.planBuriedReshuffleAtIntake(parent, bp.Code, "line-1",
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})

	parent, err := db.GetOrderByUUID("uuid-buried-origin")
	testutil.MustNoErr(t, err, "read back buried complex parent")
	wantOrigin(t, parent, "buried complex parent", testOriginID, protocol.OriginClassAttached)

	// And the reshuffle compound it scheduled IS derivative — the unbury moves
	// exist only because this demand needed a buried bin, so they are part of
	// what the demand cost and belong in its child count.
	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list reshuffle children")
	if len(children) == 0 {
		t.Fatal("no reshuffle children created — the derivative half of this test asserted nothing")
	}
	for _, c := range children {
		wantOrigin(t, c, "reshuffle child of the buried parent", testOriginID, protocol.OriginClassAttached)
	}
}

// TestOriginPropagation_CompoundChildrenCarryTHEPARENTSOriginID is derivative
// site 1 of 2, and the assertion is on the SPECIFIC id.
//
// The parent here carries testOriginID and a second, unrelated order carries
// testOriginIDAlt, so a child that inherited from the wrong row — or minted its
// own — fails on the value rather than sliding past a presence check.
func TestOriginPropagation_CompoundChildrenCarryTHEPARENTSOriginID(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	// A decoy sharing the station and payload, holding a DIFFERENT origin.
	decoy := &orders.Order{
		EdgeUUID: "uuid-compound-decoy", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusQueued,
		OriginID: testOriginIDAlt, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(decoy), "create decoy")

	parent := &orders.Order{
		EdgeUUID: "uuid-compound-origin", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusQueued,
		OriginID: testOriginID, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-CMP-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-CMP-TGT")
	plan, err := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)
	testutil.MustNoErr(t, err, "plan unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "CreateCompoundOrder")

	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list children")
	if len(children) == 0 {
		t.Fatal("no compound children created — this test asserted nothing")
	}
	for _, c := range children {
		if c.OriginID == testOriginIDAlt {
			t.Errorf("child %d inherited the DECOY's origin %q — the stamp is reading the wrong row",
				c.ID, c.OriginID)
			continue
		}
		wantOrigin(t, c, "compound child", testOriginID, protocol.OriginClassAttached)
	}
}

// TestOriginPropagation_CompoundChildrenInheritNoDemandToo is the half a
// presence-shaped test cannot express at all: the CLASS travels with the id.
//
// A no_demand parent has no origin to copy, so a child that only inherits the id
// gets an empty pair — and an empty class is not a class. It would sit in
// "origin_id IS NULL and the class is empty", the unanswerable state the enum was
// added to abolish, indistinguishable from an old-Edge order on any surface.
func TestOriginPropagation_CompoundChildrenInheritNoDemandToo(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	parent := &orders.Order{
		EdgeUUID: "uuid-compound-nodemand", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusQueued,
		OriginClass: protocol.OriginClassNoDemand,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create no_demand parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-ND-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-ND-TGT")
	plan, err := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)
	testutil.MustNoErr(t, err, "plan unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(parent, plan), "CreateCompoundOrder")

	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list children")
	if len(children) == 0 {
		t.Fatal("no compound children created — this test asserted nothing")
	}
	for _, c := range children {
		if c.OriginClass == protocol.OriginClassOrphan {
			t.Errorf("child %d of a no_demand parent landed ORPHAN — a housekeeping move is now a finding", c.ID)
			continue
		}
		wantOrigin(t, c, "compound child of a no_demand parent", "", protocol.OriginClassNoDemand)
	}
}

// TestOriginPropagation_RestoreSyntheticAndItsChildrenCarryTheOrigin is
// derivative site 2 of 2, and it is the one that proves the rule has to be
// stamp-forward.
//
// The synthetic restore parent sets NO ParentOrderID at all — its only link to
// the complex parent is a formatted EdgeUUID string and an in-memory map that
// does not survive a restart — so a read-time walk from it reaches nothing. The
// origin has to be carried across that boundary at creation or it is gone. Two
// hops are asserted: complex parent → synthetic, and synthetic → its own restock
// children, which reach the episode two levels down.
func TestOriginPropagation_RestoreSyntheticAndItsChildrenCarryTheOrigin(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, PropReshuffleRestoreBlockers, "on"), "arm restore-blockers")

	complexParent := &orders.Order{
		EdgeUUID: "uuid-restore-origin", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusQueued,
		OriginID: testOriginID, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(complexParent), "create complex parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-RST-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-RST-TGT")
	plan, err := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)
	testutil.MustNoErr(t, err, "plan unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundOrder(complexParent, plan), "CreateCompoundOrder")
	d.scheduleRestoreIfEnabled(complexParent, grp.ID, lane.ID, plan, slots[1].ID)

	syn, err := db.GetOrderByUUID(fmt.Sprintf("restore-%d-%d", complexParent.ID, target.ID))
	testutil.MustNoErr(t, err, "read back synthetic restore parent")
	wantOrigin(t, syn, "synthetic restore parent", testOriginID, protocol.OriginClassAttached)

	if syn.ParentOrderID != nil {
		t.Errorf("the synthetic restore parent now HAS a parent_order_id (%d) — "+
			"this test's premise (that a read-time walk from it reaches nothing) no longer holds; "+
			"re-read the stamp-forward rationale before relaxing anything", *syn.ParentOrderID)
	}

	// Second hop: the restock compound the synthetic fathers. This is the one
	// that reaches the episode two levels from the demand, and it goes through
	// CreateCompoundChildren — the OTHER orders INSERT.
	d.HandleBinEnteredTransit(target.ID, slots[1].ID)
	restock, err := db.ListChildOrders(syn.ID)
	testutil.MustNoErr(t, err, "list restock children")
	if len(restock) == 0 {
		t.Fatal("no restock children created — the second hop asserted nothing")
	}
	for _, c := range restock {
		wantOrigin(t, c, "restock child of the synthetic", testOriginID, protocol.OriginClassAttached)
	}
}
