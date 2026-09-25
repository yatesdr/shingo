package uop

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	"shingoedge/store/lineside"
	"shingoedge/store/processes"
)

// bucket_level_test.go — the pile level on the wire: what one flush sends for
// the piles written in its window, read from the table at flush.

// levelFixture is one process with two local nodes. share=true instead puts
// the second node in a second process under the SAME core name: two local
// nodes, one place at Core (a process holds a core name once).
func levelFixture(t *testing.T, prefix string, share bool) (db *store.DB, m *Mutator, procID, nodeA, nodeB int64) {
	t.Helper()
	db = newReporterTestDB(t)
	procID, err := db.CreateProcess(prefix+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	procB, coreB := procID, prefix+"-B"
	if share {
		procB, err = db.CreateProcess(prefix+"-PROC2", "", "active_production", "", "", false)
		testutil.MustNoErr(t, err, "create second process")
		coreB = prefix + "-A"
	}
	for i, n := range []struct {
		proc       int64
		code, core string
		id         *int64
	}{{procID, "CA", prefix + "-A", &nodeA}, {procB, "CB", coreB, &nodeB}} {
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: n.proc, CoreNodeName: n.core, Code: n.code, Name: n.code, Sequence: i + 1, Enabled: true,
		})
		testutil.MustNoErr(t, err, "create node")
		*n.id = id
	}
	return db, New(db, "stn-test", db, db), procID, nodeA, nodeB
}

func levelsOnOutbox(t *testing.T, db *store.DB) []protocol.LinesideBucketLevel {
	t.Helper()
	levels := pendingOutboxByType[protocol.LinesideBucketLevel](t, db, protocol.SubjectLinesideBucketLevel)
	msgs, err := db.ListPendingOutbox(1000)
	testutil.MustNoErr(t, err, "list outbox")
	for _, msg := range msgs {
		testutil.MustNoErr(t, db.AckOutbox(msg.ID), "ack outbox")
	}
	return levels
}

func captureAt(t *testing.T, m *Mutator, nodeID int64, core, part string, qty int) {
	t.Helper()
	_, err := m.CaptureToLineside(CaptureEvent{
		NodeID: nodeID, CoreNodeName: core, BinID: 1, PayloadCode: part, BinEpoch: 1,
		Disposition: ReleaseDisposition{Mode: DispositionCaptureLineside, LinesideCapture: map[string]int{part: qty}},
	})
	testutil.MustNoErr(t, err, "capture")
}

// One flush sends ONE level per dirty key, however many writes the window
// held: the qty as read at the flush, and Drained as the window's drains
// summed. A flush with nothing dirty sends nothing.
func TestFlush_OneLevelPerDirtyKeyWithTheWindowsDrains(t *testing.T) {
	t.Parallel()
	db, m, _, nodeA, _ := levelFixture(t, "LVL-WIN", false)
	captureAt(t, m, nodeA, "LVL-WIN-A", "SYN-PART-1", 20)
	m.Flush()
	levelsOnOutbox(t, db)

	for _, d := range []int{3, 2} {
		drained, err := db.DrainLinesideBucket(nodeA, "SYN-PART-1", d)
		testutil.MustNoErr(t, err, "drain")
		testutil.MustNoErr(t, m.Consumed(TickEvent{NodeID: nodeA, CoreNodeName: "LVL-WIN-A",
			Drains: map[string]int{"SYN-PART-1": drained}}), "consumed")
	}
	m.Flush()

	levels := levelsOnOutbox(t, db)
	if len(levels) != 1 {
		t.Fatalf("levels = %+v, want one for the one dirty key", levels)
	}
	if got := levels[0]; got.Qty != 15 || got.Drained != 5 || got.State != protocol.LinesideBucketActive || got.SequenceID != 2 {
		t.Errorf("level = %+v, want qty 15, Drained 5 (3 + 2), active, seq 2", got)
	}

	m.Flush()
	if levels := levelsOnOutbox(t, db); len(levels) != 0 {
		t.Errorf("a flush with nothing dirty sent %+v, want nothing", levels)
	}
}

// Two local nodes with one core name are one place at Core, so they are one
// key and one level: the sum over both nodes.
func TestFlush_TwoLocalNodesSharingACoreNameSendTheSum(t *testing.T) {
	t.Parallel()
	db, m, _, nodeA, nodeB := levelFixture(t, "LVL-SHARE", true)
	captureAt(t, m, nodeA, "LVL-SHARE-A", "SYN-PART-1", 10)
	captureAt(t, m, nodeB, "LVL-SHARE-A", "SYN-PART-1", 7)
	m.Flush()

	levels := levelsOnOutbox(t, db)
	if len(levels) != 1 || levels[0].CoreNodeName != "LVL-SHARE-A" || levels[0].Qty != 17 {
		t.Errorf("levels = %+v, want one level of 17 (10 + 7) for LVL-SHARE-A", levels)
	}
}

// THE BOOT RESEND. Every pile row's level goes out, active and stranded,
// unconditionally: nothing was marked before it.
func TestResendLevels_SendsEveryRow(t *testing.T) {
	t.Parallel()
	db, _, procID, nodeA, nodeB := levelFixture(t, "LVL-BOOT", false)
	for _, c := range []struct {
		node int64
		part string
		qty  int
	}{{nodeA, "SYN-PART-1", 12}, {nodeB, "SYN-PART-2", 4}} {
		_, err := db.CaptureLinesideBucket(c.node, c.part, c.qty)
		testutil.MustNoErr(t, err, "seed pile")
	}
	_, err := db.StrandLinesidePiles(procID)
	testutil.MustNoErr(t, err, "strand")
	_, err = db.CaptureLinesideBucket(nodeA, "SYN-PART-1", 5)
	testutil.MustNoErr(t, err, "seed a new active pile")

	m := New(db, "stn-test", db, db) // a fresh accumulator, as at boot
	n, err := m.ResendLevels()
	testutil.MustNoErr(t, err, "ResendLevels")
	if n != 3 {
		t.Errorf("ResendLevels marked %d keys, want 3", n)
	}

	got := map[string]int{}
	for _, l := range levelsOnOutbox(t, db) {
		got[l.CoreNodeName+"/"+l.PayloadCode+"/"+string(l.State)] = l.Qty
	}
	want := map[string]int{
		"LVL-BOOT-A/SYN-PART-1/active":   5,
		"LVL-BOOT-A/SYN-PART-1/stranded": 12,
		"LVL-BOOT-B/SYN-PART-2/stranded": 4,
	}
	if len(got) != len(want) {
		t.Fatalf("levels = %v, want %v", got, want)
	}
	for k, q := range want {
		if got[k] != q {
			t.Errorf("level %s = %d, want %d", k, got[k], q)
		}
	}
}

// A deleted row's level goes out as 0: the mirror's "row gone".
func TestPilesChanged_DeletedRowSendsZero(t *testing.T) {
	t.Parallel()
	db, m, _, nodeA, _ := levelFixture(t, "LVL-GONE", false)
	captureAt(t, m, nodeA, "LVL-GONE-A", "SYN-PART-1", 6)
	m.Flush()
	levelsOnOutbox(t, db)

	rows, err := db.ListLinesideBuckets(nodeA)
	testutil.MustNoErr(t, err, "list")
	testutil.MustNoErr(t, db.DeleteLinesideBucket(rows[0].ID), "delete")
	m.PilesChanged(lineside.Key{NodeID: nodeA, CoreNodeName: "LVL-GONE-A", PayloadCode: "SYN-PART-1", State: lineside.StateActive})

	levels := levelsOnOutbox(t, db)
	if len(levels) != 1 || levels[0].Qty != 0 || levels[0].SequenceID != 2 {
		t.Errorf("levels = %+v, want one level of 0 at seq 2", levels)
	}
}

// pendingBucket reads Pending.Bucket under the flush lock, as the report does.
func pendingBucket(t *testing.T, m *Mutator, core, payload string) (qty int, ok bool) {
	t.Helper()
	testutil.MustNoErr(t, m.WithPending(func(p Pending) error {
		qty, ok = p.Bucket(core, payload)
		return nil
	}), "WithPending")
	return qty, ok
}

// The report states the active pile as Core holds it: the last level sent. A
// capture the flush has not sent yet does not move it, so an unflushed window
// never reads as a divergence; the flush does.
func TestPending_BucketIsTheLastSentLevel(t *testing.T) {
	t.Parallel()
	_, m, _, nodeA, _ := levelFixture(t, "LVL-PEND", false)
	captureAt(t, m, nodeA, "LVL-PEND-A", "SYN-PART-1", 10)
	m.Flush()
	captureAt(t, m, nodeA, "LVL-PEND-A", "SYN-PART-1", 4) // unsent

	if qty, ok := pendingBucket(t, m, "LVL-PEND-A", "SYN-PART-1"); !ok || qty != 10 {
		t.Errorf("Pending.Bucket before the flush = %d (%v), want 10: the level Core holds", qty, ok)
	}
	m.Flush()
	if qty, ok := pendingBucket(t, m, "LVL-PEND-A", "SYN-PART-1"); !ok || qty != 14 {
		t.Errorf("Pending.Bucket after the flush = %d (%v), want 14", qty, ok)
	}
	if _, ok := pendingBucket(t, m, "LVL-PEND-A", "SYN-PART-9"); ok {
		t.Error("Pending.Bucket claimed a key the accumulator never saw; the caller's table read must stand")
	}
}
