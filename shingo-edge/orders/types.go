package orders

import (
	"encoding/json"

	"shingo/protocol"
)

// Order types — aliased to the canonical typed constants in protocol so
// edge and core agree on the wire shape and Go callers get compile-time
// distinction from raw strings.
const (
	TypeRetrieve = protocol.OrderTypeRetrieve
	TypeMove     = protocol.OrderTypeMove
	TypeComplex  = protocol.OrderTypeComplex
)

// Order statuses aliased from protocol.
//
// Edge mirrors Core's full status vocabulary: sourcing/dispatched/faulted are
// stored on the Edge row when Core pushes them via order.update or a boot
// snapshot, so the operator sees the truth of whichever machine owns the order
// at that moment. See orders.ApplyCoreStatus for the mapping shared by the
// live-push and snapshot paths.
const (
	StatusPending      = protocol.StatusPending
	StatusSourcing     = protocol.StatusSourcing
	StatusQueued       = protocol.StatusQueued
	StatusSubmitted    = protocol.StatusSubmitted
	StatusDispatched   = protocol.StatusDispatched
	StatusAcknowledged = protocol.StatusAcknowledged
	StatusInTransit    = protocol.StatusInTransit
	StatusStaged       = protocol.StatusStaged
	StatusDelivered    = protocol.StatusDelivered
	StatusConfirmed    = protocol.StatusConfirmed
	StatusCancelled    = protocol.StatusCancelled
	StatusFailed       = protocol.StatusFailed
	StatusSkipped      = protocol.StatusSkipped
	StatusReshuffling  = protocol.StatusReshuffling
	StatusFaulted      = protocol.StatusFaulted
)

// Dispatch reply types — used by HandleDispatchReply and edge_handler.
const (
	ReplyAck       = "ack"
	ReplyWaybill   = "waybill"
	ReplyUpdate    = "update"
	ReplyDelivered = "delivered"
	ReplyError     = "error"
	ReplySkipped   = "skipped"
	ReplyStaged    = "staged"
	ReplyCancelled = "cancelled"
	ReplyQueued    = "queued"
)

// IsValidTransition delegates to the canonical state machine in protocol.
func IsValidTransition(from, to protocol.Status) bool {
	return protocol.IsValidTransition(from, to)
}

// IsTerminal delegates to the canonical definition in protocol.
func IsTerminal(status protocol.Status) bool {
	return protocol.IsTerminal(status)
}

// ReleasableAtCore reports whether Core will ACCEPT an OrderRelease for an
// order in this status. It mirrors Core's precondition verbatim — see
// shingo-core/dispatch/complex_release.go, which rejects anything that is
// neither staged nor in_transit with an "invalid_state" error (in_transit is
// accepted for duplicate fan-out from the consolidated two-robot release and
// for multi-wait re-release).
//
// Why callers need this: Manager.ReleaseOrderWithDisposition guards only
// terminal + pending/submitted, and then transitions the Edge row to
// in_transit locally. So releasing an order that is queued / sourcing /
// dispatched / acknowledged queues an envelope Core will refuse AND moves the
// Edge row anyway — a persistent Edge/Core status divergence plus a bogus
// "released" count. Ask this first and skip instead.
//
// Deliberately status-only. Core resolves the order by UUID, not by Edge's
// mirrored WaybillID, and enforces its own dispatch precondition; requiring a
// non-nil waybill here would add false negatives (a staged leg whose waybill
// write lagged would be skipped and the operator would have to click twice)
// without removing any real failure. Staged/in_transit already implies
// dispatched.
//
// Faulted is intentionally NOT releasable: Core no-ops a faulted release
// rather than erroring, so skipping it costs nothing and saves a round trip.
//
// WHAT "SKIPPED" COSTS DEPENDS ON THE CALLER, and the two differ. This is the
// load-bearing distinction; do not reason about one path from the other.
//
//   - ReleaseStagedOrders (the /release-staged button) DEFERS. Hop A4-ii records
//     a skipped leg via rememberDeferredSiblingRelease and re-fires it when it
//     reaches staged, so the operator's single click means "go for the pair,
//     defer what Core will not take yet". Nothing is lost, so nothing upstream
//     of that button needs to gate on this predicate — and gating on it would
//     remove the operator's ability to express the deferral at all.
//
//   - HandleBinPickedUp's deferred-supply branch (the CHANGEOVER release-wait
//     path) DROPS. It calls releaseIfReleasable and registers no re-fire, so a
//     supply skipped there is not retried while the evac has already lifted the
//     line's bin. That is why the operator-station glow (isReleaseReady) waits
//     for the supply on the changeover path and swap_ready does not on the
//     production path. The two look alike and are not interchangeable.
func ReleasableAtCore(status protocol.Status) bool {
	return status == StatusStaged || status == StatusInTransit
}

// StationOwnsWait reports whether the station may advance the wait this order is
// parked at — read off the plan, not guessed.
//
// ── IT USED TO BE UNANSWERABLE FROM HERE ──────────────────────────────────
//
// Core's fence refuses a release for a wait only its lane evaluator can advance,
// and it was right to. But the Edge held no way to tell which kind it had, so
// the board either offered a button that could not work or offered nothing and
// explained nothing. The sim operator papered over it with a three-strike retry
// cap — "most likely a LANE wait, the Edge cannot see that from here" — which
// converted "pushes Release forever" into "abandons forever", and abandoned
// three station-owned waits it could have released (§12.49).
//
// W1 put the owner on the step, so this is now a read. An untagged wait is the
// station's for the duration of the drain window — the meaning every pre-ruling
// plan already had — which keeps old orders releasable while the field spreads.
//
// A plan that cannot be parsed, or an order parked at no wait at all, answers
// FALSE: the board should not offer an action whose effect it cannot predict,
// and Core's hard release is the escape hatch for anything this refuses.
func StationOwnsWait(stepsJSON string, waitIndex int) bool {
	if stepsJSON == "" {
		return false
	}
	var steps []struct {
		Action   string `json:"action"`
		WaitKind string `json:"wait_kind"`
	}
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return false
	}
	seen := 0
	for _, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			// "" is the drain window's default: pre-ruling plans carry no kind and
			// have always been the station's to release.
			return s.WaitKind == waitKindStation || s.WaitKind == ""
		}
		seen++
	}
	return false
}

// waitKindStation mirrors dispatch.WaitKindStation — see the note on the Edge's
// other copy in engine/material_orders.go. Duplicated because Edge cannot import
// Core. NOT PINNED: the engine copy is (TestWaitKindStation_MatchesCore), this
// one is not, and the note that claimed it was named a test that has never
// existed.
const waitKindStation = "station"
