package plc

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"shingoedge/config"
)

// TestSSEReconcileLoop_PromotesStaleConnecting pins the Springfield
// 2026-07-24 failure.
//
// status-change is edge-triggered. When WarLink and the edge restart
// together, the one-shot bootstrap poll in sseConnect lands while WarLink is
// still opening its own PLC connections, so the edge caches "Connecting". The
// transitions that would promote those PLCs either predate the subscription
// or are missed, and before this fix nothing re-asked — IsConnected stayed
// false, pollReportingPoint early-returned, and the cell stopped counting
// until a human restarted the service. It ran that way for three and a half
// days across a weekend while WarLink itself held 49 of 50 PLCs Connected.
//
// The guarantee under test is narrow and is the whole fix: while an SSE
// stream is up, a per-PLC status that has gone stale is re-derived from
// WarLink's authoritative list within one interval.
func TestSSEReconcileLoop_PromotesStaleConnecting(t *testing.T) {
	cfg := config.Defaults()
	mgr := NewManager(nil, cfg, &mockEmitter{}, nil)

	// WarLink's truth: the PLC is up.
	mgr.wl = &mockWarlinkClient{
		plcs: []WarlinkPLC{{Name: "P42_SNF1_PLC", Status: "Connected"}},
		tags: map[string]map[string]WarlinkTag{},
	}

	// The edge's stale view: caught mid-startup and never corrected. This is
	// the exact state 40 of Springfield's 50 PLCs were wedged in.
	mgr.plcs["P42_SNF1_PLC"] = &ManagedPLC{
		Name:   "P42_SNF1_PLC",
		Status: "Connecting",
		Values: map[string]TagValue{},
	}

	if mgr.IsConnected("P42_SNF1_PLC") {
		t.Fatal("precondition: PLC should start stuck at Connecting")
	}

	// Shorten the interval for the test; restore so we don't leak into others.
	orig := sseReconcileInterval
	sseReconcileInterval = 10 * time.Millisecond
	defer func() { sseReconcileInterval = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go mgr.sseReconcileLoop(ctx)

	deadline := time.After(2 * time.Second)
	for {
		if mgr.IsConnected("P42_SNF1_PLC") {
			return // reconciled
		}
		select {
		case <-deadline:
			t.Fatal("PLC still stuck at Connecting: SSE mode never re-derived per-PLC " +
				"status from WarLink, so a missed status-change wedges the cell " +
				"until someone restarts the service")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

// TestSSEReconcileLoop_StopsWithContext pins that each SSE connection owns
// exactly one reconcile loop. Without this the loop would outlive its stream
// and every reconnect would add another, each polling WarLink forever.
func TestSSEReconcileLoop_StopsWithContext(t *testing.T) {
	cfg := config.Defaults()
	mgr := NewManager(nil, cfg, &mockEmitter{}, nil)
	mgr.wl = &mockWarlinkClient{plcs: []WarlinkPLC{}, tags: map[string]map[string]WarlinkTag{}}

	orig := sseReconcileInterval
	sseReconcileInterval = 5 * time.Millisecond
	defer func() { sseReconcileInterval = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { mgr.sseReconcileLoop(ctx); close(done) }()

	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("sseReconcileLoop did not return when its connection context was cancelled")
	}
}

// failingWarlinkClient serves a working PLC list, then fails every ListPLCs —
// a transient REST outage while the SSE stream itself is healthy.
type failingWarlinkClient struct {
	mockWarlinkClient
	fail atomic.Bool
}

func (f *failingWarlinkClient) ListPLCs(ctx context.Context) ([]WarlinkPLC, error) {
	if f.fail.Load() {
		return nil, errors.New("simulated transient REST failure")
	}
	return f.plcs, nil
}

// TestSSEReconcile_RESTFailureDoesNotFlapConnection pins the fix to a defect
// the reconcile itself introduced.
//
// warlinkSync's failure path marks the WarLink CONNECTION down and emits a
// disconnect. That is correct in poll mode, where REST is the transport. It is
// wrong under SSE, where the stream is the transport and REST is only a
// reconciliation channel — there, one 8s timeout would flap the indicator and
// fire a spurious alert once a minute on a stream that is healthy and
// delivering events. Before the reconcile existed this could not happen,
// because the bootstrap poll ran exactly once per connection.
//
// A phantom outage is precisely what this whole mechanism exists to stop
// people chasing, so the reconcile must not manufacture one.
func TestSSEReconcile_RESTFailureDoesNotFlapConnection(t *testing.T) {
	cfg := config.Defaults()
	mgr := NewManager(nil, cfg, &mockEmitter{}, nil)
	fc := &failingWarlinkClient{}
	fc.plcs = []WarlinkPLC{{Name: "P42_SNF1_PLC", Status: "Connected"}}
	fc.tags = map[string]map[string]WarlinkTag{}
	mgr.wl = fc

	// The stream is up and has said so.
	mgr.warlinkConnected = true

	// REST goes away; the stream does not.
	fc.fail.Store(true)

	// Drive it through sseReconcileLoop rather than calling warlinkSync
	// directly. Calling directly would pin the MECHANISM but not the WIRING —
	// an earlier draft of this test did exactly that, and passed even when the
	// loop was edited to pass ownsConnState=true. The bug this guards against
	// is a wrong argument at the call site, so the call site has to be in the
	// path under test.
	orig := sseReconcileInterval
	sseReconcileInterval = 10 * time.Millisecond
	defer func() { sseReconcileInterval = orig }()

	ctx, cancel := context.WithCancel(context.Background())
	go mgr.sseReconcileLoop(ctx)
	time.Sleep(150 * time.Millisecond) // several failing reconcile cycles
	cancel()

	mgr.mu.Lock()
	stillConnected := mgr.warlinkConnected
	mgr.mu.Unlock()
	if !stillConnected {
		t.Fatal("a transient REST failure marked WarLink DOWN while the SSE stream was healthy — " +
			"the reconcile channel must not own connection state")
	}

	// Poll mode is the opposite case and must still report the outage: there
	// REST *is* the transport, so a failed fetch is a real disconnect.
	mgr.warlinkSync(true)
	if mgr.warlinkConnected {
		t.Fatal("poll mode must still mark WarLink down on a REST failure — REST is the transport there")
	}
}
