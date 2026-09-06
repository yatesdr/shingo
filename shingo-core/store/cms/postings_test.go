//go:build docker

package cms_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/cms"
	"shingocore/store/nodes"
)

// seedPosting creates a pending posting and returns it.
func seedPosting(t *testing.T, db *store.DB, batchKey string) *cms.Posting {
	t.Helper()
	p := &cms.Posting{BatchKey: batchKey, BodySHA: "sha-" + batchKey}
	if err := cms.CreatePosting(db.DB, p); err != nil {
		t.Fatalf("CreatePosting: %v", err)
	}
	if p.ID == 0 {
		t.Fatal("CreatePosting did not set ID")
	}
	return p
}

func getPosting(t *testing.T, db *store.DB, id int64) *cms.Posting {
	t.Helper()
	got, err := cms.GetPosting(db.DB, id)
	if err != nil {
		t.Fatalf("GetPosting(%d): %v", id, err)
	}
	return got
}

func TestPostings_CreateStartsPendingAndReadsBack(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	p := seedPosting(t, db, "batch-1")

	got := getPosting(t, db, p.ID)
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q, want pending — a posting that has not been sent is pending", got.Status)
	}
	if got.BatchKey != "batch-1" || got.BodySHA != "sha-batch-1" {
		t.Errorf("posting = %+v, want the batch key and body sha it was created with", got)
	}
	if got.Attempts != 0 || got.RequeueCount != 0 {
		t.Errorf("attempts=%d requeues=%d, want 0/0 on a fresh row", got.Attempts, got.RequeueCount)
	}
	if got.NextRetryAt == nil {
		t.Error("next_retry_at is NULL on a fresh posting — NextPending would still take it, but the intent is 'send now'")
	}
}

// TestPostings_NextPendingIsPendingOnly is the load-bearing one. An inflight
// row is a POST that may already have landed, and this API has no idempotency
// key: handing one back to the sender books the same inventory transfer twice.
func TestPostings_NextPendingIsPendingOnly(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	pending := seedPosting(t, db, "np-pending")
	inflight := seedPosting(t, db, "np-inflight")
	posted := seedPosting(t, db, "np-posted")
	rejected := seedPosting(t, db, "np-rejected")
	failed := seedPosting(t, db, "np-failed")

	if err := cms.MarkInflight(db.DB, inflight.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight: %v", err)
	}
	if err := cms.MarkInflight(db.DB, posted.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight(posted): %v", err)
	}
	if err := cms.MarkPosted(db.DB, posted.ID, "TX-1", 200); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	if err := cms.MarkRejected(db.DB, rejected.ID, 400, "bad body"); err != nil {
		t.Fatalf("MarkRejected: %v", err)
	}
	if err := cms.MarkFailed(db.DB, failed.ID, "out of attempts"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}

	got, err := cms.NextPending(db.DB, 100)
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(got) != 1 || got[0].ID != pending.ID {
		ids := make([]int64, len(got))
		for i, p := range got {
			ids[i] = p.ID
		}
		t.Fatalf("NextPending returned %v, want only the pending row (%d)", ids, pending.ID)
	}
}

// TestPostings_NextPendingRespectsRetryTime: a row scheduled for later is not
// ready now. Without this the backoff is decorative.
func TestPostings_NextPendingRespectsRetryTime(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	later := seedPosting(t, db, "np-later")

	if err := cms.MarkPending(db.DB, later.ID, "dns", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("MarkPending: %v", err)
	}
	got, err := cms.NextPending(db.DB, 100)
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("NextPending returned %d rows, want none — the retry is an hour out", len(got))
	}

	if err := cms.MarkPending(db.DB, later.ID, "dns", time.Now().Add(-time.Minute)); err != nil {
		t.Fatalf("MarkPending(past): %v", err)
	}
	got, err = cms.NextPending(db.DB, 100)
	if err != nil {
		t.Fatalf("NextPending: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("NextPending returned %d rows, want 1 once the retry time has passed", len(got))
	}
}

// TestPostings_MarkInflightIsGuardedOnPending: two passes must not both arm the
// same row. The second is refused rather than quietly overwriting the first's
// batch key, which would leave two senders believing they own one POST.
func TestPostings_MarkInflightIsGuardedOnPending(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	p := seedPosting(t, db, "guard")

	if err := cms.MarkInflight(db.DB, p.ID, "first", "sha-1"); err != nil {
		t.Fatalf("first MarkInflight: %v", err)
	}
	if err := cms.MarkInflight(db.DB, p.ID, "second", "sha-2"); err == nil {
		t.Error("second MarkInflight succeeded — a row already inflight must not be re-armed")
	}

	got := getPosting(t, db, p.ID)
	if got.BatchKey != "first" || got.BodySHA != "sha-1" {
		t.Errorf("posting = %+v, want the FIRST arm's batch key and sha", got)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 — the refused arm must not count as a try", got.Attempts)
	}
	if got.InflightAt == nil {
		t.Error("inflight_at is NULL — the reconciler ages rows on it")
	}
}

func TestPostings_MarkPostedRecordsTheTransactionID(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	p := seedPosting(t, db, "posted")
	if err := cms.MarkInflight(db.DB, p.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight: %v", err)
	}
	if err := cms.MarkPosted(db.DB, p.ID, "MW-4242", 201); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}

	got := getPosting(t, db, p.ID)
	if got.Status != cms.StatusPosted || got.TransactionID != "MW-4242" {
		t.Errorf("posting = %+v, want posted with transaction id MW-4242", got)
	}
	if got.HTTPStatus == nil || *got.HTTPStatus != 201 {
		t.Errorf("http_status = %v, want 201", got.HTTPStatus)
	}
	if got.PostedAt == nil || got.SettledAt == nil {
		t.Error("posted_at / settled_at not stamped")
	}
	if got.NextRetryAt != nil {
		t.Error("next_retry_at survives a posted row — nothing should schedule a settled posting")
	}
}

// TestPostings_RequeueOnlyMovesInflightRows: RequeuePending is the ONLY path
// from inflight back to the send queue, and it exists because the middleware
// said it does not have the transaction. Applying it to anything else would
// re-send something whose fate is already known.
func TestPostings_RequeueOnlyMovesInflightRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	inflight := seedPosting(t, db, "rq-inflight")
	if err := cms.MarkInflight(db.DB, inflight.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight: %v", err)
	}
	if err := cms.RequeuePending(db.DB, inflight.ID); err != nil {
		t.Fatalf("RequeuePending: %v", err)
	}
	got := getPosting(t, db, inflight.ID)
	if got.Status != cms.StatusPending {
		t.Errorf("status = %q, want pending", got.Status)
	}
	if got.RequeueCount != 1 {
		t.Errorf("requeue_count = %d, want 1", got.RequeueCount)
	}
	if got.Attempts != 1 {
		t.Errorf("attempts = %d, want 1 preserved — a requeue must not refill the retry budget", got.Attempts)
	}
	if got.InflightAt != nil || got.TransactionID != "" {
		t.Errorf("posting = %+v, want inflight_at and transaction_id cleared on requeue", got)
	}

	// A posted row must not be requeued: its fate is known.
	posted := seedPosting(t, db, "rq-posted")
	if err := cms.MarkInflight(db.DB, posted.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight(posted): %v", err)
	}
	if err := cms.MarkPosted(db.DB, posted.ID, "MW-9", 200); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	if err := cms.RequeuePending(db.DB, posted.ID); err == nil {
		t.Error("RequeuePending accepted a POSTED row — that would re-send an acknowledged transfer")
	}
}

// TestPostings_ListInflightOlderThanRespectsTheSettleWindow: a POST that
// succeeded may not be queryable on the middleware side for a moment
// afterwards, so asking too early gets "no such transaction" about something
// about to exist — and that answer requeues a row that already landed.
func TestPostings_ListInflightOlderThanRespectsTheSettleWindow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	fresh := seedPosting(t, db, "sw-fresh")
	if err := cms.MarkInflight(db.DB, fresh.ID, "bk", "sha"); err != nil {
		t.Fatalf("MarkInflight: %v", err)
	}

	aged, err := cms.ListInflightOlderThan(db.DB, time.Hour)
	if err != nil {
		t.Fatalf("ListInflightOlderThan: %v", err)
	}
	if len(aged) != 0 {
		t.Errorf("a just-armed row is %d rows old enough to reconcile, want 0", len(aged))
	}

	// Age it past the window.
	if _, err := db.Exec(`UPDATE cms_postings SET inflight_at = NOW() - INTERVAL '2 hours' WHERE id=$1`, fresh.ID); err != nil {
		t.Fatalf("age the row: %v", err)
	}
	aged, err = cms.ListInflightOlderThan(db.DB, time.Hour)
	if err != nil {
		t.Fatalf("ListInflightOlderThan: %v", err)
	}
	if len(aged) != 1 || aged[0].ID != fresh.ID {
		t.Errorf("aged inflight rows = %+v, want the one that is two hours old", aged)
	}
}

// --- cms_transactions <-> cms_postings link --------------------------------

func seedTxn(t *testing.T, db *store.DB, nodeID int64, catID string, delta int64) *cms.Transaction {
	t.Helper()
	tx := &cms.Transaction{
		NodeID: nodeID, NodeName: "LINK-NODE", CatID: catID, Delta: delta,
		SourceType: "movement", Storeroom: "SM01", RobotID: "AMR-003",
	}
	if err := cms.Create(db.DB, []*cms.Transaction{tx}); err != nil {
		t.Fatalf("create txn: %v", err)
	}
	return tx
}

func TestTransactions_StoreroomAndRobotRoundTrip(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := &nodes.Node{Name: "LINK-NODE", Enabled: true}
	if err := nodes.Create(db.DB, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	seedTxn(t, db, n.ID, "CAT-RT", 5)

	got, err := cms.ListByNode(db.DB, n.ID, 10, 0)
	if err != nil {
		t.Fatalf("ListByNode: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("rows = %d, want 1", len(got))
	}
	// A new column is not done until something reads it back and asserts on
	// the value: both of these are write-only until this line exists.
	if got[0].Storeroom != "SM01" || got[0].RobotID != "AMR-003" {
		t.Errorf("row = %+v, want storeroom SM01 and robot AMR-003", got[0])
	}
	if got[0].PostingID != nil {
		t.Errorf("posting_id = %v on a fresh row, want NULL — NULL is the unsent queue", got[0].PostingID)
	}
}

func TestTransactions_AttachPostingClaimsOnlyUnclaimedRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := &nodes.Node{Name: "LINK-NODE", Enabled: true}
	if err := nodes.Create(db.DB, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := seedTxn(t, db, n.ID, "CAT-A", 1)
	b := seedTxn(t, db, n.ID, "CAT-B", 2)
	c := seedTxn(t, db, n.ID, "CAT-C", 3)

	first := seedPosting(t, db, "attach-1")
	claimed, err := cms.AttachPosting(db.DB, []int64{a.ID, b.ID}, first.ID)
	if err != nil {
		t.Fatalf("AttachPosting: %v", err)
	}
	if claimed != 2 {
		t.Fatalf("claimed = %d, want 2", claimed)
	}

	// A second posting must not be able to take rows the first holds. Two
	// postings carrying one transaction book the same transfer twice.
	second := seedPosting(t, db, "attach-2")
	claimed, err = cms.AttachPosting(db.DB, []int64{a.ID, b.ID, c.ID}, second.ID)
	if err != nil {
		t.Fatalf("AttachPosting(second): %v", err)
	}
	if claimed != 1 {
		t.Errorf("second posting claimed %d rows, want 1 — it must take only the unclaimed one", claimed)
	}

	rows, err := cms.ListByPosting(db.DB, first.ID)
	if err != nil {
		t.Fatalf("ListByPosting: %v", err)
	}
	if len(rows) != 2 || rows[0].ID != a.ID || rows[1].ID != b.ID {
		t.Errorf("first posting carries %+v, want a then b in id order", rows)
	}
}

func TestTransactions_ListUnpostedFiltersClaimedRows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := &nodes.Node{Name: "LINK-NODE", Enabled: true}
	if err := nodes.Create(db.DB, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := seedTxn(t, db, n.ID, "CAT-A", 1)
	b := seedTxn(t, db, n.ID, "CAT-B", 2)

	unposted, err := cms.ListUnposted(db.DB, 100)
	if err != nil {
		t.Fatalf("ListUnposted: %v", err)
	}
	if len(unposted) != 2 {
		t.Fatalf("unposted = %d, want 2 before anything claims them", len(unposted))
	}

	p := seedPosting(t, db, "unposted")
	if _, err := cms.AttachPosting(db.DB, []int64{a.ID}, p.ID); err != nil {
		t.Fatalf("AttachPosting: %v", err)
	}
	unposted, err = cms.ListUnposted(db.DB, 100)
	if err != nil {
		t.Fatalf("ListUnposted: %v", err)
	}
	if len(unposted) != 1 || unposted[0].ID != b.ID {
		t.Errorf("unposted = %+v, want only the unclaimed row (%d)", unposted, b.ID)
	}
}

// ── health ──────────────────────────────────────────────────────────────

// TestPostingHealth_CountsEveryStateAndAgesThem. The counts alone cannot tell
// a working queue from a stalled one — a pending row seconds old and one hours
// old are the same number — so the ages are what turn a backlog into a finding.
func TestPostingHealth_CountsEveryStateAndAgesThem(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	pending := seedPosting(t, db, "h-pending")
	inflight := seedPosting(t, db, "h-inflight")
	unresolvable := seedPosting(t, db, "h-unresolvable")
	posted := seedPosting(t, db, "h-posted")
	rejected := seedPosting(t, db, "h-rejected")
	failed := seedPosting(t, db, "h-failed")

	for _, id := range []int64{inflight.ID, unresolvable.ID, posted.ID, rejected.ID, failed.ID} {
		if err := cms.MarkInflight(db.DB, id, "bk", "sha"); err != nil {
			t.Fatalf("MarkInflight(%d): %v", id, err)
		}
	}
	// The inflight one has an id and can be reconciled; the unresolvable one
	// never got one and cannot.
	if _, err := db.Exec(`UPDATE cms_postings SET transaction_id='MW-1' WHERE id=$1`, inflight.ID); err != nil {
		t.Fatalf("set tx id: %v", err)
	}
	if err := cms.MarkPosted(db.DB, posted.ID, "MW-2", 200); err != nil {
		t.Fatalf("MarkPosted: %v", err)
	}
	if err := cms.MarkRejected(db.DB, rejected.ID, 400, "bad"); err != nil {
		t.Fatalf("MarkRejected: %v", err)
	}
	if err := cms.MarkFailed(db.DB, failed.ID, "gave up"); err != nil {
		t.Fatalf("MarkFailed: %v", err)
	}
	// Age the pending row so its age is a finding rather than a rounding.
	if _, err := db.Exec(`UPDATE cms_postings SET created_at = NOW() - INTERVAL '20 minutes' WHERE id=$1`,
		pending.ID); err != nil {
		t.Fatalf("age pending: %v", err)
	}

	h, err := cms.PostingHealth(db.DB)
	if err != nil {
		t.Fatalf("PostingHealth: %v", err)
	}
	if h.Pending != 1 || h.Inflight != 2 || h.Posted != 1 || h.Rejected != 1 || h.Failed != 1 {
		t.Errorf("counts = %+v, want 1 pending / 2 inflight / 1 posted / 1 rejected / 1 failed", h)
	}
	if h.UnresolvableInflight != 1 {
		t.Errorf("unresolvable inflight = %d, want 1 — a posting with no transaction id has no "+
			"automatic resolution and must be counted apart", h.UnresolvableInflight)
	}
	if h.PostedLastHour != 1 || h.LastPostedAt == nil {
		t.Errorf("posted_last_hour = %d, last_posted = %v — the positive evidence is missing",
			h.PostedLastHour, h.LastPostedAt)
	}
	if h.OldestPendingAgeSeconds < 60 {
		t.Errorf("oldest pending age = %ds, want the 20 minutes it was aged by — the age is "+
			"what makes a backlog a finding", h.OldestPendingAgeSeconds)
	}
	if h.OldestInflightAgeSeconds < 0 {
		t.Errorf("oldest inflight age = %d", h.OldestInflightAgeSeconds)
	}
}

// TestPostingHealth_CountsUnpostedTransactions. A transaction with no posting
// is invisible to every posting-status count, and it is the tell for a
// subscriber failing to enqueue: movements ARE being recorded and will never be
// sent, while the posting table looks quiet and healthy.
func TestPostingHealth_CountsUnpostedTransactions(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	n := &nodes.Node{Name: "LINK-NODE", Enabled: true}
	if err := nodes.Create(db.DB, n); err != nil {
		t.Fatalf("create node: %v", err)
	}
	a := seedTxn(t, db, n.ID, "CAT-A", 1)
	seedTxn(t, db, n.ID, "CAT-B", 2)

	h, err := cms.PostingHealth(db.DB)
	if err != nil {
		t.Fatalf("PostingHealth: %v", err)
	}
	if h.Unposted != 2 {
		t.Fatalf("unposted = %d, want 2", h.Unposted)
	}

	p := seedPosting(t, db, "h-unposted")
	if _, err := cms.AttachPosting(db.DB, []int64{a.ID}, p.ID); err != nil {
		t.Fatalf("AttachPosting: %v", err)
	}
	h, err = cms.PostingHealth(db.DB)
	if err != nil {
		t.Fatalf("PostingHealth: %v", err)
	}
	if h.Unposted != 1 {
		t.Errorf("unposted = %d after attaching one, want 1", h.Unposted)
	}
}

// TestPostingHealth_EmptyTableIsAllZeroes: the health read must not error or
// produce nonsense ages on a plant that has never posted. A query that failed
// here would make the diagnostics card unavailable at exactly the moment
// somebody is setting the integration up.
func TestPostingHealth_EmptyTableIsAllZeroes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	h, err := cms.PostingHealth(db.DB)
	if err != nil {
		t.Fatalf("PostingHealth on an empty table: %v", err)
	}
	if h.Pending != 0 || h.Inflight != 0 || h.Posted != 0 {
		t.Errorf("counts on an empty table = %+v", h)
	}
	if h.LastPostedAt != nil {
		t.Error("last_posted is set on a table with no posted rows")
	}
	if h.OldestPendingAgeSeconds != 0 || h.OldestInflightAgeSeconds != 0 {
		t.Errorf("ages on an empty table = %d / %d, want 0 — a NULL age must not become a "+
			"large number and read as a stalled queue",
			h.OldestPendingAgeSeconds, h.OldestInflightAgeSeconds)
	}
}
