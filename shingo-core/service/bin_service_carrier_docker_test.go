//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// A bin riding a robot's deck lives on a synthetic `_ROBOT:<vehicle>` node with
// no claim — which is the literal definition ListAnomalousTransitBins matched,
// so it listed them as bins needing physical recovery. They are the opposite of
// lost: their location is known exactly. These pin both halves of the fix.

// seedCarriedBin parks a bin on a carrier node, the way branch B does.
func seedCarriedBin(t *testing.T, db *store.DB, vehicle string) *bins.Bin {
	t.Helper()
	carrier := &nodes.Node{Name: bins.CarrierNodePrefix + vehicle, IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carrier), "create carrier node")

	bt := &bins.BinType{Code: "CAR-" + vehicle, Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "riding-" + vehicle, NodeID: &carrier.ID, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(bin), "create bin")
	return bin
}

func TestAnomalies_ExcludeBinsRidingARobot(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	riding := seedCarriedBin(t, db, "AMR-03")

	listed, err := db.ListAnomalousTransitBins()
	testutil.MustNoErr(t, err, "list anomalies")
	for _, b := range listed {
		if b.ID == riding.ID {
			t.Fatalf("bin %d on %s is listed as a transit anomaly — it is on a robot, not lost; "+
				"an operator would be sent to find a bin that is moving", b.ID, b.NodeName)
		}
	}
}

// The stray-at-a-node-group shape the synthetic widening exists for must STILL
// be listed — the carrier exclusion is a PREFIX, not a narrowing back to
// _TRANSIT. TestListAnomalies_SeesAStrayOnAnySyntheticNode
// (bin_service_arrival_test.go) is that guard and needs no second spelling here;
// it fails if anyone replaces the prefix with a name equality.

// And the recovery action itself refuses, so a stale page or a hand-made request
// cannot move a bin off a robot that is still carrying it.
func TestRecoverTransitAnomaly_RefusesABinOnACarrierNode(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewBinService(db, NewBinManifestService(db, EpochAnnounce{}))

	riding := seedCarriedBin(t, db, "AMR-07")
	dest := &nodes.Node{Name: "REAL-SLOT-1", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dest), "create dest node")

	err := svc.RecoverTransitAnomaly(riding.ID, dest.ID, "operator.test", "")
	if err == nil {
		t.Fatal("recovery was allowed on a bin riding a robot — that records the bin at a node " +
			"the floor would be sent to, and leaves the real one to be placed again on unload")
	}
	if !strings.Contains(err.Error(), "riding a robot") {
		t.Errorf("refusal should say why: %v", err)
	}
	b, err := db.GetBin(riding.ID)
	testutil.MustNoErr(t, err, "get bin")
	if b.NodeName != bins.CarrierNodePrefix+"AMR-07" {
		t.Errorf("bin moved to %q despite the refusal", b.NodeName)
	}
}
