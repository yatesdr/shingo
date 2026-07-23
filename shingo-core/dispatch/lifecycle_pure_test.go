// lifecycle_pure_test.go — Tests that don't require a database.
//
// These run on every `go test ./...` invocation (no //go:build docker
// tag) and cover the parts of lifecycle.go that are pure computation:
// error types, helper functions, and structural invariants of the
// actionMap. The driver behaviour against real persistence is in
// lifecycle_test.go (docker-tagged).

package dispatch

import (
	"errors"
	"fmt"
	"testing"

	"shingo/protocol"
)

func TestIllegalTransition_ErrorFormat(t *testing.T) {
	t.Parallel()
	err := IllegalTransition{From: StatusStaged, To: StatusInTransit}
	got := err.Error()
	want := fmt.Sprintf("illegal transition: %s → %s", StatusStaged, StatusInTransit)
	if got != want {
		t.Errorf("Error() = %q, want %q", got, want)
	}
}

func TestIsIllegalTransition_Direct(t *testing.T) {
	t.Parallel()
	err := IllegalTransition{From: StatusPending, To: StatusDelivered}
	if !IsIllegalTransition(err) {
		t.Error("IsIllegalTransition returned false for direct IllegalTransition")
	}
}

func TestIsIllegalTransition_Wrapped(t *testing.T) {
	t.Parallel()
	inner := IllegalTransition{From: StatusPending, To: StatusDelivered}
	wrapped := fmt.Errorf("persist failed: %w", inner)
	if !IsIllegalTransition(wrapped) {
		t.Error("IsIllegalTransition returned false for wrapped IllegalTransition (errors.As should unwrap)")
	}
}

func TestIsIllegalTransition_OtherError(t *testing.T) {
	t.Parallel()
	if IsIllegalTransition(errors.New("not an illegal transition")) {
		t.Error("IsIllegalTransition returned true for unrelated error")
	}
	if IsIllegalTransition(nil) {
		t.Error("IsIllegalTransition returned true for nil")
	}
}

// TestActionMap_KeysAreValidTransitions asserts every (from, to) key in
// actionMap is also a legal transition in protocol.validTransitions.
// An action map entry for an illegal transition would be unreachable
// dead code (transition() rejects before firing actions).
func TestActionMap_KeysAreValidTransitions(t *testing.T) {
	t.Parallel()
	for key := range actionMap {
		if !protocol.IsValidTransition(key.from, key.to) {
			t.Errorf("actionMap has entry %s→%s which is NOT in protocol.validTransitions — entry is unreachable", key.from, key.to)
		}
	}
}

// TestActionMap_NoNilActions asserts every action slot in actionMap is
// non-nil. A nil action would panic at dispatch time.
func TestActionMap_NoNilActions(t *testing.T) {
	t.Parallel()
	for key, actions := range actionMap {
		if len(actions) == 0 {
			t.Errorf("actionMap[%s→%s] has empty action slice — remove the entry instead", key.from, key.to)
			continue
		}
		for i, action := range actions {
			if action == nil {
				t.Errorf("actionMap[%s→%s][%d] is nil", key.from, key.to, i)
			}
		}
	}
}

// TestActionMap_TerminalCoverage asserts every non-terminal status that Core
// can actually reach has an emitCancelled and emitFailed action wired for its
// (from, Cancelled) and (from, Failed) transitions. The action map is the
// contract for "engine wiring receives a notification when an order terminates
// from status X" — a missing entry means a class of terminations would silently
// skip the notification path.
//
// The status vocabulary is shared between Core and Edge (both validate against
// protocol.validTransitions), but Core's planning machine never holds every
// status in that table. coreUnreachableStatuses names the statuses that appear
// in the shared table yet no Core code path ever writes to a Core order row —
// they are Edge-lifecycle words. Core can never transition OUT of them, so
// requiring a cancel/fail action for them would guard an impossible transition.
// See docs/order-lifecycle.md ("Two machines, one enum") for the full picture.
//
// This is a NAMED EXEMPTION, not a loosened assertion: adding a genuinely
// Core-reachable non-terminal status without cancel/fail actions still fails
// this test. If a status moves from Edge-only to Core-reachable, remove it
// from coreUnreachableStatuses AND add the missing action entries together.
func TestActionMap_TerminalCoverage(t *testing.T) {
	t.Parallel()
	for fromStr := range protocol.AllValidTransitions() {
		from := protocol.Status(fromStr)
		if coreUnreachableStatuses[from] {
			continue
		}
		// Every Core-reachable non-terminal status that allows a Cancelled
		// transition must have an emitCancelled action.
		if protocol.IsValidTransition(from, StatusCancelled) {
			actions := actionMap[transitionKey{from, StatusCancelled}]
			if len(actions) == 0 {
				t.Errorf("actionMap[%s→Cancelled] missing — non-terminal status %s can be cancelled but has no notification action", from, from)
			}
		}
		if protocol.IsValidTransition(from, StatusFailed) {
			actions := actionMap[transitionKey{from, StatusFailed}]
			if len(actions) == 0 {
				t.Errorf("actionMap[%s→Failed] missing — non-terminal status %s can fail but has no notification action", from, from)
			}
		}
	}
}

// coreUnreachableStatuses lists statuses present in the shared
// protocol.validTransitions table that no Core code path ever writes to a Core
// order row. They exist in the table because Edge validates its own lifecycle
// against it, but Core's machine never holds them, so an actionMap entry keyed
// on transitioning OUT of one could never fire.
//
//   - submitted: Edge's word for "the order envelope is in Edge's outbox." Core
//     never produces a submitted row.
//   - acknowledged: Core's vendor adapter maps fleet states via MapState, which
//     never returns acknowledged (the ladder starts at dispatched). Core's only
//     Acknowledge call site is a defensive, never-firing arm.
var coreUnreachableStatuses = map[protocol.Status]bool{
	StatusSubmitted:    true,
	StatusAcknowledged: true,
}

// TestEvent_FieldsAreOptional documents the Event struct contract: all
// fields are optional and can be left as zero values. Locks the bag-of-
// fields shape so per-method context structs don't sneak in without an
// explicit decision.
func TestEvent_FieldsAreOptional(t *testing.T) {
	t.Parallel()
	ev := Event{} // zero value — all fields optional
	if ev.Actor != "" || ev.Reason != "" || ev.PreviousStatus != "" {
		t.Errorf("Event zero value is unexpectedly non-empty: %+v", ev)
	}
}

func TestIsPostDelivery(t *testing.T) {
	t.Parallel()
	if !IsPostDelivery(StatusDelivered) {
		t.Error("IsPostDelivery(delivered) = false, want true")
	}
	if !IsPostDelivery(StatusConfirmed) {
		t.Error("IsPostDelivery(confirmed) = false, want true")
	}
	notPost := []protocol.Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusFailed, StatusCancelled, StatusFaulted, StatusReshuffling,
	}
	for _, s := range notPost {
		if IsPostDelivery(s) {
			t.Errorf("IsPostDelivery(%q) = true, want false", s)
		}
	}
}
