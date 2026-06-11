//go:build sim

package simwarlink

import (
	"context"
	"testing"
	"time"

	"shingo/shared/clock"
	"shingoedge/config"
	"shingoedge/plc"
)

// nopEmitter satisfies plc.EventEmitter with no-ops — the handshake test only
// cares that the poll loop reaches "Connected", not about emitted events.
type nopEmitter struct{}

func (nopEmitter) EmitCounterRead(int64, string, string, int64)                          {}
func (nopEmitter) EmitCounterDelta(int64, int64, int64, int64, int64, string)            {}
func (nopEmitter) EmitCounterAnomaly(int64, int64, string, string, int64, int64, string) {}
func (nopEmitter) EmitPLCConnected(string)                                               {}
func (nopEmitter) EmitPLCDisconnected(string, error)                                     {}
func (nopEmitter) EmitPLCHealthAlert(string, string)                                     {}
func (nopEmitter) EmitPLCHealthRecover(string)                                           {}
func (nopEmitter) EmitCounterReadError(int64, string, string, string)                    {}
func (nopEmitter) EmitWarLinkConnected()                                                 {}
func (nopEmitter) EmitWarLinkDisconnected(error)                                         {}

// Gate 3 handshake: the fake satisfies the discovery handshake against the REAL
// plc.Manager — poll loop → ListPLCs/ListTags → cache → IsConnected. This is the
// blocker (S1/S3) check that the WarlinkPLC/WarlinkTag shapes match what the
// manager consumes. The manager polls on real time (it isn't clock-injected),
// so we use a short poll rate and wait briefly for the first tick.
func TestFakeHandshakeWithRealManager(t *testing.T) {
	cfg := &config.Config{}
	cfg.WarLink.Mode = "poll"
	cfg.WarLink.PollRate = 20 * time.Millisecond
	cfg.Sim = config.SimConfig{
		Enabled: true,
		Processes: []config.SimProcessConfig{
			{PLCName: "PRESS-1", TagName: "PRESS-1_COUNTER", TickInterval: time.Second, UOPPerTick: 1},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	fake := NewFakeClient(ctx, cfg.Sim, clock.Real())

	// db=nil is safe: the WarLink poll loop does discovery only; reporting-point
	// polling (which touches the DB) is a separate loop not started here.
	mgr := plc.NewManager(nil, cfg, nopEmitter{}, fake)
	mgr.StartWarLinkPoller()
	defer mgr.StopWarLinkPoller()

	if !waitFor(2*time.Second, func() bool { return mgr.IsConnected("PRESS-1") }) {
		t.Fatal("manager never saw PRESS-1 as Connected via the fake handshake")
	}

	// The counter tag must be in the manager's cache (proves ListTags shape +
	// key-prefix stripping line up with applyTags/ReadTag).
	v, err := mgr.ReadTag("PRESS-1", "PRESS-1_COUNTER")
	if err != nil {
		t.Fatalf("ReadTag after handshake: %v", err)
	}
	if _, ok := toInt(v); !ok {
		t.Fatalf("cached tag value not numeric: %T(%v)", v, v)
	}
}

func waitFor(d time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

func toInt(v any) (int64, bool) {
	switch n := v.(type) {
	case int64:
		return n, true
	case float64:
		return int64(n), true
	default:
		return 0, false
	}
}
