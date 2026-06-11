package simulator

import (
	"testing"
	"time"

	"shingo/shared/clock"
	"shingocore/fleet"
)

func mkTransport(t *testing.T, s *SimulatorBackend, ext string) string {
	t.Helper()
	res, err := s.CreateTransportOrder(fleet.TransportOrderRequest{ExternalID: ext, FromLoc: "A", ToLoc: "B"})
	if err != nil {
		t.Fatalf("CreateTransportOrder(%s): %v", ext, err)
	}
	return res.VendorOrderID
}

// T2.1 (F1): IDs come from a monotonic counter, so an evicted ID is never
// re-minted. The old len(s.orders)+1 scheme reused IDs the moment the map
// shrank — which eviction now makes happen.
func TestOrderIDsAreMonotonicAndNeverReused(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := clock.NewManual(start)
	s := New(WithClock(m))

	id1 := mkTransport(t, s, "o1")
	id2 := mkTransport(t, s, "o2")
	if id1 != "sim-1" || id2 != "sim-2" {
		t.Fatalf("want sim-1/sim-2, got %s/%s", id1, id2)
	}

	// Drive the first order terminal and evict it.
	s.DriveState(id1, "FINISHED")
	m.Advance(11 * time.Minute)
	if n := s.EvictTerminalBefore(m.Now()); n != 1 {
		t.Fatalf("want 1 evicted, got %d", n)
	}
	if s.HasOrder(id1) {
		t.Fatalf("%s should have been evicted", id1)
	}

	// With one order left, len(s.orders)+1 would have minted sim-2 (collision).
	// The monotonic counter must produce sim-3.
	id3 := mkTransport(t, s, "o3")
	if id3 != "sim-3" {
		t.Fatalf("want sim-3 (monotonic, no reuse), got %s", id3)
	}
}

// T2.1: terminal orders (FINISHED/FAILED/STOPPED) older than the cutoff are
// reaped; active orders and orders without a terminal timestamp are kept.
func TestEvictTerminalBefore(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := clock.NewManual(start)
	s := New(WithClock(m))

	done := mkTransport(t, s, "done")
	failed := mkTransport(t, s, "failed")
	stopped := mkTransport(t, s, "stopped")
	active := mkTransport(t, s, "active")

	s.DriveState(done, "FINISHED")
	s.DriveState(failed, "FAILED")
	if err := s.CancelOrder(stopped); err != nil { // sets STOPPED + stamps
		t.Fatalf("CancelOrder: %v", err)
	}
	s.DriveState(active, "RUNNING")

	// Stamped at t0; nothing is strictly before t0.
	if n := s.EvictTerminalBefore(start); n != 0 {
		t.Fatalf("nothing should evict at t0, got %d", n)
	}

	m.Advance(10 * time.Minute)
	if n := s.EvictTerminalBefore(m.Now()); n != 3 {
		t.Fatalf("want 3 evicted (FINISHED/FAILED/STOPPED), got %d", n)
	}
	if !s.HasOrder(active) {
		t.Fatalf("active (RUNNING) order must be kept")
	}
	if got := s.OrderCount(); got != 1 {
		t.Fatalf("want 1 order remaining, got %d", got)
	}
	if ids := s.VendorOrderIDs(); len(ids) != 1 || ids[0] != active {
		t.Fatalf("orderSeq should be [%s], got %v", active, ids)
	}
}

// T2.1: a terminal order whose timestamp equals the cutoff is kept (strict
// Before), so the retention window is inclusive of its own boundary.
func TestEvictKeepsRecentTerminal(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := clock.NewManual(start)
	s := New(WithClock(m))

	id := mkTransport(t, s, "recent")
	m.Advance(5 * time.Minute)
	s.DriveState(id, "FINISHED") // terminalAt = t0+5m == cutoff below

	if n := s.EvictTerminalBefore(m.Now()); n != 0 {
		t.Fatalf("equal-time terminal should be kept, got %d evicted", n)
	}
	if !s.HasOrder(id) {
		t.Fatalf("%s should be kept", id)
	}
}
