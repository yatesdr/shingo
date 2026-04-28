//go:build docker

package store

import (
	"testing"

	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

func TestClaimBin(t *testing.T) {
	db := testDB(t)

	bt := &bins.BinType{Code: "TOTE-CLM", Description: "Test tote"}
	db.CreateBinType(bt)

	node := &nodes.Node{Name: "STORE-CLM", Enabled: true}
	db.CreateNode(node)

	bin := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-CLM-1", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin)

	orderID := int64(42)

	// Claim
	if err := db.ClaimBin(bin.ID, orderID); err != nil {
		t.Fatalf("ClaimBin: %v", err)
	}
	got, _ := db.GetBin(bin.ID)
	if got.ClaimedBy == nil || *got.ClaimedBy != orderID {
		t.Errorf("ClaimedBy = %v, want %d", got.ClaimedBy, orderID)
	}

	// Unclaim
	if err := db.UnclaimBin(bin.ID); err != nil {
		t.Fatalf("UnclaimBin: %v", err)
	}
	got2, _ := db.GetBin(bin.ID)
	if got2.ClaimedBy != nil {
		t.Errorf("ClaimedBy after unclaim = %v, want nil", got2.ClaimedBy)
	}
}

func TestUnclaimOrderBins(t *testing.T) {
	db := testDB(t)

	bt := &bins.BinType{Code: "TOTE-UO", Description: "Test tote"}
	db.CreateBinType(bt)

	node := &nodes.Node{Name: "STORE-UO", Enabled: true}
	db.CreateNode(node)

	bin1 := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-UO-1", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin1)
	bin2 := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-UO-2", NodeID: &node.ID, Status: "available"}
	db.CreateBin(bin2)

	orderID := int64(99)
	db.ClaimBin(bin1.ID, orderID)
	db.ClaimBin(bin2.ID, orderID)

	// Unclaim all bins for order
	db.UnclaimOrderBins(orderID)

	got1, _ := db.GetBin(bin1.ID)
	got2, _ := db.GetBin(bin2.ID)
	if got1.ClaimedBy != nil {
		t.Errorf("bin1 ClaimedBy = %v, want nil", got1.ClaimedBy)
	}
	if got2.ClaimedBy != nil {
		t.Errorf("bin2 ClaimedBy = %v, want nil", got2.ClaimedBy)
	}
}

func TestFindEmptyCompatibleBin(t *testing.T) {
	db := testDB(t)

	// Setup: bin type, payload, bin type assignment
	bt := &bins.BinType{Code: "TOTE-FEC", Description: "Compatible tote"}
	db.CreateBinType(bt)

	bp := &payloads.Payload{Code: "WIDGET-FEC", UOPCapacity: 50}
	db.CreatePayload(bp)

	// Link payload to bin type
	db.SetPayloadBinTypes(bp.ID, []int64{bt.ID})

	// Create nodes in two zones
	nodeA := &nodes.Node{Name: "STORE-A1", Enabled: true, Zone: "zone-a"}
	db.CreateNode(nodeA)
	nodeB := &nodes.Node{Name: "STORE-B1", Enabled: true, Zone: "zone-b"}
	db.CreateNode(nodeB)

	// Create empty bins (no payloads)
	binA := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-FEC-A", NodeID: &nodeA.ID, Status: "available"}
	db.CreateBin(binA)
	binB := &bins.Bin{BinTypeID: bt.ID, Label: "BIN-FEC-B", NodeID: &nodeB.ID, Status: "available"}
	db.CreateBin(binB)

	// Zone preference: should find binA when preferring zone-a
	found, err := db.FindEmptyCompatibleBin("WIDGET-FEC", "zone-a", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin zone-a: %v", err)
	}
	if found.ID != binA.ID {
		t.Errorf("zone-a: got bin %d (%s), want bin %d (%s)", found.ID, found.Label, binA.ID, binA.Label)
	}

	// Zone preference: should find binB when preferring zone-b
	found2, err := db.FindEmptyCompatibleBin("WIDGET-FEC", "zone-b", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin zone-b: %v", err)
	}
	if found2.ID != binB.ID {
		t.Errorf("zone-b: got bin %d (%s), want bin %d (%s)", found2.ID, found2.Label, binB.ID, binB.Label)
	}

	// No zone preference: should find one (the first by ID)
	found3, err := db.FindEmptyCompatibleBin("WIDGET-FEC", "", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin no zone: %v", err)
	}
	if found3.ID != binA.ID {
		t.Errorf("no zone: got bin %d, want bin %d (first by ID)", found3.ID, binA.ID)
	}

	// Claimed bins should be excluded
	db.ClaimBin(binA.ID, 1)
	found4, err := db.FindEmptyCompatibleBin("WIDGET-FEC", "zone-a", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin after claim: %v", err)
	}
	// Should fall back to zone-b since zone-a bin is claimed
	if found4.ID != binB.ID {
		t.Errorf("after claim: got bin %d, want bin %d (fallback)", found4.ID, binB.ID)
	}

	// Bins with manifests should be excluded
	db.UnclaimBin(binA.ID)
	db.SetBinManifest(binA.ID, `{"items":[]}`, bp.Code, 100)
	db.ConfirmBinManifest(binA.ID, "")

	found5, err := db.FindEmptyCompatibleBin("WIDGET-FEC", "zone-a", 0)
	if err != nil {
		t.Fatalf("FindEmptyCompatibleBin after payload: %v", err)
	}
	if found5.ID != binB.ID {
		t.Errorf("after payload on binA: got bin %d, want bin %d", found5.ID, binB.ID)
	}

	// Unknown payload: post-2026-04-27 advisory enforcement means a payload
	// with no rules in payload_bin_types matches any compatible empty bin.
	// At this point binA has a manifest (above) so the only empty bin is
	// binB in zone-b. Zone-a query returns ErrNoRows; any-zone fallback
	// returns binB.
	found6, err := db.FindEmptyCompatibleBin("NONEXISTENT", "zone-a", 0)
	if err != nil {
		t.Fatalf("expected advisory fallback to return a bin, got err: %v", err)
	}
	if found6.ID != binB.ID {
		t.Errorf("nonexistent payload (advisory): got bin %d, want bin %d (binB still empty)", found6.ID, binB.ID)
	}
}
