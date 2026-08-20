package simulator

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingocore/fleet"
)

// §R.98 stage A1. A map miss in ReleaseOrder carries two different facts and
// they must not share an answer:
//
//   - the order settled and the eviction sweep reaped it — moot, idempotent nil;
//   - this backend never issued the mission at all — an error, because there is
//     no mission to append to and Core's durable witness must not advance.
//
// The second case is what a Core restart produces against an in-process fleet,
// and returning nil for it is what made a whole measured window unreadable.
func TestReleaseOrderNeverIssuedIsAnError(t *testing.T) {
	s := New()

	err := s.ReleaseOrder("sim-does-not-exist", []fleet.OrderBlock{
		{BlockID: "b1", Location: "A", BinTask: "JackUnload"},
	}, true)
	if err == nil {
		t.Fatal("release of a never-issued mission must fail, not report success")
	}
	if !strings.Contains(err.Error(), "never issued") {
		t.Fatalf("the error must name what is wrong; got %q", err)
	}
}

// The idempotent arm is the one that has to keep working: an order that reached
// a terminal state and was evicted still absorbs a late release without error,
// because the release genuinely is moot and a hard error there cascades a
// spurious second failure onto the Edge.
func TestReleaseOrderSettledAndEvictedStaysIdempotent(t *testing.T) {
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m := clock.NewManual(start)
	s := New(WithClock(m))

	id := mkTransport(t, s, "settled")
	s.DriveState(id, "FINISHED")
	m.Advance(10 * time.Minute)
	if n := s.EvictTerminalBefore(m.Now()); n != 1 {
		t.Fatalf("want 1 evicted, got %d", n)
	}
	if s.HasOrder(id) {
		t.Fatalf("%s should have been evicted", id)
	}

	if err := s.ReleaseOrder(id, []fleet.OrderBlock{
		{BlockID: "late", Location: "B", BinTask: "JackUnload"},
	}, true); err != nil {
		t.Fatalf("release of a settled-and-evicted order must stay idempotent: %v", err)
	}
}

// A terminal order still in the map is the same moot case one step earlier, and
// keeps the same answer.
func TestReleaseOrderTerminalButUnevictedStaysIdempotent(t *testing.T) {
	s := New()

	id := mkTransport(t, s, "terminal")
	s.DriveState(id, "FINISHED")

	if err := s.ReleaseOrder(id, []fleet.OrderBlock{
		{BlockID: "late", Location: "B", BinTask: "JackUnload"},
	}, true); err != nil {
		t.Fatalf("release of a terminal order must stay idempotent: %v", err)
	}
}

// The restart shape, end to end: a live staged mission is released fine; the
// same mission after the backend has forgotten it (a fresh process, which is
// exactly what a Core restart hands the in-process fleet) refuses.
func TestReleaseOrderRefusesAfterTheBackendForgets(t *testing.T) {
	s := New()
	res, err := s.CreateOrder(fleet.CreateOrderRequest{
		ExternalID: "staged",
		Blocks:     []fleet.OrderBlock{{BlockID: "head", Location: "A", BinTask: "JackLoad"}},
		Complete:   false,
	})
	if err != nil {
		t.Fatalf("CreateOrder: %v", err)
	}
	tail := []fleet.OrderBlock{{BlockID: "tail", Location: "B", BinTask: "JackUnload"}}
	if err := s.ReleaseOrder(res.VendorOrderID, tail, true); err != nil {
		t.Fatalf("release against a live mission must succeed: %v", err)
	}

	restarted := New() // the fleet a restarted Core wakes up next to
	if err := s.ReleaseOrder(res.VendorOrderID, tail, true); err != nil {
		t.Fatalf("sanity: the original backend still holds the mission: %v", err)
	}
	if err := restarted.ReleaseOrder(res.VendorOrderID, tail, true); err == nil {
		t.Fatal("a backend that never issued the mission must refuse the append")
	}
}
