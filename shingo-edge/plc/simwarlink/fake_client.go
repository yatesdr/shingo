//go:build sim

// Package simwarlink is a fake plc.WarlinkClient for sim mode (brief T3.1). It
// is anchored on the discovery handshake (ListPLCs + ListTags) so the REAL
// plc.Manager poll pipeline runs against it unchanged: poll loop → ListTags →
// cache → pollReportingPoint → CalculateDelta → enqueueProductionTick. There is
// no SSE implementation (S3) — dev config uses poll mode and OpenEventStream
// returns a reader that never delivers.
package simwarlink

import (
	"context"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"shingo/shared/clock"
	"shingoedge/config"
	"shingoedge/plc"
)

var _ plc.WarlinkClient = (*FakeClient)(nil)

// ReadinessFunc reports whether the fake PLC should emit a tick for the given
// plcName. When nil or returns true, the tick proceeds. When it returns false,
// the tick is suppressed (the machine is not ready — starved input, no output
// bin, or downtime). Set by the edge composition root to inspect runtime states.
type ReadinessFunc func(plcName string) bool

// FakeClient serves a fixed set of PLCs (one per distinct plc_name in the sim
// process list), each with counter tags that climb on a clock ticker — standing
// in for presses/lines whose PLCs WarLink would normally poll.
type FakeClient struct {
	mu    sync.RWMutex
	plcs  []string                    // distinct PLC names, sorted (stable output)
	vals  map[string]map[string]int64 // plcName → tagName → counter
	clk   clock.Clock
	ready ReadinessFunc // nil = always ready (backward-compatible)
}

// NewFakeClient builds the fake from the sim process list and starts one ticker
// goroutine per process (under ctx) that increments its counter by uop_per_tick
// every tick_interval. ctx owns the tickers' lifetime; edge main passes a
// process-lived context (the sim runs until exit, like the core driver).
// SetReadinessFunc sets the per-process readiness gate (G3). Call before the
// first tick or at any time — the check is read under RLock on each tick.
// Nil means always ready (the default, backward-compatible behavior).
func (f *FakeClient) SetReadinessFunc(fn ReadinessFunc) {
	f.mu.Lock()
	f.ready = fn
	f.mu.Unlock()
}

func NewFakeClient(ctx context.Context, cfg config.SimConfig, clk clock.Clock) *FakeClient {
	f := &FakeClient{vals: make(map[string]map[string]int64), clk: clk}
	seen := make(map[string]bool)
	for _, p := range cfg.Processes {
		if f.vals[p.PLCName] == nil {
			f.vals[p.PLCName] = make(map[string]int64)
		}
		f.vals[p.PLCName][p.TagName] = 0
		if !seen[p.PLCName] {
			seen[p.PLCName] = true
			f.plcs = append(f.plcs, p.PLCName)
		}
	}
	sort.Strings(f.plcs)
	for _, p := range cfg.Processes {
		interval := p.TickInterval
		if interval <= 0 {
			interval = 3 * time.Second
		}
		// The clock applies the speed multiplier: a SimClock re-paces its tickers
		// live (NewTicker re-reads speed each cycle). Pre-scaling the interval here
		// too would double-count and break the live speed toggle.
		// Register the ticker synchronously (before returning) so a test using a
		// manual clock can Advance immediately without racing goroutine startup.
		go f.tick(ctx, p, f.clk.NewTicker(interval))
	}
	return f
}

func (f *FakeClient) tick(ctx context.Context, p config.SimProcessConfig, t clock.Ticker) {
	per := p.UOPPerTick
	if per <= 0 {
		per = 1
	}
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C():
			// Readiness gate (G3): a real PLC stops counting when the machine
			// isn't ready (starved input, no output bin, downtime). The fake
			// checks the injected ReadinessFunc and suppresses the tick when
			// the machine should idle.
			f.mu.RLock()
			ready := f.ready
			f.mu.RUnlock()
			if ready != nil && !ready(p.PLCName) {
				continue // suppress tick — machine not ready
			}
			f.mu.Lock()
			f.vals[p.PLCName][p.TagName] += per
			f.mu.Unlock()
		}
	}
}

// ListPLCs returns every configured PLC as Connected — the literal status string
// plc.Manager.IsConnected compares against.
func (f *FakeClient) ListPLCs(ctx context.Context) ([]plc.WarlinkPLC, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	out := make([]plc.WarlinkPLC, 0, len(f.plcs))
	for _, name := range f.plcs {
		out = append(out, plc.WarlinkPLC{
			Name:        name,
			Address:     "sim",
			Status:      "Connected",
			ProductName: "SimPLC",
		})
	}
	return out, nil
}

// ListTags returns the PLC's current counter values. Keys carry the "plcName."
// prefix the real WarLink uses (Manager.applyTags strips it); values are int64,
// which Manager.toInt64 accepts. Error stays empty so connectionErrorFromTags
// doesn't read it as a disconnect.
func (f *FakeClient) ListTags(ctx context.Context, plcName string) (map[string]plc.WarlinkTag, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	tags := f.vals[plcName]
	out := make(map[string]plc.WarlinkTag, len(tags))
	for tagName, v := range tags {
		out[plcName+"."+tagName] = plc.WarlinkTag{
			PLC:   plcName,
			Name:  tagName,
			Type:  "DINT",
			Value: v,
		}
	}
	return out, nil
}

// ListAllTags returns the same counters in the all-tags shape (HMI convenience).
func (f *FakeClient) ListAllTags(ctx context.Context, plcName string) ([]plc.WarlinkTagInfo, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	tags := f.vals[plcName]
	names := make([]string, 0, len(tags))
	for name := range tags {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]plc.WarlinkTagInfo, 0, len(names))
	for _, name := range names {
		out = append(out, plc.WarlinkTagInfo{
			Name:       name,
			Type:       "DINT",
			Configured: true,
			Enabled:    true,
			Value:      tags[name],
		})
	}
	return out, nil
}

// SetTagPublishing is a no-op — every sim tag is already "published".
func (f *FakeClient) SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error {
	return nil
}

// ReadTagValue returns the current counter for a single tag (HMI convenience).
func (f *FakeClient) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	f.mu.RLock()
	defer f.mu.RUnlock()
	tags, ok := f.vals[plcName]
	if !ok {
		return nil, fmt.Errorf("simwarlink: PLC %q not found", plcName)
	}
	v, ok := tags[tagName]
	if !ok {
		return nil, fmt.Errorf("simwarlink: tag %q not found on %q", tagName, plcName)
	}
	return v, nil
}

// WriteTagValue discards the write (Q4: zone lights / heartbeats have no sim
// effect) and reports success.
func (f *FakeClient) WriteTagValue(ctx context.Context, plcName, tagName string, value any) error {
	return nil
}

// OpenEventStream returns a reader that stays open and never delivers. Poll mode
// never calls this; a closed/blocking reader would break a misconfigured sse
// mode, so we hand back a live pipe whose writer is never used (S3).
func (f *FakeClient) OpenEventStream(ctx context.Context) (io.ReadCloser, error) {
	r, _ := io.Pipe()
	return r, nil
}
