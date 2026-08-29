//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// demand_by_hand_docker_test.go — the two doors where a PERSON is the caller.
//
// A bin move and a carried-bin recovery both create an order that no episode
// asked for, and both had a field missing at the constructor. Neither gap shows
// up on the happy path, which is why they survived: the order dispatches, the
// robot drives, the bin lands. They show up the moment something takes the bin
// away and the order has to be read by a downstream guard that was built to
// exempt exactly this class and cannot recognise it.

// TestCreateBinMove_StampsItsSourceIntent pins the field the bin-move door was
// missing.
//
// THE COST OF THE GAP, end to end: the door stamps BinID and no PayloadCode,
// which is correct — a move relocates a physical bin and has no payload key to
// match on. The scanner's empty-payload guard knows that, and exempts a move by
// reading order.SourceIntent == "local" (fulfillment/scanner.go). But this door
// never stamped SourceIntent, so the exemption built to protect moves could not
// see one. While the order kept its bin_id the guard was never reached
// (scanner.go routes a held bin straight to dispatch), and the day a dig took
// the bin and cleared the pointer, the order fell through to a guard that
// called a person's move a construction bug: "order has empty payload_code".
//
// Every other door stamps it — lifecycle intake, compound children, loader
// replenish — through the same SourceIntentForType. This one is now the fourth.
func TestCreateBinMove_StampsItsSourceIntent(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, lineNode, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "PART-A", storageNode.ID, "BIN-INTENT")
	eng := newTestEngine(t, db, simulator.New())

	res, err := eng.CreateBinMove(BinMoveRequest{
		Selection:    BinSelectionAuto,
		SourceNodeID: storageNode.ID,
		DestNodeID:   lineNode.ID,
		StationID:    "test-station",
		Desc:         "manual move",
	})
	testutil.MustNoErr(t, err, "create bin move")

	got, err := db.GetOrder(res.OrderID)
	testutil.MustNoErr(t, err, "get order")

	if got.SourceIntent != dispatch.SourceIntentLocal {
		t.Fatalf("SourceIntent = %q, want %q — a move sources the bin AT its source node, and "+
			"every guard downstream reads that from the data field, not from the order type. "+
			"Unstamped, the scanner's own move exemption cannot see this order and fails it "+
			"structural the moment it loses its bin_id",
			got.SourceIntent, dispatch.SourceIntentLocal)
	}
}

// TestRecoverCarriedBin_StampsItsOriginClass pins the recovery door's class.
//
// A recovery order landed with a BLANK origin_class — not attached, not
// no_demand, not orphan, but the empty-string vacuum the column defaults to. That is
// invisible in both directions: the no_demand bucket does not count it, and the
// orphan surface — the one place a lost demand link is supposed to show up —
// does not either. Its siblings at the same kind of door (the bin move, the
// spot orders) all stamp no_demand at the literal, for the stated reason that
// leaving it blank puts a deliberate action in the bucket that means "we lost
// something".
//
// A recovery is Core tidying physics, so no_demand is the true answer, and
// stamping it at birth is the only way it is ever true: nothing downstream may
// reconstruct whether a call happened.
func TestRecoverCarriedBin_StampsItsOriginClass(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	backend := testdb.NewTrackingBackend()
	eng := newTestEngine(t, db, backend)

	dest := &nodes.Node{Name: "RECOVERY-DEST", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest")
	bin := seedCarried(t, db, "AMR-OC1", "RECOVERY-DEST")
	cacheRobot(eng, dispatchableRobot("AMR-OC1"))

	order, _, err := eng.RecoverCarriedBin(bin.ID, "operator:test")
	testutil.MustNoErr(t, err, "recover carried bin")

	got, err := db.GetOrder(order.ID)
	testutil.MustNoErr(t, err, "get order")

	if got.OriginClass != protocol.OriginClassNoDemand {
		t.Fatalf("OriginClass = %q, want %q — a recovery is Core reconciling its own books with "+
			"the floor, so it is originless BY CONSTRUCTION and says so at the literal. Blank "+
			"is the vacuum: neither counted as no_demand nor surfaced as orphan",
			got.OriginClass, protocol.OriginClassNoDemand)
	}
}
