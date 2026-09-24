//go:build docker

package messaging

import (
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

// tickEmit is one call of the cell-tick emitter (the SSE cell-heartbeat feed).
type tickEmit struct {
	station    string
	processID  int64
	styleID    int64
	recordedAt time.Time
}

type tickHarness struct {
	t     *testing.T
	db    *store.DB
	svc   *CoreDataService
	mu    sync.Mutex
	emits []tickEmit
}

func newTickHarness(t *testing.T) *tickHarness {
	t.Helper()
	db := testdb.Open(t)
	h := &tickHarness{t: t, db: db}
	h.svc = NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	h.svc.SetCellTickEmitter(func(station string, processID, styleID int64, recordedAt time.Time) {
		h.mu.Lock()
		h.emits = append(h.emits, tickEmit{station, processID, styleID, recordedAt})
		h.mu.Unlock()
	})
	if err := db.EnsureHeartbeatPartitions(time.Now().UTC()); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	// The projection is synchronous in the handler; the waits below still
	// hold for it (they return at once).
	return h
}

func (h *tickHarness) emitted() []tickEmit {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]tickEmit(nil), h.emits...)
}

// tick delivers one legacy production.tick from station.
func (h *tickHarness) tick(station string, snap protocol.CounterSnapshot) {
	env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station}}
	h.svc.HandleProductionTick(env, &snap)
}

// rows counts cell_part_events rows for (cell, edge_snapshot_id).
func (h *tickHarness) rows(cell string, edgeID int64) int {
	var n int
	if err := h.db.QueryRow(`SELECT count(*) FROM cell_part_events WHERE cell_id=$1 AND edge_snapshot_id=$2`,
		cell, edgeID).Scan(&n); err != nil {
		h.t.Fatalf("count rows: %v", err)
	}
	return n
}

// waitRow waits until (cell, edgeID) has at least one row. The projection may
// be asynchronous; a barrier tick sent after the one under test is how the
// tests know everything before it has been handled.
func (h *tickHarness) waitRow(cell string, edgeID int64) {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if h.rows(cell, edgeID) > 0 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	h.t.Fatalf("no cell_part_events row for %s/%d after 10s", cell, edgeID)
}

// waitEmits waits until at least n emits have been recorded (the emitter runs
// after the INSERT, so a visible row does not yet mean an emitted one).
func (h *tickHarness) waitEmits(n int) []tickEmit {
	h.t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if em := h.emitted(); len(em) >= n {
			return em
		}
		time.Sleep(10 * time.Millisecond)
	}
	return h.emitted()
}

func msTime(sec int, ms int) time.Time {
	return time.Now().UTC().Truncate(time.Hour).Add(time.Duration(sec)*time.Second + time.Duration(ms)*time.Millisecond)
}

// TestProductionTick_ProjectsEveryColumn (P4): one tick becomes one
// cell_part_events row with every column mapped, and exactly one cell-tick emit
// carrying {station, process_id, style_id, recorded_at}.
func TestProductionTick_ProjectsEveryColumn(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	at := msTime(10, 123)
	h.tick("stn-a", protocol.CounterSnapshot{
		ReportingPointID: 99, EdgeSnapshotID: 41, ProcessID: 7, StyleID: 3,
		CountValue: 1234, Delta: 2, Anomaly: "jump", RecordedAt: at,
	})
	h.waitRow("stn-a", 41)

	var (
		cell, anomaly               string
		recorded                    time.Time
		edgeID, cv, delta, pid, sid int64
	)
	if err := h.db.QueryRow(`SELECT cell_id, recorded_at, edge_snapshot_id, count_value, delta, anomaly, process_id, style_id
		FROM cell_part_events WHERE cell_id='stn-a' AND edge_snapshot_id=41`).
		Scan(&cell, &recorded, &edgeID, &cv, &delta, &anomaly, &pid, &sid); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if cell != "stn-a" || edgeID != 41 || cv != 1234 || delta != 2 || anomaly != "jump" || pid != 7 || sid != 3 {
		t.Errorf("row = %s/%d cv=%d delta=%d anomaly=%q pid=%d sid=%d", cell, edgeID, cv, delta, anomaly, pid, sid)
	}
	if !recorded.Equal(at) {
		t.Errorf("recorded_at = %v, want %v (ms preserved)", recorded, at)
	}

	em := h.waitEmits(1)
	if len(em) != 1 {
		t.Fatalf("emits = %d, want 1", len(em))
	}
	if em[0].station != "stn-a" || em[0].processID != 7 || em[0].styleID != 3 || !em[0].recordedAt.Equal(at) {
		t.Errorf("emit = %+v", em[0])
	}
}

// TestProductionTick_SameTickTwiceIsOneRow (P5): a redelivered tick (same
// station, same edge_snapshot_id, same recorded_at — an outbox re-send) is one
// row and one emit.
func TestProductionTick_SameTickTwiceIsOneRow(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	snap := protocol.CounterSnapshot{EdgeSnapshotID: 5, ProcessID: 7, StyleID: 3, CountValue: 10, Delta: 1, RecordedAt: msTime(20, 5)}
	h.tick("stn-a", snap)
	h.tick("stn-a", snap)
	h.tick("stn-a", protocol.CounterSnapshot{EdgeSnapshotID: 6, ProcessID: 7, StyleID: 3, CountValue: 11, Delta: 1, RecordedAt: msTime(21, 5)})
	h.waitRow("stn-a", 6) // barrier

	if n := h.rows("stn-a", 5); n != 1 {
		t.Errorf("rows for the redelivered tick = %d, want 1", n)
	}
	if n := len(h.waitEmits(2)); n != 2 {
		t.Errorf("emits = %d, want 2 (the tick once, the barrier once)", n)
	}
}

// TestProductionTick_SameIDTwoStationsIsTwoRows (P6): Edge-local snapshot ids
// collide across stations (§8 #22), so the same edge_snapshot_id from two
// stations is two rows.
func TestProductionTick_SameIDTwoStationsIsTwoRows(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	at := msTime(30, 0)
	h.tick("stn-a", protocol.CounterSnapshot{EdgeSnapshotID: 7, ProcessID: 1, StyleID: 1, Delta: 1, RecordedAt: at})
	h.tick("stn-b", protocol.CounterSnapshot{EdgeSnapshotID: 7, ProcessID: 2, StyleID: 2, Delta: 1, RecordedAt: at})
	h.waitRow("stn-a", 7)
	h.waitRow("stn-b", 7)
	if a, b := h.rows("stn-a", 7), h.rows("stn-b", 7); a != 1 || b != 1 {
		t.Errorf("rows stn-a/stn-b = %d/%d, want 1/1", a, b)
	}
}

// TestProductionTick_ReusedIDAfterRestore (P9) pins what happens when an Edge's
// SQLite is restored from a backup: counter_snapshots ids rewind, and a NEW
// tick arrives carrying an edge_snapshot_id Core has already seen, with a new
// recorded_at.
//
// INVERTED by the move of the dedup key onto cell_part_events as
// (cell_id, edge_snapshot_id, recorded_at). It used to pin the drop: TryDedup
// keyed on (station, edge_snapshot_id) alone, so the new tick was discarded as a
// "replay" — a silent gap, which ComputeStops turned into a fake stop, until the
// Edge's ids passed the old high-water mark. The new tick is projected; a true
// re-send (same recorded_at, read back from the row) still is not.
func TestProductionTick_ReusedIDAfterRestore(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	h.tick("stn-a", protocol.CounterSnapshot{EdgeSnapshotID: 5, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: msTime(40, 0)})
	h.waitRow("stn-a", 5)
	// After the restore: same id, later stamp.
	h.tick("stn-a", protocol.CounterSnapshot{EdgeSnapshotID: 5, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: msTime(900, 0)})
	h.tick("stn-a", protocol.CounterSnapshot{EdgeSnapshotID: 6, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: msTime(901, 0)})
	h.waitRow("stn-a", 6) // barrier

	if n := h.rows("stn-a", 5); n != 2 {
		t.Errorf("rows for edge_snapshot_id 5 = %d, want 2 (the post-restore tick is projected)", n)
	}
}
