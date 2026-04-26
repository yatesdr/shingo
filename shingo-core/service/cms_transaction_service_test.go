//go:build docker

package service

import (
	"testing"

	"shingocore/store"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// seedCMSTxn inserts a single cms_transactions row against nodeID and
// returns it.
func seedCMSTxn(t *testing.T, db *store.DB, nodeID int64, catID string, delta int64) *cms.Transaction {
	t.Helper()
	tx := &cms.Transaction{
		NodeID:     nodeID,
		NodeName:   "seed-node",
		TxnType:    "movement",
		CatID:      catID,
		Delta:      delta,
		QtyBefore:  0,
		QtyAfter:   delta,
		SourceType: "movement",
	}
	if err := db.CreateCMSTransactions([]*cms.Transaction{tx}); err != nil {
		t.Fatalf("CreateCMSTransactions: %v", err)
	}
	if tx.ID == 0 {
		t.Fatal("expected Create to populate ID")
	}
	return tx
}

func TestCMSTransactionService_ListByNode_FiltersByNode(t *testing.T) {
	db := testDB(t)
	nodeA := &nodes.Node{Name: "CMS-NA", Enabled: true}
	if err := db.CreateNode(nodeA); err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB := &nodes.Node{Name: "CMS-NB", Enabled: true}
	if err := db.CreateNode(nodeB); err != nil {
		t.Fatalf("create nodeB: %v", err)
	}

	seedCMSTxn(t, db, nodeA.ID, "CAT-1", 5)
	seedCMSTxn(t, db, nodeA.ID, "CAT-2", 3)
	seedCMSTxn(t, db, nodeB.ID, "CAT-3", 1)

	svc := NewCMSTransactionService(db)

	rows, err := svc.ListByNode(nodeA.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("len(rows) = %d, want 2", len(rows))
	}
	for _, r := range rows {
		if r.NodeID != nodeA.ID {
			t.Errorf("row.NodeID = %d, want %d", r.NodeID, nodeA.ID)
		}
	}
}

func TestCMSTransactionService_ListByNode_RespectsLimitAndOffset(t *testing.T) {
	db := testDB(t)
	node := &nodes.Node{Name: "CMS-PG", Enabled: true}
	if err := db.CreateNode(node); err != nil {
		t.Fatalf("create node: %v", err)
	}

	for i := 0; i < 5; i++ {
		seedCMSTxn(t, db, node.ID, "CAT-P", int64(i+1))
	}

	svc := NewCMSTransactionService(db)

	// First page: 2 rows.
	page1, err := svc.ListByNode(node.ID, 2, 0)
	if err != nil {
		t.Fatalf("ListByNode page1: %v", err)
	}
	if len(page1) != 2 {
		t.Errorf("page1 len = %d, want 2", len(page1))
	}

	// Second page (offset 2): also 2 rows, non-overlapping.
	page2, err := svc.ListByNode(node.ID, 2, 2)
	if err != nil {
		t.Fatalf("ListByNode page2: %v", err)
	}
	if len(page2) != 2 {
		t.Errorf("page2 len = %d, want 2", len(page2))
	}
	for _, a := range page1 {
		for _, b := range page2 {
			if a.ID == b.ID {
				t.Errorf("page1 and page2 share row ID %d", a.ID)
			}
		}
	}
}

func TestCMSTransactionService_ListAll_SpansNodes(t *testing.T) {
	db := testDB(t)
	nodeA := &nodes.Node{Name: "CMS-ALL-A", Enabled: true}
	if err := db.CreateNode(nodeA); err != nil {
		t.Fatalf("create nodeA: %v", err)
	}
	nodeB := &nodes.Node{Name: "CMS-ALL-B", Enabled: true}
	if err := db.CreateNode(nodeB); err != nil {
		t.Fatalf("create nodeB: %v", err)
	}

	seedCMSTxn(t, db, nodeA.ID, "CA", 1)
	seedCMSTxn(t, db, nodeB.ID, "CB", 2)

	svc := NewCMSTransactionService(db)
	rows, err := svc.ListAll(50, 0)
	if err != nil {
		t.Fatalf("ListAll: %v", err)
	}
	if len(rows) < 2 {
		t.Fatalf("len(rows) = %d, want >= 2", len(rows))
	}

	// Sanity: matches direct *store.DB call.
	dbRows, _ := db.ListAllCMSTransactions(50, 0)
	if len(dbRows) != len(rows) {
		t.Errorf("db rows = %d, svc rows = %d, should match", len(dbRows), len(rows))
	}
}
