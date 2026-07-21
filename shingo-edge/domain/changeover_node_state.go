package domain

import (
	"database/sql/driver"

	"shingo/protocol"
)

// NodeTaskState is the typed state for a changeover_node_task row. Wraps
// string so it serializes natively over JSON and SQL while gaining
// compile-time distinction from raw strings and other enum-shaped state
// types (Changeover.State, StationTask.State, protocol.Status).
type NodeTaskState string

// Canonical node-task state constants. Names mirror the underlying
// string literals so a grep for the literal still finds the constant.
const (
	// Planner-emitted initial states.
	NodeTaskSwapRequired     NodeTaskState = "swap_required"
	NodeTaskStagingRequested NodeTaskState = "staging_requested"
	NodeTaskEmptyRequested   NodeTaskState = "empty_requested"
	NodeTaskReleaseRequested NodeTaskState = "release_requested"

	// Intermediate / terminal-by-situation states.
	NodeTaskStaged      NodeTaskState = "staged"
	NodeTaskLineCleared NodeTaskState = "line_cleared" // terminal for drop, intermediate for swap/evacuate

	// Clean terminal states.
	NodeTaskReleased  NodeTaskState = "released"
	NodeTaskSwitched  NodeTaskState = "switched"
	NodeTaskUnchanged NodeTaskState = "unchanged"

	// Terminal state with no current Go producer. Referenced by the
	// IsTerminal predicate and the operator-station HTML template as a
	// terminal state, but no in-tree code path writes it. Possibly a
	// legacy state from before some refactor, possibly set externally
	// (manual SQL during triage), possibly vestigial. Audit follow-up
	// tracked in SHINGO_TODO.md "Verify NodeTaskState 'verified'
	// production usage". Kept in the enum so existing DB rows still load.
	NodeTaskVerified NodeTaskState = "verified"

	// Failure-disposition states.
	NodeTaskCancelled NodeTaskState = "cancelled"
	NodeTaskError     NodeTaskState = "error"

	// NodeTaskAwaitingMaterial marks a swap/evacuate task whose SUPPLY order
	// is parked on Core with queue code waiting_for_material — the material
	// pool at the order's own node (its anchor) is dry, so Core queued the
	// order instead of failing it terminal (C(ii) supply widening). Written by
	// the orders manager when the queue-reason push lands; exits through the
	// normal staged-delivery writer when material arrives, or through operator
	// abandon (AbandonChangeoverSupply). NON-terminal with NO carve-out: an
	// awaiting task blocks changeover completion until one of those happens.
	NodeTaskAwaitingMaterial NodeTaskState = "awaiting_material"

	// NodeTaskAbandoned marks a task whose supply half the operator gave up
	// on — plain abandon (both halves cancelled) or accepted half-swap (evac
	// kept, supply cancelled; Core sees cancel reason accept_half_swap and
	// leaves the partner alone). TERMINAL: the changeover completes without
	// this node's new material, same semantic family as a Core-skipped supply
	// leg advancing to released. skip_note carries the operator-facing story.
	NodeTaskAbandoned NodeTaskState = "abandoned"

	// NodeTaskCapacityBlocked marks a drop task whose source-side fleet
	// dispatch failed because the destination (storage) is full —
	// distinct from NodeTaskLineCleared (no bin to remove, evac done)
	// and NodeTaskError (needs operator intervention). Renders amber on
	// the HMI: the system is waiting for space to open up, not broken.
	// The fulfillment scanner replays these when capacity opens via
	// CheckDropoffCapacity on the corresponding Core-side queued order.
	NodeTaskCapacityBlocked NodeTaskState = "capacity_blocked"
)

// String satisfies fmt.Stringer.
func (s NodeTaskState) String() string { return string(s) }

// Scan implements sql.Scanner for reading from a database column.
// Accepts string or []byte; NULL becomes the empty NodeTaskState.
// Does NOT validate the value against known constants — historical rows
// from retired states must still load. Same approach as protocol.Status.
func (s *NodeTaskState) Scan(v any) error {
	return protocol.ScanEnumNamed(s, v, "domain.NodeTaskState.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (s NodeTaskState) Value() (driver.Value, error) {
	return protocol.ValueEnum(s)
}

// IsTerminal reports whether the state represents a clean completion —
// the task finished its work and the changeover can advance toward
// "completed" if all sibling tasks also did.
//
// Excludes NodeTaskError (operator retry is expected) and NodeTaskCancelled
// (only set by cancelProcessChangeoverInternal, which moves the changeover
// row to "cancelled" rather than "completed", so the completion gate never
// reaches a row with cancelled tasks).
//
// NodeTaskLineCleared is terminal only for drop situations. For
// swap/evacuate it's an intermediate state ("Order B finished evacuating,
// waiting for Order A to deliver"); for drop there is no Order A — once
// the line is clear, the task is done.
//
// Single source of truth for the changeover completion gate, the
// auto-completion path, the node-changeover operator-entry guard, the
// per-station rollup, and the dashboard's "all nodes complete" indicator.
func (s NodeTaskState) IsTerminal(situation string) bool {
	switch s {
	case NodeTaskSwitched, NodeTaskVerified, NodeTaskUnchanged, NodeTaskReleased, NodeTaskAbandoned:
		return true
	case NodeTaskLineCleared:
		return situation == "drop"
	case NodeTaskCapacityBlocked:
		// Terminal for drop only — same reasoning as LineCleared. A drop
		// that hits a capacity block has done its source-side work (no
		// bin to remove from the line, or the bin can't be moved yet
		// because there's nowhere to put it). The changeover proceeds
		// while the underlying Core-side store/move order stays queued
		// until CheckDropoffCapacity passes. For swap/evacuate where
		// downstream choreography matters, capacity_blocked is NOT
		// terminal — the partner leg is still expected to materialize.
		return situation == "drop"
	}
	return false
}
