package plc

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/store/counters"
)

// TestProductionTick_PreservesPerTickAcrossBinSwapGap proves the PRESERVING
// half of the §8 #13 / §12 premise: production.tick emits exactly one envelope
// per PLC counter tick, each carrying its own RecordedAt and CountValue, and it
// does so regardless of bin-binding state — because it is published in the PLC
// manager UPSTREAM of the engine's inventory hold-and-replay.
//
// The DESTRUCTIVE half (the inventory BinUOPDelta stream lumping the same gap
// ticks into a single delta) is proven in
// engine/wiring_counter_delta_holdreplay_test.go. Together the two tests are the
// code substitute for the unrun live "tap bin_uop_delta across a changeover"
// spike (§8 #13): per-tick timing IS destroyed on the inventory channel and IS
// preserved on production.tick.
func TestProductionTick_PreservesPerTickAcrossBinSwapGap(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.Messaging.StationID = "STN-TEST"
	mgr := NewManager(db, cfg, &mockEmitter{}, nil)

	rp := counters.ReportingPoint{
		ID: 1, PLCName: "logix", TagName: "Cell_A_Count",
		ProcessID: 7, StyleID: 3,
	}

	// Six counter ticks. The first three model "while bin A is bound"; the last
	// three model the finalize→new-empty-bin gap that lumps the BinUOPDelta
	// stream. production.tick is upstream of bin binding, so all six must emit —
	// including the final jump tick (§8 #20). The per-tick EdgeSnapshotID /
	// CountValue are the identity that proves no lumping occurred.
	const ticks = 6
	for i := int64(1); i <= ticks; i++ {
		anomaly := ""
		if i == ticks {
			anomaly = "jump" // a jump must still emit a heartbeat tick
		}
		mgr.enqueueProductionTick(rp, i /*snapID*/, i /*newCount*/, 1 /*delta*/, anomaly)
	}

	msgs, err := db.ListPendingOutbox(100)
	if err != nil {
		t.Fatalf("list outbox: %v", err)
	}

	var snaps []protocol.CounterSnapshot
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectProductionTick {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			t.Fatalf("decode envelope (outbox id %d): %v", m.ID, err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			t.Fatalf("decode data (outbox id %d): %v", m.ID, err)
		}
		if data.Subject != protocol.SubjectProductionTick {
			t.Errorf("envelope subject=%q, want %q", data.Subject, protocol.SubjectProductionTick)
		}
		var snap protocol.CounterSnapshot
		if err := json.Unmarshal(data.Body, &snap); err != nil {
			t.Fatalf("decode CounterSnapshot (outbox id %d): %v", m.ID, err)
		}
		snaps = append(snaps, snap)
	}

	// One envelope per tick — NOT lumped. This is the whole point: 6 ticks → 6
	// production.tick events (contrast the 4 BinUOPDelta events in the engine
	// test, where the 3 gap ticks collapse into 1).
	if len(snaps) != ticks {
		t.Fatalf("production.tick envelopes=%d, want %d (one per tick, no lumping)", len(snaps), ticks)
	}

	// Order-independent: sort by EdgeSnapshotID (== tick order) before checking.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].EdgeSnapshotID < snaps[j].EdgeSnapshotID })

	var last time.Time
	for i, s := range snaps {
		want := int64(i + 1)
		if s.CountValue != want {
			t.Errorf("tick %d: CountValue=%d, want %d (per-tick value preserved)", i, s.CountValue, want)
		}
		if s.EdgeSnapshotID != want {
			t.Errorf("tick %d: EdgeSnapshotID=%d, want %d (distinct per tick)", i, s.EdgeSnapshotID, want)
		}
		if s.Delta != 1 {
			t.Errorf("tick %d: Delta=%d, want 1 (per-tick unit delta, not a lumped sum)", i, s.Delta)
		}
		if s.Station != "STN-TEST" {
			t.Errorf("tick %d: Station=%q, want STN-TEST", i, s.Station)
		}
		if s.ProcessID != 7 || s.StyleID != 3 {
			t.Errorf("tick %d: ProcessID/StyleID=%d/%d, want 7/3 (enriched from rp)", i, s.ProcessID, s.StyleID)
		}
		// RecordedAt must be Go-stamped (non-zero, UTC) — NOT SQLite's
		// second-granularity datetime('now') default (§8 #21). Successive
		// wall-clock reads can be equal at coarse OS resolution, so assert
		// non-decreasing, not strictly increasing.
		if s.RecordedAt.IsZero() {
			t.Errorf("tick %d: RecordedAt is zero — must be stamped in Go via time.Now().UTC()", i)
		}
		if loc := s.RecordedAt.Location(); loc != time.UTC {
			t.Errorf("tick %d: RecordedAt location=%v, want UTC", i, loc)
		}
		if !last.IsZero() && s.RecordedAt.Before(last) {
			t.Errorf("tick %d: RecordedAt %v before previous %v (per-tick timestamps must be monotonic)", i, s.RecordedAt, last)
		}
		last = s.RecordedAt
	}

	// The jump tick (last) must be present and carry Anomaly=="jump": the
	// heartbeat records that the cell physically fired even though inventory
	// attribution gates jumps for operator confirmation (§8 #20).
	if got := snaps[ticks-1].Anomaly; got != "jump" {
		t.Errorf("final tick Anomaly=%q, want %q (jumps still emit a production.tick)", got, "jump")
	}
}
