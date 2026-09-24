package plc

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/config"
	"shingoedge/store"
)

// TestProductionTick_SnapshotRowCarriesTheTick: the counter_snapshots row the
// poll already writes is the durable record of the tick. The one INSERT binds
// recorded_ms (Go clock, taken before the INSERT), process_id and style_id, so
// a shipper can rebuild the wire event from the row alone and an outage that
// spans a changeover still attributes each stroke to the style it was made on.
func TestProductionTick_SnapshotRowCarriesTheTick(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)
	before := clock.Now().UTC().UnixMilli()
	r.pass(4)
	after := clock.Now().UTC().UnixMilli()

	var ms, pid, sid int64
	if err := r.db.QueryRow(`SELECT recorded_ms, process_id, style_id FROM counter_snapshots
		WHERE reporting_point_id = ? ORDER BY id DESC LIMIT 1`, r.rpID).Scan(&ms, &pid, &sid); err != nil {
		t.Fatalf("read snapshot row: %v", err)
	}
	if ms < before || ms > after {
		t.Errorf("recorded_ms = %d, want within [%d, %d]", ms, before, after)
	}
	if pid != r.proc || sid != r.sty {
		t.Errorf("process/style = %d/%d, want %d/%d", pid, sid, r.proc, r.sty)
	}
}

// TestProductionTick_NoOutboxRow: the heartbeat feed no longer rides the
// outbox. A tick writes its counter_snapshots row and nothing else.
func TestProductionTick_NoOutboxRow(t *testing.T) {
	t.Parallel()
	r := newTickRig(t)
	r.pass(1)
	r.pass(2)
	msgs, err := r.db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}
	for _, m := range msgs {
		if m.MsgType == protocol.SubjectProductionTick {
			t.Errorf("outbox row %d carries %s — the tick feed must not write the outbox", m.ID, m.MsgType)
		}
	}
}

// TestProductionTick_PollPassStatementBudget pins the Pi cost of a poll pass,
// counted at the driver seam: the reporting-point list, the snapshot INSERT and
// the reporting-point UPDATE. Nothing per tick for the heartbeat feed — it was
// an outbox INSERT here, plus a drain SELECT and an Ack UPDATE behind it.
func TestProductionTick_PollPassStatementBudget(t *testing.T) {
	t.Parallel()
	db, qc, err := store.OpenCounting(filepath.Join(t.TempDir(), "count.db"))
	if err != nil {
		t.Fatalf("open counting: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.Messaging.StationID = "stn-test"
	mgr := NewManager(db, cfg, &mockEmitter{}, nil)
	proc, err := db.CreateProcess("PROC-A", "", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sty, err := db.CreateStyle("STYLE-A", "", proc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateReportingPoint(rigPLC, rigTag, sty); err != nil {
		t.Fatal(err)
	}
	mgr.plcs[rigPLC] = &ManagedPLC{Name: rigPLC, Status: "Connected", Values: map[string]TagValue{}}
	r := &tickRig{t: t, db: db, mgr: mgr}

	r.pass(1)
	qc.Reset()
	r.pass(2)
	if got := qc.Count(); got != 3 {
		t.Errorf("poll pass with one tick = %d statements, want 3 (list + snapshot INSERT + reporting-point UPDATE)", got)
	}
	qc.Reset()
	r.pass(2)
	if got := qc.Count(); got != 1 {
		t.Errorf("poll pass with no change = %d statements, want 1 (the list)", got)
	}
}
