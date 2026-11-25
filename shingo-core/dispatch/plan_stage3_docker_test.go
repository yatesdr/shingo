//go:build docker

package dispatch

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

func mkPlainOrder(t *testing.T, db interface {
	CreateOrder(*orders.Order) error
}, uuid string, otype protocol.OrderType, payload, destName string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationUID: "ST", OrderType: otype, Status: StatusQueued,
		Quantity: 1, PayloadCode: payload, DeliveryNode: destName,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create plain order")
	return o
}

// TestStage3_IsStorageDropoff pins the corrected reserve predicate: it must be
// TRUE for a standalone STOR node (the store's own destination, frequently
// top-level and so false to a LANE/NGRP-parent test — that bare predicate would have
// regressed C2) AND for a deep-lane LANE-child slot, and FALSE for a line/consume
// node.
func TestStage3_IsStorageDropoff(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, lineNode, _ := testdb.SetupTestData(t, db) // lineNode is a top-level, untyped consume node
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "get STOR type")
	laneType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	storNode := &nodes.Node{Name: "S3-STOR", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(storNode), "create STOR node")

	lane := &nodes.Node{Name: "S3-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: testdb.MarkedGroup(t, db, "S3-LANE-GRP")}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	laneRaw, err := db.GetNode(lane.ID)
	lane = testutil.Must(t, laneRaw, err, "db.GetNode(lane.ID)")
	laneChild := &nodes.Node{Name: "S3-LANE-SLOT", Enabled: true, ParentID: &lane.ID}
	testutil.MustNoErr(t, db.CreateNode(laneChild), "create lane child")

	cases := []struct {
		name string
		node string
		want bool
	}{
		{"standalone STOR node (store's dest, top-level)", storNode.Name, true},
		{"LANE-child slot (deep lane)", laneChild.Name, true},
		{"line / consume node", lineNode.Name, false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := isStorageDropoff(db, c.node); got != c.want {
			t.Errorf("isStorageDropoff(%s=%q) = %v, want %v", c.name, c.node, got, c.want)
		}
	}
}

// TestAttemptNodes_AnswerAsTheStoreDoes pins one complex attempt's read of its
// plan (attemptNodes) to the store's doors: for every name the steps name, a
// name they do not, a wait's node, a dotted child and an unknown one, the
// view's GetNodeByDotName answers what the store's does (the same node, or
// sql.ErrNoRows), asked once and asked again; for every node it finds, GetNode
// and LaneOf answer as the store's, and the storage question
// (isStorageDropoff) answers the same over both. The want columns state each
// node's lane and storage answer, so a fixture that stops building what it says
// fails here rather than agreeing vacuously.
func TestAttemptNodes_AnswerAsTheStoreDoes(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	testdb.SetupStandardData(t, db) // the node types, STOR among them
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "get STOR type")
	laneType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")
	mk := func(n *nodes.Node) *nodes.Node {
		t.Helper()
		n.Enabled = true
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
		return n
	}
	grp := testdb.MarkedGroup(t, db, "AN-GRP")
	lane := mk(&nodes.Node{Name: "AN-LANE", IsSynthetic: true, NodeTypeID: &laneType.ID, ParentID: grp})
	s1 := mk(&nodes.Node{Name: "AN-S1", ParentID: &lane.ID})
	mk(&nodes.Node{Name: "AN-S2", ParentID: &lane.ID})
	mk(&nodes.Node{Name: "AN-FLAT", ParentID: grp})
	mk(&nodes.Node{Name: "AN-STOR", NodeTypeID: &storType.ID})
	mk(&nodes.Node{Name: "AN-LINE"})

	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: s1.Name},
		{Action: protocol.ActionDropoff, Node: "AN-STOR"},
		{Action: protocol.ActionPickup, Node: "AN-FLAT"},
		{Action: protocol.ActionDropoff, Node: "AN-LANE.AN-S2"},
		{Action: protocol.ActionDropoff, Node: "AN-NOPE"},
		{Action: protocol.ActionPickup, Node: s1.Name},
		{Action: protocol.ActionWait, Node: "AN-LINE"},
		{Action: protocol.ActionDropoff},
	}
	view := readAttemptNodes(db, steps)
	cases := []struct {
		name    string
		lane    string // "" in no lane; "-" not found
		storage bool
	}{
		{"AN-S1", "AN-LANE", true},
		{"AN-STOR", "", true},
		{"AN-FLAT", "", true},
		{"AN-LANE.AN-S2", "AN-LANE", true},
		{"AN-NOPE", "-", false},
		{"AN-LINE", "", false},          // a wait's node: not in the batch, read through
		{"AN-GRP", "", false},           // named by no step: read through
		{"AN-LANE.AN-NOPE", "-", false}, // a dotted name with no such child
	}
	for _, c := range cases {
		for ask := 1; ask <= 2; ask++ {
			want, wErr := db.GetNodeByDotName(c.name)
			got, gErr := view.GetNodeByDotName(c.name)
			if (wErr == nil) != (gErr == nil) || (wErr != nil && !errors.Is(gErr, sql.ErrNoRows)) ||
				!reflect.DeepEqual(want, got) {
				t.Errorf("%q (ask %d): the store answers %+v, %v; the view %+v, %v", c.name, ask, want, wErr, got, gErr)
				continue
			}
			if (got == nil) != (c.lane == "-") {
				t.Errorf("%q (ask %d) found %+v, want found=%v", c.name, ask, got, c.lane != "-")
			}
			if got == nil {
				continue
			}
			wantRaw, err := db.GetNode(got.ID)
			wantByID := testutil.Must(t, wantRaw, err, "the store's GetNode")
			if byID, err := view.GetNode(got.ID); err != nil || !reflect.DeepEqual(byID, wantByID) {
				t.Errorf("%q: GetNode(%d) over the view = %+v, %v; the store's %+v", c.name, got.ID, byID, err, wantByID)
			}
			wantLane, wlErr := db.LaneOf(got)
			gotLane, glErr := view.LaneOf(got)
			if wlErr != nil || glErr != nil || !reflect.DeepEqual(wantLane, gotLane) {
				t.Errorf("%q: LaneOf over the store %+v, %v; over the view %+v, %v", c.name, wantLane, wlErr, gotLane, glErr)
			}
			if name := nodeName(gotLane); (gotLane == nil && c.lane != "") || (gotLane != nil && name != c.lane) {
				t.Errorf("%q stands in lane %q, want %q", c.name, name, c.lane)
			}
		}
		if s, v := isStorageDropoff(db, c.name), isStorageDropoff(view, c.name); s != v || s != c.storage {
			t.Errorf("%q: isStorageDropoff over the store %v, over the view %v, want %v", c.name, s, v, c.storage)
		}
	}
}

// TestStage3_MoveToStorageRace_ExactlyOneWins pins the ★ fix: two concurrent
// moves to the same empty storage node now go through the node-driven reserve —
// exactly one wins, the other must requeue. Pre-Stage-3 a move had only a
// CheckDropoffCapacity read, so both would have dropped (the #115/#117 race the
// store had before C2).
func TestStage3_MoveToStorageRace_ExactlyOneWins(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, _, bp := testdb.SetupTestData(t, db)
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "db.GetNodeTypeByCode(\"STOR\")")
	dest := &nodes.Node{Name: "S3-MV-DEST", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create STOR dest")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	m1 := mkPlainOrder(t, db, "s3-mv-1", OrderTypeMove, bp.Code, dest.Name)
	m2 := mkPlainOrder(t, db, "s3-mv-2", OrderTypeMove, bp.Code, dest.Name)

	e1 := d.ReserveStorageDropoff(m1).Err
	e2 := d.ReserveStorageDropoff(m2).Err
	wins := 0
	if e1 == nil {
		wins++
	}
	if e2 == nil {
		wins++
	}
	if wins != 1 {
		t.Fatalf("exactly one move must win the storage slot, got %d winners (e1=%v e2=%v)", wins, e1, e2)
	}
}

// TestStage3_MoveVsStore_ExactlyOneWins proves the reserve is NODE-driven, not
// type-driven: a move and a store racing for the same empty storage node — exactly
// one wins, regardless of which type it is.
func TestStage3_MoveVsStore_ExactlyOneWins(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	_, _, bp := testdb.SetupTestData(t, db)
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "db.GetNodeTypeByCode(\"STOR\")")
	dest := &nodes.Node{Name: "S3-MVST-DEST", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create STOR dest")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mv := mkPlainOrder(t, db, "s3-mvst-move", OrderTypeMove, bp.Code, dest.Name)
	// The second order's type is incidental — the point is that
	// ReserveStorageDropoff is driven by the DESTINATION being a STOR node, not
	// by the order type, so two differently-typed orders race the same slot.
	st := mkPlainOrder(t, db, "s3-mvst-store", protocol.OrderType("store"), bp.Code, dest.Name)

	e1 := d.ReserveStorageDropoff(mv).Err
	e2 := d.ReserveStorageDropoff(st).Err
	wins := 0
	if e1 == nil {
		wins++
	}
	if e2 == nil {
		wins++
	}
	if wins != 1 {
		t.Fatalf("exactly one of {move, store} must win the shared storage slot, got %d (move=%v store=%v)", wins, e1, e2)
	}
}

// TestStage3_ALoaderWindowIsHeldAtAcquire: a loader's window is one carrier's at
// a time, so a plain order delivering to it holds it at its acquire
// (holdPosition: the storage question or the loader-home read), as a storage
// slot is held: the first of two moves to the window holds its pending slot row
// and the second waits on the slot. A line holds nothing, as before. The window
// is a plain node (no STOR type, no group parent), so only its home row says it
// is a window.
func TestStage3_ALoaderWindowIsHeldAtAcquire(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	f := newDrainFixture(t, db, "S3LW", 1, 0)
	w := f.windows[0]
	_, line, bp := testdb.SetupTestData(t, db)

	m1 := mkPlainOrder(t, db, "s3lw-1", OrderTypeMove, bp.Code, w.Name)
	m2 := mkPlainOrder(t, db, "s3lw-2", OrderTypeMove, bp.Code, w.Name)
	if dest := f.d.ReserveStorageDropoff(m1); dest.Refused() {
		t.Fatalf("the first move to window %s was refused: %v", w.Name, dest.Err)
	}
	if !holdsSlot(t, db, m1.ID, w.ID) {
		t.Errorf("the first move to window %s holds no slot row on it: a window is one carrier's at a time", w.Name)
	}
	if dest := f.d.ReserveStorageDropoff(m2); !dest.Refused() || dest.Cause != CauseStoreSlotContended {
		t.Errorf("the second move to window %s: refused=%v cause %q, want it to wait as store-slot-contended",
			w.Name, dest.Refused(), dest.Cause)
	}

	toLine := mkPlainOrder(t, db, "s3lw-line", OrderTypeMove, bp.Code, line.Name)
	if dest := f.d.ReserveStorageDropoff(toLine); dest.Refused() {
		t.Fatalf("a move to the line was refused: %v", dest.Err)
	}
	if holdsSlot(t, db, toLine.ID, line.ID) {
		t.Errorf("a move to the line %s holds a slot row on it: a line is no one carrier's", line.Name)
	}
}
