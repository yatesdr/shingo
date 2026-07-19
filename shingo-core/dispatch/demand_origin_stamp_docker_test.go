//go:build docker

package dispatch

import (
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

// TestOriginIntake_BuriedComplexParentStampsFromTheEnvelope covers intake site
// 3 of 3 — the one most likely to be missed, because handleComplexBuriedAtIntake
// LOOKS derivative: it is reached from the resolver's buried branch and its
// first act is to schedule a reshuffle. It is not. The order it builds is the
// complex parent itself and its origin comes off the same envelope as the main
// path's.
//
// Miss it and every complex order that happens to arrive on a buried bin becomes
// an orphan — a bucket that fills in proportion to how full the lanes are.
func TestOriginIntake_BuriedComplexParentStampsFromTheEnvelope(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-BURIED-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-BURIED-TGT")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.handleComplexBuriedAtIntake(testEnvelope(),
		&protocol.ComplexOrderRequest{
			OrderUUID:   "uuid-buried-origin",
			OriginID:    testOriginID,
			OriginClass: protocol.OriginClassAttached,
		},
		bp.Code,
		&BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID},
	)

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

// TestOriginPropagation_CompoundChildrenCarryTHEPARENTSOriginID covers the
// derivative site through its ordinary caller, and the assertion is on the
// SPECIFIC id. (There was a second derivative site — the synthetic restore
// parent — until cb74bfdc deleted that subsystem; see
// TestOriginPropagation_DigChildrenStampAtTheSeamNotTheCaller.)
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

// TestOriginPropagation_DigChildrenStampAtTheSeamNotTheCaller pins the stamp on
// CreateCompoundChildrenOnly ITSELF — the single orders INSERT every derivative
// order is born from — rather than on CreateCompoundOrder, which merely calls it.
//
// THIS TEST REPLACED THE RESTORE-SYNTHETIC PROOF, and the substitution is not
// like-for-like. The original was derivative site 2 of 2: the synthetic restore
// parent set NO ParentOrderID at all, so a read-time walk from it reached
// nothing, which made it the sharpest available argument that the origin has to
// be carried forward at creation. That subsystem was deleted (cb74bfdc) and the
// argument lost its live example — every surviving derivative order DOES set a
// parent. The rule did not change; only the demonstration did.
//
// So this asserts what still has teeth on the surviving dig path. Site 1
// (TestOriginPropagation_CompoundChildrenCarryTHEPARENTSOriginID) enters through
// CreateCompoundOrder, which means the stamp could in principle be moved up into
// that wrapper and site 1 would stay green while CreateCompoundChildrenOnly — a
// method with its own contract, documented for parents already Reshuffling —
// silently stopped stamping. Entering at the seam is what closes that.
//
// The decoy carries a DIFFERENT origin so a child inheriting from the wrong row
// fails on the value, not on a presence check.
func TestOriginPropagation_DigChildrenStampAtTheSeamNotTheCaller(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	grp, lane, slots, _, bp := setupNodeGroupWithShuffle(t, db)

	decoy := &orders.Order{
		EdgeUUID: "uuid-dig-seam-decoy", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusQueued,
		OriginID: testOriginIDAlt, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(decoy), "create decoy")

	// Written directly at Reshuffling — the state CreateCompoundChildrenOnly
	// exists to serve, and the reason it is a separate method from
	// CreateCompoundOrder (whose BeginReshuffle would be a no-op transition).
	parent := &orders.Order{
		EdgeUUID: "uuid-dig-seam-origin", StationID: "line-1",
		OrderType: OrderTypeComplex, Status: StatusReshuffling,
		OriginID: testOriginID, OriginClass: protocol.OriginClassAttached,
	}
	testutil.MustNoErr(t, db.CreateOrder(parent), "create already-reshuffling parent")

	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "BIN-ORIGIN-DIG-BLK")
	target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, "BIN-ORIGIN-DIG-TGT")
	plan, err := PlanReshuffleUnburyOnly(db, target, slots[1], lane, grp.ID)
	testutil.MustNoErr(t, err, "plan unbury")

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, d.CreateCompoundChildrenOnly(parent, plan), "CreateCompoundChildrenOnly")

	children, err := db.ListChildOrders(parent.ID)
	testutil.MustNoErr(t, err, "list dig children")
	if len(children) == 0 {
		t.Fatal("no dig children created — this test asserted nothing")
	}
	for _, c := range children {
		if c.OriginID == testOriginIDAlt {
			t.Errorf("dig child %d inherited the DECOY's origin %q — the stamp is reading the wrong row",
				c.ID, c.OriginID)
			continue
		}
		wantOrigin(t, c, "dig child via the children-only seam", testOriginID, protocol.OriginClassAttached)
	}
}
