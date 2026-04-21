//go:build docker

package store

import "testing"

func TestCreateAndListCMSTransactions(t *testing.T) {
	db := testDB(t)

	// Create two nodes — cms_transactions.node_id is NOT NULL FK.
	nodeA := &Node{Name: "CMS-NODE-A", Enabled: true}
	if err := db.CreateNode(nodeA); err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB := &Node{Name: "CMS-NODE-B", Enabled: true}
	if err := db.CreateNode(nodeB); err != nil {
		t.Fatalf("create nodeB: %v", err)
	}

	// Batch-insert a handful of transactions across both nodes.
	txns := []*CMSTransaction{
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "arrival", CatID: "CAT-1", Delta: 5, QtyAfter: 5, SourceType: "movement"},
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "departure", CatID: "CAT-1", Delta: -2, QtyBefore: 5, QtyAfter: 3, SourceType: "movement"},
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "arrival", CatID: "CAT-2", Delta: 7, QtyAfter: 7, SourceType: "movement"},
		{NodeID: nodeB.ID, NodeName: "CMS-NODE-B", TxnType: "arrival", CatID: "CAT-1", Delta: 3, QtyAfter: 3, SourceType: "movement"},
	}
	if err := db.CreateCMSTransactions(txns); err != nil {
		t.Fatalf("CreateCMSTransactions: %v", err)
	}
	for i, tx := range txns {
		if tx.ID == 0 {
			t.Errorf("txn[%d].ID not assigned", i)
		}
	}

	// ListCMSTransactions(nodeA) — should return 3 rows in DESC id order
	got, err := db.ListCMSTransactions(nodeA.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListCMSTransactions: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("nodeA txn count = %d, want 3", len(got))
	}
	// Assert descending ID order
	if !(got[0].ID > got[1].ID && got[1].ID > got[2].ID) {
		t.Errorf("not in DESC id order: %d, %d, %d", got[0].ID, got[1].ID, got[2].ID)
	}
	for _, r := range got {
		if r.NodeID != nodeA.ID {
			t.Errorf("row NodeID = %d, want %d (filter leaked)", r.NodeID, nodeA.ID)
		}
	}

	// Paging: limit=2, offset=0 should give the 2 newest for nodeA.
	page1, err := db.ListCMSTransactions(nodeA.ID, 2, 0)
	if err != nil {
		t.Fatalf("list page 1: %v", err)
	}
	if len(page1) != 2 {
		t.Fatalf("page1 len = %d, want 2", len(page1))
	}

	// Paging: offset=2 — only 1 row remaining for nodeA.
	page2, err := db.ListCMSTransactions(nodeA.ID, 2, 2)
	if err != nil {
		t.Fatalf("list page 2: %v", err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len = %d, want 1", len(page2))
	}
	if page1[0].ID == page2[0].ID {
		t.Error("page1 and page2 should not overlap")
	}

	// ListAllCMSTransactions — should return all 4 rows
	all, err := db.ListAllCMSTransactions(10, 0)
	if err != nil {
		t.Fatalf("ListAllCMSTransactions: %v", err)
	}
	if len(all) != 4 {
		t.Errorf("all len = %d, want 4", len(all))
	}
}

func TestSumCatIDsAtBoundary(t *testing.T) {
	db := testDB(t)

	bt := &BinType{Code: "CMS-BT", Description: "cms tote"}
	db.CreateBinType(bt)

	// Boundary group + two child storage nodes
	boundary := &Node{Name: "CMS-BOUND", Enabled: true, IsSynthetic: true}
	db.CreateNode(boundary)
	childA := &Node{Name: "CMS-BOUND-A", Enabled: true, ParentID: &boundary.ID}
	db.CreateNode(childA)
	childB := &Node{Name: "CMS-BOUND-B", Enabled: true, ParentID: &boundary.ID}
	db.CreateNode(childB)

	// Unrelated sibling node at top level — its bin's cat_ids should NOT appear in totals
	outside := &Node{Name: "CMS-OUTSIDE", Enabled: true}
	db.CreateNode(outside)

	// Bins with manifests at each node
	b1 := &Bin{BinTypeID: bt.ID, Label: "CMS-B1", NodeID: &childA.ID, Status: "available"}
	db.CreateBin(b1)
	db.SetBinManifest(b1.ID, `{"items":[{"catid":"CAT-1","qty":10},{"catid":"CAT-2","qty":3}]}`, "P1", 10)

	b2 := &Bin{BinTypeID: bt.ID, Label: "CMS-B2", NodeID: &childB.ID, Status: "available"}
	db.CreateBin(b2)
	db.SetBinManifest(b2.ID, `{"items":[{"catid":"CAT-1","qty":5}]}`, "P1", 5)

	// Outside bin with CAT-3 — must be excluded
	bOut := &Bin{BinTypeID: bt.ID, Label: "CMS-OUT", NodeID: &outside.ID, Status: "available"}
	db.CreateBin(bOut)
	db.SetBinManifest(bOut.ID, `{"items":[{"catid":"CAT-3","qty":99}]}`, "P9", 99)

	totals := db.SumCatIDsAtBoundary(boundary.ID)

	if totals["CAT-1"] != 15 {
		t.Errorf("CAT-1 total = %d, want 15", totals["CAT-1"])
	}
	if totals["CAT-2"] != 3 {
		t.Errorf("CAT-2 total = %d, want 3", totals["CAT-2"])
	}
	if _, ok := totals["CAT-3"]; ok {
		t.Errorf("CAT-3 should not be present (not under boundary): got %d", totals["CAT-3"])
	}
}
