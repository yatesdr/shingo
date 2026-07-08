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

func mkPlainOrder(t *testing.T, db interface {
	CreateOrder(*orders.Order) error
}, uuid string, otype protocol.OrderType, payload, destName string) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "ST", OrderType: otype, Status: StatusQueued,
		Quantity: 1, PayloadCode: payload, DeliveryNode: destName,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create plain order")
	return o
}

// TestStage3_IsStorageDropoff pins the corrected reserve predicate: it must be
// TRUE for a standalone STOR node (the store's own destination, frequently
// top-level and so isConcreteStorageDropoff-false — the bare predicate would have
// regressed C2) AND for a deep-lane LANE-child slot, and FALSE for a line/consume
// node.
func TestStage3_IsStorageDropoff(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, lineNode, _ := setupTestData(t, db) // lineNode is a top-level, untyped consume node
	storType, err := db.GetNodeTypeByCode("STOR")
	testutil.MustNoErr(t, err, "get STOR type")
	laneType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "get LANE type")

	storNode := &nodes.Node{Name: "S3-STOR", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(storNode), "create STOR node")

	lane := &nodes.Node{Name: "S3-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID}
	testutil.MustNoErr(t, db.CreateNode(lane), "create lane")
	lane, _ = db.GetNode(lane.ID)
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

// TestStage3_MoveToStorageRace_ExactlyOneWins pins the ★ fix: two concurrent
// moves to the same empty storage node now go through the node-driven reserve —
// exactly one wins, the other must requeue. Pre-Stage-3 a move had only a
// CheckDropoffCapacity read, so both would have dropped (the #115/#117 race the
// store had before C2).
func TestStage3_MoveToStorageRace_ExactlyOneWins(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	storType, _ := db.GetNodeTypeByCode("STOR")
	dest := &nodes.Node{Name: "S3-MV-DEST", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create STOR dest")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	m1 := mkPlainOrder(t, db, "s3-mv-1", OrderTypeMove, bp.Code, dest.Name)
	m2 := mkPlainOrder(t, db, "s3-mv-2", OrderTypeMove, bp.Code, dest.Name)

	e1 := d.ReserveStorageDropoff(m1)
	e2 := d.ReserveStorageDropoff(m2)
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
	db := testDB(t)
	_, _, bp := setupTestData(t, db)
	storType, _ := db.GetNodeTypeByCode("STOR")
	dest := &nodes.Node{Name: "S3-MVST-DEST", Enabled: true, NodeTypeID: &storType.ID}
	testutil.MustNoErr(t, db.CreateNode(dest), "create STOR dest")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	mv := mkPlainOrder(t, db, "s3-mvst-move", OrderTypeMove, bp.Code, dest.Name)
	st := mkPlainOrder(t, db, "s3-mvst-store", OrderTypeStore, bp.Code, dest.Name)

	e1 := d.ReserveStorageDropoff(mv)
	e2 := d.ReserveStorageDropoff(st)
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
