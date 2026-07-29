//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
)

func originAt(key, id string, rev int64, opened time.Time) store.DemandOrigin {
	return store.DemandOrigin{
		OriginID: id, Revision: rev, EpisodeKey: key, Kind: "cell",
		Direction: "supply", StationID: "PLANT.LINE1", ProcessID: 42,
		PayloadCode: "PANEL-B", OpenedAt: opened,
	}
}

// THE GUARD IS THE WHOLE REASON THIS IS STATE AND NOT EVENTS. A duplicate
// delivery must be a no-op and a reordered pair must resolve by comparison,
// structurally — not by a handler remembering what it has already seen.
func TestUpsertDemandOrigin_RevisionGuard(t *testing.T) {
	db := testdb.Open(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	closed := opened.Add(30 * time.Minute)

	first := originAt("cell|PLANT.LINE1|42|PANEL-B|supply", "aaaaaaaa-0000-0000-0000-000000000001", 1, opened)
	first.OpenedTotal = 40
	if err := db.UpsertDemandOrigin(first); err != nil {
		t.Fatalf("insert rev 1: %v", err)
	}

	// The close, at a higher revision — applies.
	closeMsg := first
	closeMsg.Revision = 2
	closeMsg.ClosedAt = &closed
	closeMsg.CloseReason = "recovered"
	if err := db.UpsertDemandOrigin(closeMsg); err != nil {
		t.Fatalf("apply rev 2: %v", err)
	}
	got, err := db.GetDemandOrigin(first.OriginID)
	if err != nil || got == nil {
		t.Fatalf("read back: %v", err)
	}
	if got.CloseReason != "recovered" || got.ClosedAt == nil {
		t.Fatalf("rev 2 must apply: close_reason=%q closed_at=%v", got.CloseReason, got.ClosedAt)
	}

	// THE REORDERED CASE: the open arrives again, late, at its original
	// revision. It must LOSE — a stale message that re-opened a closed episode
	// would resurrect a demand that already ended.
	stale := first
	stale.OpenedTotal = 999
	if err := db.UpsertDemandOrigin(stale); err != nil {
		t.Fatalf("replay rev 1: %v", err)
	}
	got, _ = db.GetDemandOrigin(first.OriginID)
	if got.ClosedAt == nil || got.CloseReason != "recovered" {
		t.Error("a lower revision re-opened a closed episode — the guard did not hold")
	}
	if got.OpenedTotal != 40 {
		t.Errorf("opened_total = %d, want 40 — the stale message overwrote a live field", got.OpenedTotal)
	}

	// AND THE DUPLICATE: same revision, redelivered. Equal is not greater, so
	// it is a no-op rather than a rewrite.
	dup := closeMsg
	dup.CloseReason = "changed-underneath"
	if err := db.UpsertDemandOrigin(dup); err != nil {
		t.Fatalf("redeliver rev 2: %v", err)
	}
	got, _ = db.GetDemandOrigin(first.OriginID)
	if got.CloseReason != "recovered" {
		t.Errorf("close_reason = %q — an equal revision must be a no-op", got.CloseReason)
	}
}

// THE LAST MESSAGE IS SUFFICIENT ON ITS OWN. Lose everything except the close
// and Core still converges — that is the property state transfer buys and
// events cannot, where a lost `opened` is unrecoverable.
func TestUpsertDemandOrigin_CloseAloneConverges(t *testing.T) {
	db := testdb.Open(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)
	closed := opened.Add(20 * time.Minute)

	only := originAt("cell|PLANT.LINE1|77|PANEL-C|supply", "aaaaaaaa-0000-0000-0000-000000000002", 4, opened)
	only.ClosedAt = &closed
	only.CloseReason = "recovered"
	if err := db.UpsertDemandOrigin(only); err != nil {
		t.Fatalf("close with no prior open: %v", err)
	}

	got, err := db.GetDemandOrigin(only.OriginID)
	if err != nil || got == nil {
		t.Fatalf("a close alone must produce a complete episode: %v", err)
	}
	if got.ClosedAt == nil || got.OpenedAt.IsZero() {
		t.Error("the episode should carry both ends — the close message contains the whole row")
	}
}

// SUPERSEDE: the gap between the two identities.
//
// The revision guard orders messages within one origin_id; the partial unique
// index enforces one open episode per episode_key. A close for A that fails to
// publish while B's open succeeds lands B at a key A still holds — and the
// drainer does not stop at a failed message, so this is reachable rather than
// theoretical.
func TestSupersedeOpenEpisode_YieldsToTheRealClose(t *testing.T) {
	db := testdb.Open(t)
	key := "cell|PLANT.LINE1|99|PANEL-D|supply"
	openedA := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Millisecond)
	openedB := openedA.Add(time.Hour)

	a := originAt(key, "aaaaaaaa-0000-0000-0000-00000000000a", 1, openedA)
	if err := db.UpsertDemandOrigin(a); err != nil {
		t.Fatalf("open A: %v", err)
	}

	// B opens at a place A still holds. Without the supersede this insert
	// violates the partial unique index and the NEWER episode is lost.
	n, err := db.SupersedeOpenEpisode(key, "aaaaaaaa-0000-0000-0000-00000000000b", openedB)
	if err != nil {
		t.Fatalf("supersede: %v", err)
	}
	if n != 1 {
		t.Fatalf("superseded %d episodes, want 1", n)
	}
	b := originAt(key, "aaaaaaaa-0000-0000-0000-00000000000b", 1, openedB)
	if err := db.UpsertDemandOrigin(b); err != nil {
		t.Fatalf("open B after supersede: %v", err)
	}

	gotA, _ := db.GetDemandOrigin(a.OriginID)
	if gotA.CloseReason != store.CloseReasonSuperseded || gotA.ClosedAt == nil {
		t.Fatalf("A should be superseded, got reason=%q closed=%v", gotA.CloseReason, gotA.ClosedAt)
	}
	// THE REVISION MUST NOT HAVE MOVED. It is what lets the real close win.
	if gotA.Revision != 1 {
		t.Fatalf("supersede bumped A's revision to %d — Core's guess would now outrank the truth", gotA.Revision)
	}

	// A's real close finally arrives, at its own higher revision.
	realClosedAt := openedA.Add(45 * time.Minute)
	realClose := a
	realClose.Revision = 2
	realClose.ClosedAt = &realClosedAt
	realClose.CloseReason = "recovered"
	if err := db.UpsertDemandOrigin(realClose); err != nil {
		t.Fatalf("apply A's real close: %v", err)
	}
	gotA, _ = db.GetDemandOrigin(a.OriginID)
	if gotA.CloseReason != "recovered" {
		t.Errorf("close_reason = %q, want \"recovered\" — the placeholder must yield to the real close",
			gotA.CloseReason)
	}
}

// CORE-OWNED FIELDS SURVIVE AN EDGE MESSAGE.
//
// signal_count and uop_delivered are accumulated on Core from its own signals
// and its own audit trail; the Edge message does not carry them. Listing them
// in the upsert's SET clause would zero Core's facts on every subsequent
// message — silently, and only for episodes that get more than one, which is
// every episode that ever closes.
func TestUpsertDemandOrigin_DoesNotZeroCoreOwnedFields(t *testing.T) {
	db := testdb.Open(t)
	opened := time.Now().UTC().Add(-time.Hour).Truncate(time.Millisecond)

	o := originAt("cell|PLANT.LINE1|55|PANEL-E|supply", "aaaaaaaa-0000-0000-0000-00000000000c", 1, opened)
	if err := db.UpsertDemandOrigin(o); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if _, err := db.Exec(
		`UPDATE demand_origins SET signal_count = 7, uop_delivered = 350, used_edge_reports = true
		  WHERE origin_id = $1`, o.OriginID); err != nil {
		t.Fatalf("seed core-owned fields: %v", err)
	}

	next := o
	next.Revision = 2
	next.RerequestCount = 3
	if err := db.UpsertDemandOrigin(next); err != nil {
		t.Fatalf("apply rev 2: %v", err)
	}

	var signals, uop int
	var usedEdge bool
	if err := db.QueryRow(
		`SELECT signal_count, uop_delivered, used_edge_reports FROM demand_origins WHERE origin_id = $1`,
		o.OriginID).Scan(&signals, &uop, &usedEdge); err != nil {
		t.Fatalf("read core-owned fields: %v", err)
	}
	if signals != 7 || uop != 350 || !usedEdge {
		t.Errorf("an Edge message zeroed Core's own fields: signal_count=%d uop_delivered=%d used_edge_reports=%v",
			signals, uop, usedEdge)
	}
}
