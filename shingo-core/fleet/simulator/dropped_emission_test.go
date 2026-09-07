package simulator

import (
	"bytes"
	"errors"
	"log"
	"os"
	"strings"
	"testing"

	"shingocore/fleet"
)

type silentEmitter struct{ calls int }

func (e *silentEmitter) EmitOrderStatusChanged(int64, string, string, string, string, string, *fleet.OrderSnapshot) {
	e.calls++
}
func (e *silentEmitter) EmitBlockCompleted(int64, string, string, string, string, int64, int64) {}
func (e *silentEmitter) EmitGraceExpired(int64, string)                                         {}

type unresolvableIDs struct{}

func (unresolvableIDs) ResolveVendorOrderID(string) (int64, error) {
	return 0, errors.New("no order with that vendor id")
}

// missNTimes resolves only after the caller has eaten N misses — the
// UpdateOrderVendor-in-flight window generalized to an outage.
type missNTimes struct{ remaining int }

func (r *missNTimes) ResolveVendorOrderID(string) (int64, error) {
	if r.remaining > 0 {
		r.remaining--
		return 0, errors.New("not landed yet")
	}
	return 42, nil
}

// §R.98 stage A4 → Fix A (2026-09-07). A state change the fleet made and Core
// never heard about used to be committed silently and indistinguishable,
// downstream, from a state change that never happened. The contract flips
// here: an unresolvable vendor id no longer drops the transition — it HOLDS
// it. The state does not advance, nothing emits, and the caller retries, the
// same way the real RDS poller keeps the old tracked state and re-polls.
func TestDriveState_UnresolvableTransitionIsHeld(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := New()
	em := &silentEmitter{}
	s.InitTracker(em, unresolvableIDs{})
	id := mkTransport(t, s, "unresolvable")

	old, mapped, deferred := s.DriveState(id, "RUNNING")
	if old != "CREATED" {
		t.Fatalf("on a deferral the old state must still be reported; got %q", old)
	}
	if mapped != "" {
		t.Fatalf("on a deferral no status was produced; got %q", mapped)
	}
	if !deferred {
		t.Fatalf("an unresolvable vendor id must defer, not drop")
	}
	if em.calls != 0 {
		t.Fatalf("a deferred transition must not emit; calls = %d", em.calls)
	}
	if got := s.GetOrder(id).State; got != "CREATED" {
		t.Fatalf("a deferred transition must not commit; order still in %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "deferred CREATED → RUNNING") || !strings.Contains(out, id) {
		t.Fatalf("the deferral must name the transition and the mission; got %q", out)
	}

	// The held transition still retires cleanly once the resolver can see the
	// order — the driver's retry, generalized here.
	s.InitTracker(em, fixedResolver{42})
	if old, mapped, deferred := s.DriveState(id, "RUNNING"); deferred || old != "CREATED" || mapped != "in_transit" {
		t.Fatalf("resolve-after-defer: DriveState returned (%q, %q, %v)", old, mapped, deferred)
	}
	if em.calls != 1 {
		t.Fatalf("the retried transition must emit exactly once; calls = %d", em.calls)
	}
	if got := s.GetOrder(id).State; got != "RUNNING" {
		t.Fatalf("the state must advance once the resolver resolves; got %q", got)
	}
}

// DriveStateWithRobot carries the same hold-don't-drop contract.
func TestDriveStateWithRobot_UnresolvableTransitionIsHeld(t *testing.T) {
	var buf bytes.Buffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	s := New()
	em := &silentEmitter{}
	s.InitTracker(em, unresolvableIDs{})
	id := mkTransport(t, s, "unresolvable-robot")

	old, mapped, deferred := s.DriveStateWithRobot(id, "RUNNING", "AMR-07")
	if !deferred || old != "CREATED" || mapped != "" {
		t.Fatalf("DriveStateWithRobot on unresolvable id returned (%q, %q, %v)", old, mapped, deferred)
	}
	if em.calls != 0 {
		t.Fatalf("a deferred transition must not emit; calls = %d", em.calls)
	}
	if got := s.GetOrder(id).State; got != "CREATED" {
		t.Fatalf("a deferred transition must not commit; order still in %q", got)
	}
	out := buf.String()
	if !strings.Contains(out, "deferred CREATED → RUNNING") || !strings.Contains(out, "AMR-07") {
		t.Fatalf("the deferral must name the transition and the robot; got %q", out)
	}
	s.InitTracker(em, fixedResolver{42})
	if _, _, deferred := s.DriveStateWithRobot(id, "RUNNING", "AMR-07"); deferred {
		t.Fatalf("the retried transition must not defer once the resolver resolves")
	}
	if em.calls != 1 {
		t.Fatalf("the retried transition must emit exactly once; calls = %d", em.calls)
	}
}

// Fix A's core contract, exercised over N misses: a transition that the
// resolver cannot see yet neither commits (the retrying caller must still see
// the old state) nor emits (Core hears each transition exactly once, and only
// when it happened), and lands exactly once once the resolver can see it.
func TestDriveState_MissThenResolveCommitsAndEmitsExactlyOnce(t *testing.T) {
	s := New()
	em := &silentEmitter{}
	res := &missNTimes{remaining: 5}
	s.InitTracker(em, res)
	id := mkTransport(t, s, "missy")

	for i := 0; i < 5; i++ {
		old, mapped, deferred := s.DriveState(id, "RUNNING")
		if !deferred || old != "CREATED" || mapped != "" {
			t.Fatalf("miss %d: expected deferred with old state; got (%q, %q, %v)", i, old, mapped, deferred)
		}
		if em.calls != 0 {
			t.Fatalf("miss %d: nothing may emit before the resolver resolves; calls = %d", i, em.calls)
		}
		if got := s.GetOrder(id).State; got != "CREATED" {
			t.Fatalf("miss %d: the state must not commit on a miss; got %q", i, got)
		}
	}

	old, mapped, deferred := s.DriveState(id, "RUNNING")
	if deferred || old != "CREATED" || mapped != "in_transit" {
		t.Fatalf("the resolving attempt returned (%q, %q, %v)", old, mapped, deferred)
	}
	if em.calls != 1 {
		t.Fatalf("the transition must emit exactly once, on landing; calls = %d", em.calls)
	}
	if got := s.GetOrder(id).State; got != "RUNNING" {
		t.Fatalf("the state must commit on landing; got %q", got)
	}

	// And once landed, re-driving the same state is neither a deferral nor an
	// emission (no-transition path).
	if _, _, deferred := s.DriveState(id, "RUNNING"); deferred {
		t.Fatalf("a same-state re-drive must not defer")
	}
	if em.calls != 1 {
		t.Fatalf("a same-state re-drive must not emit; calls = %d", em.calls)
	}
}

// The not-found case keeps its contract — empty everything, and NOT a
// deferral (a driver must not retry an order the simulator has never issued;
// that retry-forever is the drift the tombstones exist to name).
func TestDriveState_NotFoundIsNotDeferral(t *testing.T) {
	s := New()
	em := &silentEmitter{}
	s.InitTracker(em, fixedResolver{42})

	old, mapped, deferred := s.DriveState("sim-never-issued", "RUNNING")
	if deferred || old != "" || mapped != "" {
		t.Fatalf("not-found must be empty-and-not-deferred; got (%q, %q, %v)", old, mapped, deferred)
	}
}
