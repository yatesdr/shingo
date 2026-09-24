package messaging

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store"
	"shingoedge/store/counters"
)

// shipRig is a counting store with one reporting point, plus a fake publisher.
type shipRig struct {
	t    *testing.T
	db   *store.DB
	qc   *store.QueryCounter
	rpID int64
	proc int64
	sty  int64

	mu      sync.Mutex
	fail    error
	sent    [][]byte
	attempt int
}

func newShipRig(t *testing.T) *shipRig {
	t.Helper()
	db, qc, err := store.OpenCounting(filepath.Join(t.TempDir(), "ship.db"))
	if err != nil {
		t.Fatalf("OpenCounting: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	proc, err := db.CreateProcess("PROC-A", "", "", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	sty, err := db.CreateStyle("STYLE-A", "", proc)
	if err != nil {
		t.Fatal(err)
	}
	rpID, err := db.CreateReportingPoint("logix", "Cell_A_Count", sty)
	if err != nil {
		t.Fatal(err)
	}
	return &shipRig{t: t, db: db, qc: qc, rpID: rpID, proc: proc, sty: sty}
}

func (r *shipRig) publish(b []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.attempt++
	if r.fail != nil {
		return r.fail
	}
	r.sent = append(r.sent, append([]byte(nil), b...))
	return nil
}

func (r *shipRig) setFail(err error) {
	r.mu.Lock()
	r.fail = err
	r.mu.Unlock()
}

func (r *shipRig) messages() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.sent...)
}

// tick writes one snapshot row the way the poll does.
func (r *shipRig) tick(count, delta int64, anomaly string, at time.Time, styleID int64) int64 {
	r.t.Helper()
	id, err := r.db.InsertCounterSnapshot(r.rpID, count, delta, anomaly,
		counters.TickStamp{RecordedAt: at, ProcessID: r.proc, StyleID: styleID})
	if err != nil {
		r.t.Fatalf("insert snapshot: %v", err)
	}
	return id
}

func (r *shipRig) shipper() *TickShipper {
	r.t.Helper()
	s, err := NewTickShipper(r.db, r.publish, "stn-test")
	if err != nil {
		r.t.Fatalf("NewTickShipper: %v", err)
	}
	return s
}

func decodeTicks(t *testing.T, b []byte) (*protocol.Envelope, protocol.ProductionTicks) {
	t.Helper()
	var env protocol.Envelope
	if err := json.Unmarshal(b, &env); err != nil {
		t.Fatalf("decode envelope: %v", err)
	}
	var data protocol.Data
	if err := env.DecodePayload(&data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.Subject != protocol.SubjectProductionTicks {
		t.Fatalf("subject = %q, want %q", data.Subject, protocol.SubjectProductionTicks)
	}
	var body protocol.ProductionTicks
	if err := json.Unmarshal(data.Body, &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return &env, body
}

// TestTickShipper_OneMessagePerPass: everything the pass wrote goes out as ONE
// production.ticks message, NoExpiry, stamped with the station, carrying the
// projected fields and not reporting_point_id.
func TestTickShipper_OneMessagePerPass(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	t0 := time.Date(2026, 3, 2, 10, 0, 0, 0, time.UTC).Add(123 * time.Millisecond)
	id1 := r.tick(1, 1, "", t0, r.sty)
	idR := r.tick(3, 3, "reset", t0.Add(time.Second), r.sty) // ships: the counter restarted and made 3
	r.tick(5, 0, "", t0.Add(2*time.Second), r.sty)           // filtered: no delta
	r.tick(6, 1, "", t0.Add(3*time.Second), 0)               // filtered: no style
	id2 := r.tick(900, 894, "jump", t0.Add(4*time.Second), r.sty)

	if err := s.ShipPending(); err != nil {
		t.Fatalf("ShipPending: %v", err)
	}
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	env, body := decodeTicks(t, msgs[0])
	if env.Src.Station != "stn-test" {
		t.Errorf("station = %q", env.Src.Station)
	}
	if !env.ExpiresAt.IsZero() {
		t.Errorf("exp = %v, want none: production.ticks is NoExpiry", env.ExpiresAt)
	}
	if len(body.Ticks) != 3 {
		t.Fatalf("ticks = %+v, want the three shippable rows", body.Ticks)
	}
	w := body.Ticks[0]
	if w.EdgeSnapshotID != id1 || w.ProcessID != r.proc || w.StyleID != r.sty || w.CountValue != 1 ||
		w.Delta != 1 || w.Anomaly != "" || !w.RecordedAt.Equal(t0) {
		t.Errorf("tick 0 = %+v", w)
	}
	if rs := body.Ticks[1]; rs.EdgeSnapshotID != idR || rs.Anomaly != "reset" || rs.Delta != 3 {
		t.Errorf("tick 1 = %+v, want the reset", rs)
	}
	if j := body.Ticks[2]; j.EdgeSnapshotID != id2 || j.Anomaly != "jump" || j.Delta != 894 {
		t.Errorf("tick 2 = %+v, want the jump", j)
	}
	if strings.Contains(string(msgs[0]), "reporting_point") {
		t.Errorf("wire carries reporting_point_id; Core drops it, so it stays off the wire")
	}
	if s.Cursor() != id2 {
		t.Errorf("cursor = %d, want %d", s.Cursor(), id2)
	}
}

// TestTickShipper_CursorMovesOnlyOnSuccess: a failed publish leaves the cursor
// where it was, and the next attempt sends the same rows.
func TestTickShipper_CursorMovesOnlyOnSuccess(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	start := s.Cursor()
	now := clock.Now().UTC()
	id := r.tick(1, 1, "", now, r.sty)

	r.setFail(errors.New("broker down"))
	if err := s.ShipPending(); err == nil {
		t.Fatal("ShipPending returned nil on a failed publish")
	}
	if s.Cursor() != start {
		t.Fatalf("cursor moved to %d on a failed publish", s.Cursor())
	}
	r.setFail(nil)
	if err := s.ShipPending(); err != nil {
		t.Fatalf("ShipPending: %v", err)
	}
	if s.Cursor() != id {
		t.Errorf("cursor = %d, want %d", s.Cursor(), id)
	}
	msgs := r.messages()
	if len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	}
	if _, body := decodeTicks(t, msgs[0]); len(body.Ticks) != 1 || body.Ticks[0].EdgeSnapshotID != id {
		t.Errorf("retry body = %+v, want the row that failed", body.Ticks)
	}
}

// TestTickShipper_PagesUntilCaughtUp: a backlog (after an outage) goes out in
// pages of 500 until nothing is left, one message per page.
func TestTickShipper_PagesUntilCaughtUp(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	t0 := clock.Now().UTC().Add(-time.Hour)
	var last int64
	for i := 0; i < 1201; i++ {
		last = r.tick(int64(i+1), 1, "", t0.Add(time.Duration(i)*time.Second), r.sty)
	}
	if err := s.ShipPending(); err != nil {
		t.Fatalf("ShipPending: %v", err)
	}
	msgs := r.messages()
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 pages (500, 500, 201)", len(msgs))
	}
	for i, want := range []int{500, 500, 201} {
		if _, body := decodeTicks(t, msgs[i]); len(body.Ticks) != want {
			t.Errorf("page %d = %d ticks, want %d", i, len(body.Ticks), want)
		}
	}
	if s.Cursor() != last {
		t.Errorf("cursor = %d, want %d", s.Cursor(), last)
	}
}

// TestTickShipper_StatementBudget: a caught-up pass is one range read and no
// write; the cursor is persisted only by persistIfMoved, which writes one row
// when the cursor moved and nothing when it did not.
func TestTickShipper_StatementBudget(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	r.tick(1, 1, "", clock.Now().UTC(), r.sty)
	r.tick(2, 1, "", clock.Now().UTC(), r.sty)

	r.qc.Reset()
	if err := s.ShipPending(); err != nil {
		t.Fatal(err)
	}
	if got := r.qc.Count(); got != 1 {
		t.Errorf("ShipPending with 2 rows = %d statements, want 1 (one range read, no write)", got)
	}
	r.qc.Reset()
	if err := s.ShipPending(); err != nil {
		t.Fatal(err)
	}
	if got := r.qc.Count(); got != 1 {
		t.Errorf("ShipPending caught up = %d statements, want 1", got)
	}
	r.qc.Reset()
	s.persistIfMoved()
	if got := r.qc.Count(); got != 1 {
		t.Errorf("persist after a move = %d statements, want 1", got)
	}
	r.qc.Reset()
	s.persistIfMoved()
	if got := r.qc.Count(); got != 0 {
		t.Errorf("persist with no move = %d statements, want 0", got)
	}
	if id, ok, err := r.db.ProductionTickCursor(); err != nil || !ok || id != s.Cursor() {
		t.Errorf("persisted cursor = %d/%v/%v, want %d", id, ok, err, s.Cursor())
	}
}

// TestTickShipper_FirstBootStartsAfterTheOutboxEra: on the first boot after
// the upgrade there is no cursor row. Rows written by the old binary have no
// recorded_ms and were already shipped through the outbox, so the cursor starts
// at the last of them and is persisted at once; rows the new binary wrote
// (recorded_ms set) ship.
func TestTickShipper_FirstBootStartsAfterTheOutboxEra(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	for i := 0; i < 3; i++ {
		if _, err := r.db.Exec(`INSERT INTO counter_snapshots (reporting_point_id, count_value, delta) VALUES (?, ?, 1)`,
			r.rpID, i+1); err != nil {
			t.Fatal(err)
		}
	}
	var oldMax int64
	if err := r.db.QueryRow(`SELECT MAX(id) FROM counter_snapshots`).Scan(&oldMax); err != nil {
		t.Fatal(err)
	}
	newID := r.tick(4, 1, "", clock.Now().UTC(), r.sty)

	s := r.shipper()
	if s.Cursor() != oldMax {
		t.Errorf("initial cursor = %d, want %d (the last outbox-era row)", s.Cursor(), oldMax)
	}
	if id, ok, err := r.db.ProductionTickCursor(); err != nil || !ok || id != oldMax {
		t.Errorf("initial cursor not persisted: %d/%v/%v", id, ok, err)
	}
	if err := s.ShipPending(); err != nil {
		t.Fatal(err)
	}
	if msgs := r.messages(); len(msgs) != 1 {
		t.Fatalf("messages = %d, want 1", len(msgs))
	} else if _, body := decodeTicks(t, msgs[0]); len(body.Ticks) != 1 || body.Ticks[0].EdgeSnapshotID != newID {
		t.Errorf("shipped %+v, want only the new-binary row %d", body.Ticks, newID)
	}

	// A restart reads the persisted cursor, not the table.
	s2 := r.shipper()
	if s2.Cursor() != oldMax {
		t.Errorf("restart cursor = %d, want the persisted %d", s2.Cursor(), oldMax)
	}
}

// TestTickShipper_StopPersistsAndExits: the loop ships on Notify, and Stop
// persists a moved cursor and returns.
func TestTickShipper_StopPersistsAndExits(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	s.Start()
	id := r.tick(1, 1, "", clock.Now().UTC(), r.sty)
	s.Notify()
	deadline := time.Now().Add(5 * time.Second)
	for len(r.messages()) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if len(r.messages()) != 1 {
		t.Fatalf("no publish within 5s of Notify")
	}
	done := make(chan struct{})
	go func() { s.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop did not return")
	}
	if got, ok, err := r.db.ProductionTickCursor(); err != nil || !ok || got != id {
		t.Errorf("cursor after Stop = %d/%v/%v, want %d persisted", got, ok, err, id)
	}
}

// TestTickShipper_Lag: pending counts shippable rows past the cursor, and the
// age is that of the oldest of them.
func TestTickShipper_Lag(t *testing.T) {
	t.Parallel()
	r := newShipRig(t)
	s := r.shipper()
	if p, age, err := s.Lag(); err != nil || p != 0 || age != 0 {
		t.Errorf("empty lag = %d/%d/%v, want 0/0/nil", p, age, err)
	}
	now := clock.Now().UTC()
	r.tick(1, 1, "", now.Add(-90*time.Second), r.sty)
	r.tick(0, 1, "", now.Add(-80*time.Second), 0) // not shippable: no style
	r.tick(2, 1, "", now.Add(-10*time.Second), r.sty)
	p, age, err := s.Lag()
	if err != nil {
		t.Fatal(err)
	}
	if p != 2 {
		t.Errorf("pending = %d, want 2", p)
	}
	if age < 89_000 || age > 120_000 {
		t.Errorf("oldest unsent age = %d ms, want ~90000", age)
	}
}
