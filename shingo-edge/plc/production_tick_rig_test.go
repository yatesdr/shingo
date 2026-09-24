package plc

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/counters"
)

// tickRig drives the real poll path — pollAllReportingPoints over a reporting
// point stored in SQLite, with the PLC value served from the WarLink cache — so
// the production-tick pins exercise the snapshot INSERT and the ship filter the
// way a plant does, not a helper called beside them.
type tickRig struct {
	t    *testing.T
	db   *store.DB
	mgr  *Manager
	rpID int64
	proc int64
	sty  int64
}

const (
	rigPLC = "logix"
	rigTag = "Cell_A_Count"
)

func newTickRig(t *testing.T) *tickRig {
	t.Helper()
	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.Messaging.StationID = "stn-test"
	// A small threshold so a jump is cheap to produce.
	cfg.Counter.JumpThreshold = 100
	mgr := NewManager(db, cfg, &mockEmitter{}, nil)

	proc, err := db.CreateProcess("PROC-A", "", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sty, err := db.CreateStyle("STYLE-A", "", proc)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	rpID, err := db.CreateReportingPoint(rigPLC, rigTag, sty)
	if err != nil {
		t.Fatalf("create reporting point: %v", err)
	}
	mgr.plcs[rigPLC] = &ManagedPLC{Name: rigPLC, Status: "Connected", Values: map[string]TagValue{}}
	return &tickRig{t: t, db: db, mgr: mgr, rpID: rpID, proc: proc, sty: sty}
}

// setCount puts a counter value into the WarLink cache.
func (r *tickRig) setCount(v int64) {
	mp := r.mgr.plcs[rigPLC]
	mp.mu.Lock()
	mp.Values[rigTag] = TagValue{Name: rigTag, TypeStr: "DINT", Value: v}
	mp.mu.Unlock()
}

// pass runs one poll pass with the counter at v.
func (r *tickRig) pass(v int64) {
	r.setCount(v)
	r.mgr.pollAllReportingPoints()
}

// rp returns the stored reporting point as the poll pass reads it.
func (r *tickRig) rp() counters.ReportingPoint {
	rps, err := r.db.ListEnabledReportingPoints()
	if err != nil {
		r.t.Fatalf("list reporting points: %v", err)
	}
	for _, rp := range rps {
		if rp.ID == r.rpID {
			return rp
		}
	}
	r.t.Fatalf("reporting point %d not found", r.rpID)
	return counters.ReportingPoint{}
}

// wireTick is the seven fields the heartbeat feed carries per tick, whatever
// the transport. ReportingPointID is deliberately absent: Core drops it.
type wireTick struct {
	EdgeSnapshotID int64
	ProcessID      int64
	StyleID        int64
	CountValue     int64
	Delta          int64
	Anomaly        string
	RecordedAt     time.Time
	Station        string
}

// shippedTicks returns every tick that has reached the transport, in id order.
//
// At this base the transport is the outbox: one production.tick row per tick.
func (r *tickRig) shippedTicks() []wireTick {
	r.t.Helper()
	msgs, err := r.db.ListPendingOutbox(1000)
	if err != nil {
		r.t.Fatalf("list outbox: %v", err)
	}
	var out []wireTick
	for _, m := range msgs {
		if m.MsgType != protocol.SubjectProductionTick {
			continue
		}
		var env protocol.Envelope
		if err := json.Unmarshal(m.Payload, &env); err != nil {
			r.t.Fatalf("decode envelope (outbox id %d): %v", m.ID, err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			r.t.Fatalf("decode data (outbox id %d): %v", m.ID, err)
		}
		if data.Subject != protocol.SubjectProductionTick {
			r.t.Errorf("envelope subject=%q, want %q", data.Subject, protocol.SubjectProductionTick)
		}
		var snap protocol.CounterSnapshot
		if err := json.Unmarshal(data.Body, &snap); err != nil {
			r.t.Fatalf("decode CounterSnapshot (outbox id %d): %v", m.ID, err)
		}
		out = append(out, wireTick{
			EdgeSnapshotID: snap.EdgeSnapshotID, ProcessID: snap.ProcessID, StyleID: snap.StyleID,
			CountValue: snap.CountValue, Delta: snap.Delta, Anomaly: snap.Anomaly,
			RecordedAt: snap.RecordedAt, Station: env.Src.Station,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EdgeSnapshotID < out[j].EdgeSnapshotID })
	return out
}
