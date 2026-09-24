package plc

import (
	"encoding/json"
	"sort"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
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

	// The transport: a production tick shipper over the rig's store, with a
	// publisher that records every message.
	shipper *messaging.TickShipper
	mu      sync.Mutex
	sent    [][]byte
}

// ship returns the rig's shipper, creating it on first use.
func (r *tickRig) ship() *messaging.TickShipper {
	r.t.Helper()
	if r.shipper == nil {
		s, err := messaging.NewTickShipper(r.db, func(b []byte) error {
			r.mu.Lock()
			r.sent = append(r.sent, append([]byte(nil), b...))
			r.mu.Unlock()
			return nil
		}, r.mgr.cfg.StationID())
		if err != nil {
			r.t.Fatalf("NewTickShipper: %v", err)
		}
		r.shipper = s
	}
	return r.shipper
}

func (r *tickRig) messages() [][]byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([][]byte(nil), r.sent...)
}

const (
	rigPLC = "logix"
	rigTag = "Cell_A_Count"
)

func newTickRig(t *testing.T) *tickRig {
	t.Helper()
	return newTickRigEmitting(t, &mockEmitter{})
}

// newTickRigEmitting is newTickRig with the manager's event sink supplied, for
// the pins that read what the poll emits as well as what it ships.
func newTickRigEmitting(t *testing.T, em EventEmitter) *tickRig {
	t.Helper()
	db := testdb.Open(t)
	cfg := config.Defaults()
	cfg.Messaging.StationID = "stn-test"
	// A small threshold so a jump is cheap to produce.
	cfg.Counter.JumpThreshold = 100
	mgr := NewManager(db, cfg, em, nil)

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
// The transport is the production tick shipper: it is run once here, and every
// production.ticks message it has published is decoded.
func (r *tickRig) shippedTicks() []wireTick {
	r.t.Helper()
	if err := r.ship().ShipPending(); err != nil {
		r.t.Fatalf("ShipPending: %v", err)
	}
	return r.decodeSent()
}

// decodeSent decodes every message published so far, without shipping.
func (r *tickRig) decodeSent() []wireTick {
	r.t.Helper()
	var out []wireTick
	for i, m := range r.messages() {
		var env protocol.Envelope
		if err := json.Unmarshal(m, &env); err != nil {
			r.t.Fatalf("decode envelope %d: %v", i, err)
		}
		var data protocol.Data
		if err := env.DecodePayload(&data); err != nil {
			r.t.Fatalf("decode data %d: %v", i, err)
		}
		if data.Subject != protocol.SubjectProductionTicks {
			r.t.Errorf("envelope subject=%q, want %q", data.Subject, protocol.SubjectProductionTicks)
		}
		var body protocol.ProductionTicks
		if err := json.Unmarshal(data.Body, &body); err != nil {
			r.t.Fatalf("decode ProductionTicks %d: %v", i, err)
		}
		for _, tk := range body.Ticks {
			out = append(out, wireTick{
				EdgeSnapshotID: tk.EdgeSnapshotID, ProcessID: tk.ProcessID, StyleID: tk.StyleID,
				CountValue: tk.CountValue, Delta: tk.Delta, Anomaly: tk.Anomaly,
				RecordedAt: tk.RecordedAt, Station: env.Src.Station,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EdgeSnapshotID < out[j].EdgeSnapshotID })
	return out
}
