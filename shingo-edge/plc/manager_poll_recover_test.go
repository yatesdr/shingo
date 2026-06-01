package plc

import (
	"sync/atomic"
	"testing"

	"shingoedge/config"
	"shingoedge/store/counters"
)

// panicEmitter wraps mockEmitter to panic on EmitCounterReadError, the
// path pollReportingPoint takes when ReadTag fails. Used to verify
// pollReportingPointSafe recovers a panic in the per-RP poll body.
type panicEmitter struct {
	mockEmitter
	tripped atomic.Bool
}

func (p *panicEmitter) EmitCounterReadError(rpID int64, plcName, tagName, errMsg string) {
	p.tripped.Store(true)
	panic("simulated downstream emit panic")
}

// TestPollReportingPointSafe_RecoversFromPanic pins Field-notes Note 9a.
// A panic in the per-RP poll body must NOT kill the polling goroutine
// — pollReportingPointSafe wraps the call in defer/recover so a single
// bad reporting point can't take down counter polling for the whole
// edge.
func TestPollReportingPointSafe_RecoversFromPanic(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	emitter := &panicEmitter{}
	mgr := NewManager(nil, cfg, emitter)

	// Seed a connected PLC so IsConnected returns true and we reach the
	// ReadTag → emit path. ReadTag returns an error because no tag is
	// registered, which triggers EmitCounterReadError → panic.
	mgr.plcs["logix_test"] = &ManagedPLC{
		Name:   "logix_test",
		Status: "Connected",
		Values: map[string]TagValue{},
	}

	rp := counters.ReportingPoint{
		ID:      1,
		PLCName: "logix_test",
		TagName: "Missing_Tag",
	}

	// Pre-fix: this call would propagate the panic out of the polling
	// goroutine and silently kill the loop.
	mgr.pollReportingPointSafe(rp)

	if !emitter.tripped.Load() {
		t.Fatal("expected panicEmitter to trip; pollReportingPoint did not reach EmitCounterReadError")
	}
}
