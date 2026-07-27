package plc

import (
	"context"
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
