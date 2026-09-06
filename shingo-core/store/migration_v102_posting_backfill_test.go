//go:build docker

package store_test

import (
	"strings"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/material"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// boundaryNode creates a node for a cms_transactions row to point at.
// cms_transactions.node_id carries a foreign key, so node_id 0 is refused.
func boundaryNode(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n := &nodes.Node{Name: name, IsSynthetic: true, Enabled: true}
	if err := db.CreateNode(n); err != nil {
		t.Fatalf("create node %s: %v", name, err)
	}
	return n.ID
}

// TestV102_HopkinsvilleDayOneCardIsNotRed is the scenario the backfill exists
// for, at the level a person actually sees.
//
// Hopkinsville carries 355 cms_transactions rows that predate the feed
// (Springfield 1109; measured 2026-09-05). Every fixture in the suite starts
// from an empty table, so day-one behaviour at a plant WITH history was
// invisible — and it is the only behaviour that matters on the day of the
// deploy. Unbackfilled, the card reads "355 transactions have been recorded but
// never queued for posting", ranked above most real findings, permanently,
// with no acknowledge path.
//
// A smaller number than 355 here; the count is not the point, the ranking is.
func TestV102_HopkinsvilleDayOneCardIsNotRed(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	nodeID := boundaryNode(t, db, "V102-DAYONE")

	legacy := make([]*cms.Transaction, 0, 12)
	for i := 0; i < 12; i++ {
		legacy = append(legacy, &cms.Transaction{
			NodeID: nodeID, NodeName: "V102-DAYONE", CatID: "PART-OLD",
			Delta: -1, SourceType: cms.SourceTypeMovement,
		})
	}
	if err := db.CreateCMSTransactions(legacy); err != nil {
		t.Fatalf("seed legacy transactions: %v", err)
	}
	// Return them to their pre-v102 shape, then let v102 run over them again.
	if _, err := db.Exec(`UPDATE cms_transactions SET posting_id = NULL`); err != nil {
		t.Fatalf("pre-v102 shape: %v", err)
	}
	if _, err := db.Exec(`UPDATE cms_transactions SET posting_id = 0 WHERE posting_id IS NULL`); err != nil {
		t.Fatalf("apply the v102 backfill: %v", err)
	}
	// A tagged boundary and one successful post: the rest of what "healthy"
	// requires, so the assertion is about the legacy rows and nothing else.
	if err := db.SetNodeProperty(nodeID, material.CMSStoreroomProperty, "SM01"); err != nil {
		t.Fatalf("tag boundary: %v", err)
	}
	p := &cms.Posting{BodySHA: "sha-dayone"}
	if err := cms.CreatePosting(db.DB, p); err != nil {
		t.Fatalf("CreatePosting: %v", err)
	}
	if err := cms.MarkInflight(db.DB, p.ID, "sha-dayone"); err != nil {
		t.Fatalf("MarkInflight: %v", err)
	}
	if err := cms.MarkPosted(db.DB, p.ID, "MW-DAYONE", 200); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}

	svc := service.NewCMSPostingService(db, 24*time.Hour)
	h, err := svc.Health(service.ProcessState{Enabled: true})
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	h.Verdict()

	if strings.Contains(h.Why, "never queued") {
		t.Errorf("the day-one card reports the plant's history as unqueued work: %q. There "+
			"is no acknowledge path, so it would say this forever and bury every real "+
			"finding under it.", h.Why)
	}
	if !h.Healthy {
		t.Errorf("a plant with history, a tagged boundary and a successful post is not "+
			"healthy: %s", h.Why)
	}
}

// v102 adds cms_transactions.posting_id, which is NULL for every row that
// predates it — and NULL is the health surface's definition of "recorded but
// never queued for posting". Springfield carries 1109 such rows and
// Hopkinsville 355 (measured 2026-09-05), so without the backfill HK's
// diagnostics card reads "355 transactions have been recorded but never queued"
// from the first minute after the deploy, forever, ranked above most real
// findings.
//
// Every fixture in the suite starts from an empty cms_transactions, which is
// why nothing caught this: day-one behaviour at a plant with history was
// invisible to the tests. This one supplies that input.
func TestV102_BackfillsLegacyRowsWithZero(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)

	// A row as it exists before the feed: no posting has ever claimed it, and
	// none ever will. cms.Create deliberately does not bind posting_id, so this
	// lands NULL exactly as a pre-v102 row does after the ALTER.
	legacy := []*cms.Transaction{{
		NodeID: boundaryNode(t, db, "V102-LEGACY"), NodeName: "V102-LEGACY",
		CatID: "PART-LEGACY", Delta: -5, SourceType: "movement",
	}}
	if err := db.CreateCMSTransactions(legacy); err != nil {
		t.Fatalf("seed legacy transaction: %v", err)
	}
	if _, err := db.Exec(`UPDATE cms_transactions SET posting_id = NULL WHERE id = $1`,
		legacy[0].ID); err != nil {
		t.Fatalf("return the row to its pre-v102 shape: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 102`); err != nil {
		t.Fatalf("clear v102 row: %v", err)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v102: %v", err)
	}
	defer migrated.Close()

	var postingID *int64
	if err := migrated.QueryRow(`SELECT posting_id FROM cms_transactions WHERE id = $1`,
		legacy[0].ID).Scan(&postingID); err != nil {
		t.Fatalf("read back posting_id: %v", err)
	}
	if postingID == nil {
		t.Fatal("a legacy transaction is still posting_id IS NULL after v102 — " +
			"the health card will report it as unposted work forever")
	}
	if *postingID != 0 {
		t.Errorf("posting_id = %d, want the 0 sentinel meaning 'predates the CMS integration'",
			*postingID)
	}

	// The assertion that matters is not the column value but what the health
	// surface makes of it, because the column is only a problem through that
	// reader.
	h, err := cms.PostingHealth(migrated.DB, 24*time.Hour)
	if err != nil {
		t.Fatalf("posting health: %v", err)
	}
	if h.Unposted != 0 {
		t.Errorf("unposted_transaction_count = %d, want 0 — a backfilled legacy row is not "+
			"work the subscriber failed to enqueue", h.Unposted)
	}
}

// TestV102_BackfillDoesNotClaimNewWork is the selectivity half. The backfill
// runs once, at migration time; a transaction recorded afterwards must still
// arrive NULL, because NULL IS the queue. A backfill that also swallowed new
// rows would silence the one signal that says the subscriber has stopped
// enqueueing.
func TestV102_BackfillDoesNotClaimNewWork(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	fresh := []*cms.Transaction{{
		NodeID: boundaryNode(t, db, "V102-FRESH"), NodeName: "V102-FRESH",
		CatID: "PART-FRESH", Delta: 7, SourceType: "movement",
	}}
	if err := db.CreateCMSTransactions(fresh); err != nil {
		t.Fatalf("create transaction: %v", err)
	}

	h, err := cms.PostingHealth(db.DB, 24*time.Hour)
	if err != nil {
		t.Fatalf("posting health: %v", err)
	}
	if h.Unposted != 1 {
		t.Errorf("unposted_transaction_count = %d, want 1 — a transaction no posting has "+
			"claimed is exactly what this count exists to show", h.Unposted)
	}
}
