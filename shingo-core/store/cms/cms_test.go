//go:build docker

package cms_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

func TestCoverage_CreateAndListCMSTransactions(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	nodeA := &nodes.Node{Name: "CMS-NODE-A", Enabled: true}
	if err := nodes.Create(db.DB, nodeA); err != nil { t.Fatalf("create nodeA: %v", err) }
	nodeB := &nodes.Node{Name: "CMS-NODE-B", Enabled: true}
	if err := nodes.Create(db.DB, nodeB); err != nil { t.Fatalf("create nodeB: %v", err) }
	txns := []*cms.Transaction{
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "arrival", CatID: "CAT-1", Delta: 5, QtyAfter: 5, SourceType: "movement"},
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "departure", CatID: "CAT-1", Delta: -2, QtyBefore: 5, QtyAfter: 3, SourceType: "movement"},
		{NodeID: nodeA.ID, NodeName: "CMS-NODE-A", TxnType: "arrival", CatID: "CAT-2", Delta: 7, QtyAfter: 7, SourceType: "movement"},
		{NodeID: nodeB.ID, NodeName: "CMS-NODE-B", TxnType: "arrival", CatID: "CAT-1", Delta: 3, QtyAfter: 3, SourceType: "movement"},
	}
	if err := cms.Create(db.DB, txns); err != nil { t.Fatalf("Create: %v", err) }
	for i, tx := range txns { if tx.ID == 0 { t.Errorf("txn[%d].ID not assigned", i) } }
	got, err := cms.ListByNode(db.DB, nodeA.ID, 10, 0)
	if err != nil { t.Fatalf("ListByNode: %v", err) }
	if len(got) != 3 { t.Fatalf("nodeA txn count = %d, want 3", len(got)) }
	if !(got[0].ID > got[1].ID && got[1].ID > got[2].ID) { t.Errorf("not in DESC id order") }
	for _, r := range got { if r.NodeID != nodeA.ID { t.Errorf("row NodeID = %d, want %d", r.NodeID, nodeA.ID) } }
	page1, err := cms.ListByNode(db.DB, nodeA.ID, 2, 0)
	if err != nil { t.Fatalf("list page 1: %v", err) }
	if len(page1) != 2 { t.Fatalf("page1 len = %d, want 2", len(page1)) }
	page2, err := cms.ListByNode(db.DB, nodeA.ID, 2, 2)
	if err != nil { t.Fatalf("list page 2: %v", err) }
	if len(page2) != 1 { t.Fatalf("page2 len = %d, want 1", len(page2)) }
	if page1[0].ID == page2[0].ID { t.Error("page1 and page2 should not overlap") }
	all, err := cms.ListAll(db.DB, 10, 0)
	if err != nil { t.Fatalf("ListAll: %v", err) }
	if len(all) != 4 { t.Errorf("all len = %d, want 4", len(all)) }
}
