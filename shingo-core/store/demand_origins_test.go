//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
)

func originAt(key, id string, rev int64, opened time.Time) store.DemandOrigin {
	return store.DemandOrigin{
		OriginID: id, Revision: rev, EpisodeKey: key, Kind: "cell",
		Direction: "supply", StationID: "PLANT.LINE1", ProcessID: "SNF2",
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

// ── The maintained-group keeper's two reads ────────────────────────────────

// ListOpenEpisodesOfKind is the kind-parameterised read that
// ListOpenThresholdEpisodes became an adapter over. The keeper holds NO
// in-memory episode state, so this read IS its memory — which makes "does it
// return exactly the open episodes of the kind asked for, and nothing else" the
// property everything downstream rests on.
func TestListOpenEpisodesOfKind_SeparatesKinds(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()

	thr := store.DemandOrigin{
		OriginID: "aaaaaaaa-1111-2222-3333-444444444444", EpisodeKey: protocol.ThresholdEpisodeKey("SLN_002", "PANEL-A"),
		Kind: protocol.EpisodeKindThreshold, StationID: "PLANT.LINE1",
		CoreNodeName: "SLN_002", PayloadCode: "PANEL-A", OpenedAt: now,
	}
	mnt := store.DemandOrigin{
		OriginID: "bbbbbbbb-1111-2222-3333-444444444444", EpisodeKey: protocol.MaintainEpisodeKey("SYN_EMPTIES", "45x58x32"),
		Kind: protocol.EpisodeKindMaintain, StationID: "PLANT.LINE1",
		CoreNodeName: "SYN_EMPTIES", OpenedAt: now,
	}
	testutil.MustNoErr(t, db.OpenCoreEpisode(thr, false), "mint threshold")
	testutil.MustNoErr(t, db.OpenCoreEpisode(mnt, false), "mint maintain")

	gotMnt, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "ListOpenEpisodesOfKind maintain")
	if len(gotMnt) != 1 || gotMnt[0].OriginID != "bbbbbbbb-1111-2222-3333-444444444444" {
		t.Fatalf("maintain episodes = %+v, want only kind-mnt-1", gotMnt)
	}
	// The kind is stamped back onto the row from the argument, so a caller can
	// read it without a second lookup.
	if gotMnt[0].Kind != protocol.EpisodeKindMaintain {
		t.Errorf("Kind = %q, want %q", gotMnt[0].Kind, protocol.EpisodeKindMaintain)
	}

	// The adapter still answers only its own kind.
	gotThr, err := db.ListOpenThresholdEpisodes()
	testutil.MustNoErr(t, err, "ListOpenThresholdEpisodes")
	if len(gotThr) != 1 || gotThr[0].OriginID != "aaaaaaaa-1111-2222-3333-444444444444" {
		t.Fatalf("threshold episodes = %+v, want only kind-thr-1", gotThr)
	}

	// A CLOSED maintain episode leaves the open set. The keeper re-reads this
	// every tick, so a settled episode that kept showing up would suppress the
	// next mint for that group and type forever.
	closed, err := db.CloseDemandOriginByID("bbbbbbbb-1111-2222-3333-444444444444", protocol.CloseReasonRecovered,
		protocol.ClosedByNotification, now.Add(time.Minute))
	testutil.MustNoErr(t, err, "close maintain")
	if !closed {
		t.Fatal("close reported no row moved")
	}
	gotMnt, err = db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "ListOpenEpisodesOfKind after close")
	if len(gotMnt) != 0 {
		t.Errorf("maintain episodes after close = %+v, want none", gotMnt)
	}
}

// MaintainedEpisodeForOrigin is the sourcing side's whole view of the typed ask:
// the group it belongs to and the carrier type it is short of, from one read.
//
// The blank-origin case is the one that MUST NOT reach the database:
// orders.origin_id is a UUID column and comparing it to "" is a type error at
// Postgres, not an empty result — and every non-maintainer order in the plant
// carries a blank origin, so this is the overwhelmingly common call.
func TestMaintainedEpisodeForOrigin(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	now := time.Now().UTC()

	testutil.MustNoErr(t, db.OpenCoreEpisode(store.DemandOrigin{
		OriginID:   "11111111-2222-3333-4444-555555555555",
		EpisodeKey: protocol.MaintainEpisodeKey("SYN_EMPTIES", "45x58x32"),
		Kind:       protocol.EpisodeKindMaintain, StationID: "PLANT.LINE1",
		CoreNodeName: "SYN_EMPTIES", OpenedAt: now,
	}, false), "mint maintain")

	group, got, err := db.MaintainedEpisodeForOrigin("11111111-2222-3333-4444-555555555555")
	testutil.MustNoErr(t, err, "MaintainedEpisodeForOrigin")
	if got != "45x58x32" {
		t.Errorf("type = %q, want 45x58x32", got)
	}
	// THE GROUP IS THE OTHER HALF, and it is load-bearing: it is what keeps a
	// top-off ask from sourcing out of the group it is filling.
	if group != "SYN_EMPTIES" {
		t.Errorf("group = %q, want SYN_EMPTIES", group)
	}

	// BLANK ORIGIN: no query, no error, no type. If this ever reaches Postgres
	// it fails with a UUID cast error rather than returning nothing.
	group, got, err = db.MaintainedEpisodeForOrigin("")
	testutil.MustNoErr(t, err, "MaintainedEpisodeForOrigin blank")
	if got != "" || group != "" {
		t.Errorf("blank origin gave group=%q type=%q, want both empty", group, got)
	}

	// An origin that is not a maintain episode is not an error — it is the
	// ordinary answer for every other order in the plant.
	testutil.MustNoErr(t, db.OpenCoreEpisode(store.DemandOrigin{
		OriginID:   "99999999-8888-7777-6666-555555555555",
		EpisodeKey: protocol.ThresholdEpisodeKey("SLN_002", "PANEL-A"),
		Kind:       protocol.EpisodeKindThreshold, StationID: "PLANT.LINE1",
		CoreNodeName: "SLN_002", PayloadCode: "PANEL-A", OpenedAt: now,
	}, false), "mint threshold")
	group, got, err = db.MaintainedEpisodeForOrigin("99999999-8888-7777-6666-555555555555")
	testutil.MustNoErr(t, err, "MaintainedEpisodeForOrigin threshold origin")
	if got != "" || group != "" {
		t.Errorf("threshold origin gave group=%q type=%q, want both empty. A non-maintain "+
			"episode has neither, and returning its core node as a `group` would hand the "+
			"finder a subtree to exclude that nothing asked it to exclude", group, got)
	}

	// OPEN ONLY. A closed episode's type is history; an ask still live against a
	// settled episode must fall through to the ordinary derivation rather than
	// keep sourcing for a demand nobody is counting.
	_, err = db.CloseDemandOriginByID("11111111-2222-3333-4444-555555555555",
		protocol.CloseReasonRecovered, protocol.ClosedByNotification, now.Add(time.Minute))
	testutil.MustNoErr(t, err, "close maintain")
	group, got, err = db.MaintainedEpisodeForOrigin("11111111-2222-3333-4444-555555555555")
	testutil.MustNoErr(t, err, "MaintainedEpisodeForOrigin after close")
	if got != "" || group != "" {
		t.Errorf("closed episode gave group=%q type=%q, want both empty", group, got)
	}
}
