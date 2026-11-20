//go:build docker

package service

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/material"
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
		CatID:      catID,
		Delta:      delta,
		SourceType: "movement",
	}
	testutil.MustNoErr(t, db.CreateCMSTransactions([]*cms.Transaction{tx}), "CreateCMSTransactions")
	if tx.ID == 0 {
		t.Fatal("expected Create to populate ID")
	}
	return tx
}

func TestCMSTransactionService_ListByNode_FiltersByNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	nodeA := &nodes.Node{Name: "CMS-NA", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "create nodeA")
	nodeB := &nodes.Node{Name: "CMS-NB", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeB), "create nodeB")

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
	t.Parallel()
	db := testDB(t)
	node := &nodes.Node{Name: "CMS-PG", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")

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
	t.Parallel()
	db := testDB(t)
	nodeA := &nodes.Node{Name: "CMS-ALL-A", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeA), "create nodeA")
	nodeB := &nodes.Node{Name: "CMS-ALL-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(nodeB), "create nodeB")

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

// TestCMSPostingService_HistoryThatPredatesTheFeedIsHealthy: a plant whose
// cms_transactions predate the feed (posting_id 0, the sentinel its history
// carries), with a tagged boundary and one successful post, reads healthy on
// the diagnostics card. The same rows unqueued (NULL) read as the
// "never queued" finding first, so the card is shown to carry that finding
// before its absence is asserted. Read as unposted work, the history would
// head the card forever with no acknowledge path. It replaces store's
// deleted TestV102_HopkinsvilleDayOneCardIsNotRed, which never ran a migration.
func TestCMSPostingService_HistoryThatPredatesTheFeedIsHealthy(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	node := &nodes.Node{Name: "CMS-DAYONE", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(node), "create node")
	history := make([]*cms.Transaction, 0, 12)
	for i := 0; i < 12; i++ {
		history = append(history, &cms.Transaction{
			NodeID: node.ID, NodeName: node.Name, CatID: "PART-OLD",
			Delta: -1, SourceType: cms.SourceTypeMovement,
		})
	}
	testutil.MustNoErr(t, db.CreateCMSTransactions(history), "seed the history")
	// A tagged boundary and one successful post: the rest of what healthy
	// requires, so the verdict is about the history and nothing else.
	testutil.MustNoErr(t, db.SetNodeProperty(node.ID, material.CMSStoreroomProperty, "SM01"), "tag boundary")
	p := &cms.Posting{BodySHA: "sha-dayone"}
	testutil.MustNoErr(t, cms.CreatePosting(db.DB, p), "CreatePosting")
	testutil.MustNoErr(t, cms.MarkInflight(db.DB, p.ID, "sha-dayone"), "MarkInflight")
	testutil.MustNoErr(t, cms.MarkPosted(db.DB, p.ID, "MW-DAYONE", 200), "MarkPosted")

	svc := NewCMSPostingService(db, 24*time.Hour)
	card := func() *FeedHealth {
		t.Helper()
		h, err := svc.Health(ProcessState{Enabled: true})
		testutil.MustNoErr(t, err, "Health")
		h.Verdict()
		return h
	}
	if h := card(); h.Healthy || !strings.Contains(h.Why, "never queued") {
		t.Fatalf("with the history unqueued the card reads healthy=%v %q, want the never-queued finding: "+
			"the assertion below would pass for the wrong reason", h.Healthy, h.Why)
	}

	_, err := db.Exec(`UPDATE cms_transactions SET posting_id = 0 WHERE posting_id IS NULL`)
	testutil.MustNoErr(t, err, "mark the history as predating the feed")
	h := card()
	if strings.Contains(h.Why, "never queued") {
		t.Errorf("the card reports the plant's history as unqueued work: %q", h.Why)
	}
	if !h.Healthy {
		t.Errorf("a plant with history, a tagged boundary and a successful post is not healthy: %s", h.Why)
	}
}
