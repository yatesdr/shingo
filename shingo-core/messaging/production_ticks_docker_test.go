//go:build docker

package messaging

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
)

func ticksEnv(station string) *protocol.Envelope {
	return &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: station}}
}

// TestProductionTicks_BatchProjectsSynchronously: one production.ticks message
// is one pass's ticks; the handler projects them before it returns, and emits
// once per row it actually inserted. A redelivered batch inserts and emits
// nothing; a batch mixing old and new rows emits only the new.
func TestProductionTicks_BatchProjectsSynchronously(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	t0 := msTime(100, 250)
	batch := protocol.ProductionTicks{Ticks: []protocol.ProductionTickEvent{
		{EdgeSnapshotID: 11, ProcessID: 7, StyleID: 3, CountValue: 1, Delta: 1, RecordedAt: t0},
		{EdgeSnapshotID: 12, ProcessID: 7, StyleID: 3, CountValue: 4, Delta: 3, RecordedAt: t0.Add(time.Second)},
		{EdgeSnapshotID: 13, ProcessID: 7, StyleID: 3, CountValue: 900, Delta: 896, Anomaly: "jump", RecordedAt: t0.Add(2 * time.Second)},
	}}
	h.svc.HandleProductionTicks(ticksEnv("stn-a"), &batch)
	for _, id := range []int64{11, 12, 13} {
		if n := h.rows("stn-a", id); n != 1 {
			t.Errorf("rows for %d = %d immediately after the handler returned, want 1", id, n)
		}
	}
	if n := len(h.emitted()); n != 3 {
		t.Errorf("emits = %d, want 3", n)
	}

	h.svc.HandleProductionTicks(ticksEnv("stn-a"), &batch)
	if n := len(h.emitted()); n != 3 {
		t.Errorf("emits after a redelivery = %d, want still 3", n)
	}

	mixed := protocol.ProductionTicks{Ticks: append(batch.Ticks[2:3:3],
		protocol.ProductionTickEvent{EdgeSnapshotID: 14, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0.Add(3 * time.Second)})}
	h.svc.HandleProductionTicks(ticksEnv("stn-a"), &mixed)
	em := h.emitted()
	if len(em) != 4 || !em[3].recordedAt.Equal(t0.Add(3*time.Second)) {
		t.Errorf("emits after a mixed batch = %+v, want one more for id 14", em)
	}
	var rows int
	if err := h.db.QueryRow(`SELECT count(*) FROM cell_part_events WHERE cell_id='stn-a'`).Scan(&rows); err != nil {
		t.Fatal(err)
	}
	if rows != 4 {
		t.Errorf("rows = %d, want 4", rows)
	}
}

// TestProductionTicks_OneStatementPerMessage: the Core cost of a pass is one
// multi-row INSERT … ON CONFLICT DO NOTHING RETURNING, whatever the batch size
// and whether or not it is a redelivery. The dedup table's INSERT per tick is
// gone.
func TestProductionTicks_OneStatementPerMessage(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(cfg)
	if err != nil {
		t.Fatalf("open counting: %v", err)
	}
	t.Cleanup(func() { cdb.Close() })
	if err := cdb.EnsureHeartbeatPartitions(time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	svc := NewCoreDataService(cdb, &captureResponder{}, service.EpochAnnounce{})
	t0 := msTime(200, 0)
	batch := protocol.ProductionTicks{Ticks: []protocol.ProductionTickEvent{
		{EdgeSnapshotID: 1, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0},
		{EdgeSnapshotID: 2, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0.Add(time.Second)},
		{EdgeSnapshotID: 3, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0.Add(2 * time.Second)},
	}}
	for _, delivery := range []string{"first delivery", "redelivery"} {
		counter.Reset()
		svc.HandleProductionTicks(ticksEnv("stn-count"), &batch)
		if got := counter.Count(); got != 1 {
			t.Errorf("%s: %d statements, want 1", delivery, got)
		}
	}
	counter.Reset()
	svc.HandleProductionTick(ticksEnv("stn-count"), &protocol.CounterSnapshot{EdgeSnapshotID: 4, Delta: 1, StyleID: 3, RecordedAt: t0.Add(3 * time.Second)})
	if got := counter.Count(); got != 1 {
		t.Errorf("legacy production.tick: %d statements, want 1 (same INSERT path)", got)
	}
}

// TestEdgeHeartbeat_RecordsTickLag: the Edge's periodic heartbeat carries the
// shipper's lag; Core keeps the latest per station in the SAME UPDATE that
// already marks the station alive. A heartbeat without the fields (an older
// Edge) leaves the previous report, and its time, alone.
func TestEdgeHeartbeat_RecordsTickLag(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	const uid = "stn-lag"
	if _, err := db.IntroduceEdge(uid, "host-lag", "v-test"); err != nil {
		t.Fatalf("introduce edge: %v", err)
	}
	svc := NewCoreDataService(db, &captureResponder{}, service.EpochAnnounce{})
	pending, age := int64(12), int64(4200)
	svc.HandleEdgeHeartbeat(ticksEnv(uid), &protocol.EdgeHeartbeat{StationID: uid, TickPending: &pending, TickOldestUnsentAgeMS: &age})

	e, err := db.GetEdgeByUID(uid)
	if err != nil {
		t.Fatal(err)
	}
	if e.TickPending == nil || *e.TickPending != 12 || e.TickOldestUnsentAgeMS == nil || *e.TickOldestUnsentAgeMS != 4200 || e.TickReportedAt == nil {
		t.Fatalf("edge lag = %v/%v at %v, want 12/4200 stamped", e.TickPending, e.TickOldestUnsentAgeMS, e.TickReportedAt)
	}
	first := *e.TickReportedAt

	svc.HandleEdgeHeartbeat(ticksEnv(uid), &protocol.EdgeHeartbeat{StationID: uid})
	e, err = db.GetEdgeByUID(uid)
	if err != nil {
		t.Fatal(err)
	}
	if e.TickPending == nil || *e.TickPending != 12 || e.TickReportedAt == nil || !e.TickReportedAt.Equal(first) {
		t.Errorf("a heartbeat without lag fields moved the report: %v at %v", e.TickPending, e.TickReportedAt)
	}
}

// TestProductionTicks_BadRowDoesNotSinkThePage: one tick whose recorded_at
// falls outside every partition (an Edge with a wrong clock) makes the page's
// multi-row INSERT fail. The Edge has already moved its cursor, so the handler
// retries that page row by row: the good ticks land and emit, the bad one is
// logged and counted against the station on edge_registry.tick_rejected, which
// the Inventory page's tick-feed panel shows.
func TestProductionTicks_BadRowDoesNotSinkThePage(t *testing.T) {
	t.Parallel()
	h := newTickHarness(t)
	const uid = "stn-bad-clock"
	if _, err := h.db.IntroduceEdge(uid, "host-bad", "v-test"); err != nil {
		t.Fatalf("introduce edge: %v", err)
	}
	t0 := msTime(300, 0)
	page := protocol.ProductionTicks{Ticks: []protocol.ProductionTickEvent{
		{EdgeSnapshotID: 21, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0},
		{EdgeSnapshotID: 22, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: time.Date(1970, 1, 1, 0, 0, 5, 0, time.UTC)},
		{EdgeSnapshotID: 23, ProcessID: 7, StyleID: 3, Delta: 1, RecordedAt: t0.Add(time.Second)},
	}}
	h.svc.HandleProductionTicks(ticksEnv(uid), &page)

	if a, b, c := h.rows(uid, 21), h.rows(uid, 22), h.rows(uid, 23); a != 1 || b != 0 || c != 1 {
		t.Errorf("rows 21/22/23 = %d/%d/%d, want 1/0/1 (the good ticks land)", a, b, c)
	}
	if n := len(h.emitted()); n != 2 {
		t.Errorf("emits = %d, want 2", n)
	}
	var rejected int64
	if err := h.db.QueryRow(`SELECT tick_rejected FROM edge_registry WHERE station_uid = $1`, uid).Scan(&rejected); err != nil {
		t.Fatalf("read tick_rejected: %v", err)
	}
	if rejected != 1 {
		t.Errorf("tick_rejected = %d, want 1", rejected)
	}
}
