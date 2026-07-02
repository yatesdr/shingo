//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// TestMoveToTransit_PreservesClaim is the core invariant of the
// bin-transit-state design: when a bin enters transit, its claimed_by
// is preserved so the owning order still owns the bin until either
// ApplyArrival commits the delivery or the failure path clears the
// claim. Without that invariant, the anomaly signal
// `node_id=_TRANSIT AND claimed_by IS NULL` would also fire on every
// healthy in-flight bin.
func TestMoveToTransit_PreservesClaim(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "TR-BT", Description: "transit-test bin type"}
	db.CreateBinType(bt)

	source := &nodes.Node{Name: "TR-SOURCE", Enabled: true}
	db.CreateNode(source)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "TR-BIN-1", NodeID: &source.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	orderID := testdb.CreateOrder(t, db).ID
	testdb.ClaimBinForTest(t, db, bin.ID, orderID)

	testutil.MustNoErr(t, svc.MoveToTransit(bin.ID), "MoveToTransit")

	got, err := db.GetBin(bin.ID)
	if err != nil {
		t.Fatalf("get bin: %v", err)
	}

	transit, err := db.GetNodeByName(domain.TransitNodeName)
	if err != nil {
		t.Fatalf("lookup transit node: %v", err)
	}
	if got.NodeID == nil || *got.NodeID != transit.ID {
		t.Errorf("after MoveToTransit, bin.NodeID = %v, want %d (_TRANSIT)", got.NodeID, transit.ID)
	}
	if got.ClaimedBy == nil || *got.ClaimedBy != orderID {
		t.Errorf("after MoveToTransit, bin.ClaimedBy = %v, want %d (claim must persist through transit — anomaly detection depends on this)", got.ClaimedBy, orderID)
	}
}

// TestMoveToTransit_Idempotent locks down the retry-safety property.
// Vendor pickup events can fire twice (poller race, fleet adapter
// resends, etc.). A second MoveToTransit on a bin already in transit
// must be a no-op, not an error.
func TestMoveToTransit_Idempotent(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "TR-BT-IDEM", Description: "idempotency test"}
	db.CreateBinType(bt)
	source := &nodes.Node{Name: "TR-SOURCE-IDEM", Enabled: true}
	db.CreateNode(source)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "TR-BIN-IDEM", NodeID: &source.ID, Status: "available"}
	db.CreateBin(bin)
	idemOrder := testdb.CreateOrder(t, db)
	testdb.ClaimBinForTest(t, db, bin.ID, idemOrder.ID)

	testutil.MustNoErr(t, svc.MoveToTransit(bin.ID), "first MoveToTransit")
	testutil.MustNoErr(t, svc.MoveToTransit(bin.ID), "second MoveToTransit (should be no-op)")
}

// TestMarkAnomaly_Sets sets the anomaly_at timestamp.
func TestMarkAnomaly_Sets(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "AN-BT", Description: "anomaly test"}
	db.CreateBinType(bt)
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "AN-BIN", Status: "available"}
	db.CreateBin(bin)

	got, _ := db.GetBin(bin.ID)
	if got.AnomalyAt != nil {
		t.Fatalf("pre-condition: AnomalyAt should be nil on a fresh bin, got %v", got.AnomalyAt)
	}

	testutil.MustNoErr(t, svc.MarkAnomaly(bin.ID), "MarkAnomaly")

	got, _ = db.GetBin(bin.ID)
	if got.AnomalyAt == nil {
		t.Errorf("after MarkAnomaly, AnomalyAt should be non-nil")
	}
}

// TestClearAnomaly_Clears clears anomaly_at after the operator
// resolves the bin's location.
func TestClearAnomaly_Clears(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := newBinSvc(db)

	bt := &bins.BinType{Code: "AN-BT-CLR", Description: "anomaly clear test"}
	db.CreateBinType(bt)
	bin := &bins.Bin{BinTypeID: bt.ID, Label: "AN-BIN-CLR", Status: "available"}
	db.CreateBin(bin)

	svc.MarkAnomaly(bin.ID)
	testutil.MustNoErr(t, svc.ClearAnomaly(bin.ID), "ClearAnomaly")

	got, _ := db.GetBin(bin.ID)
	if got.AnomalyAt != nil {
		t.Errorf("after ClearAnomaly, AnomalyAt should be nil, got %v", got.AnomalyAt)
	}
}

// TestTransitNode_IsSyntheticGate is the load-bearing assertion behind
// the whole design: a synthetic _TRANSIT node, with `is_synthetic=true`,
// is automatically excluded from FindSourceFIFO / FindEmptyCompatible /
// lane finders by the existing `is_synthetic = false` filters in those
// queries. If migration v15 ever creates _TRANSIT with is_synthetic=false,
// in-flight bins would be re-claimable by other orders — a silent
// correctness break. This test guards that.
func TestTransitNode_IsSyntheticGate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	transit, err := db.GetNodeByName(domain.TransitNodeName)
	if err != nil {
		t.Fatalf("migration v15 should have created %q: %v", domain.TransitNodeName, err)
	}
	if !transit.IsSynthetic {
		t.Fatalf("_TRANSIT node MUST have is_synthetic=true — claim queries depend on that filter to exclude in-flight bins. Got is_synthetic=false.")
	}
}
