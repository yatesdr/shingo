package protocol

import (
	"database/sql/driver"
	"sort"
	"strconv"
	"strings"
)

// Status is the typed canonical order status. Wraps string so it serializes
// natively over JSON / SQL while gaining compile-time distinction from raw
// strings and other enum-shaped string types (e.g. bin status, ack outcome).
type Status string

// QueueCode is the typed, machine-readable category of WHY a queued order is
// waiting — the structured companion to the free-text queue_reason sentence.
// Five operator-visible codes; the sentence (held in queue_reason) is generated
// from the code plus a few parameters by the dispatch formatter, so every site
// that parks an order records the same code for the same physical cause and the
// floor/analytics can GROUP BY queue_code instead of parsing prose.
//
// Crosses the wire on OrderUpdate and OrderStatusSnapshot (additive; old Edge
// ignores it, new Edge with old Core sees "" and falls back to the sentence).
// Cause (the engineer-only call-site tag) stays Core-side and never crosses.
type QueueCode string

const (
	// QueueWaitingForMaterial: no source bin available right now — waiting on
	// inventory (a payload match, an empty carrier, or a partial of a multi-bin
	// set). Covers finder waits, claim races, dry empty pools, and swap supply
	// holds alike; full-vs-empty and partial-vs-complete are parameters, not
	// separate codes.
	QueueWaitingForMaterial QueueCode = "waiting_for_material"
	// QueueWaitingForSlot: the destination slot is occupied or in-flight — waiting
	// on dropoff capacity (a concrete slot, a saturated node group, or a contended
	// storage reserve).
	QueueWaitingForSlot QueueCode = "waiting_for_slot"
	// QueueStorageRearranging: the source bin is buried or its lane is mid-reshuffle
	// — waiting on storage to be rearranged so the material becomes reachable.
	QueueStorageRearranging QueueCode = "storage_rearranging"
	// QueueWaitingForPartner: a two-robot swap removal leg is holding until its
	// supply sibling secures a replacement bin.
	QueueWaitingForPartner QueueCode = "waiting_for_partner"
	// QueueFleetUnavailable: the fleet rejected the dispatch — waiting on the robot
	// system (transient; the order re-queues and retries).
	QueueFleetUnavailable QueueCode = "fleet_unavailable"
)

// Canonical order status constants shared by core and edge.
const (
	StatusPending      Status = "pending"
	StatusSourcing     Status = "sourcing"
	StatusQueued       Status = "queued"
	StatusSubmitted    Status = "submitted"
	StatusDispatched   Status = "dispatched"
	StatusAcknowledged Status = "acknowledged"
	StatusInTransit    Status = "in_transit"
	StatusDelivered    Status = "delivered"
	StatusConfirmed    Status = "confirmed"
	StatusStaged       Status = "staged"
	StatusFaulted      Status = "faulted"
	StatusFailed       Status = "failed"
	StatusCancelled    Status = "cancelled"
	StatusReshuffling  Status = "reshuffling"
	// StatusSkipped is the "the work was never needed" terminal status —
	// distinct from Failed (work attempted and errored) and Cancelled (work
	// aborted by external decision). Today its sole producer is
	// DispatchPreparedComplex: when ApplyComplexPlan finds zero bins at every
	// pickup node (the source was emptied externally — quality hold, manual
	// removal, etc.), the order moves to Skipped instead of Failed so the
	// operator-facing surface treats it as a no-op rather than an alarm.
	StatusSkipped Status = "skipped"
)

// IsTerminal reports whether the status has no outgoing transitions.
// Delegates to the package-level function to stay single-source-of-truth.
func (s Status) IsTerminal() bool {
	return IsTerminal(s)
}

// CanTransitionTo reports whether the (s, to) transition is allowed by the
// canonical state machine.
func (s Status) CanTransitionTo(to Status) bool {
	return IsValidTransition(s, to)
}

// String satisfies fmt.Stringer; convenient for log lines and debug output.
func (s Status) String() string {
	return string(s)
}

// Scan implements sql.Scanner for reading from a database column. Accepts
// string or []byte (both are common across drivers); NULL becomes the empty
// Status. Does not validate the value against AllStatuses() — historical rows
// from retired statuses must still load. Validation belongs at write time
// via DB CHECK constraints (deferred work) or at the LifecycleService.
func (s *Status) Scan(v any) error {
	return ScanEnumNamed(s, v, "protocol.Status.Scan")
}

// Value implements driver.Valuer for writing to a database column.
func (s Status) Value() (driver.Value, error) {
	return ValueEnum(s)
}

// AllStatuses returns every status defined in this module, used by
// table-driven tests that exhaustively cover the (from, to) matrix.
func AllStatuses() []Status {
	return []Status{
		StatusPending, StatusSourcing, StatusQueued, StatusSubmitted,
		StatusDispatched, StatusAcknowledged, StatusInTransit, StatusStaged,
		StatusDelivered, StatusConfirmed, StatusFaulted, StatusFailed, StatusCancelled,
		StatusReshuffling, StatusSkipped,
	}
}

// AllQueueCodes returns every queue code defined in this module. Used by the
// formatter's exhaustiveness test (walk every code through the formatter so an
// unhandled code is a test failure, not a silent default) and by readers that
// enumerate the analytic dimension. The empty string is intentionally NOT a
// member — it means "no code / pre-schema row", not a real category.
func AllQueueCodes() []QueueCode {
	return []QueueCode{
		QueueWaitingForMaterial,
		QueueWaitingForSlot,
		QueueStorageRearranging,
		QueueWaitingForPartner,
		QueueFleetUnavailable,
	}
}

// ValidQueueCode reports whether c is one of the defined queue codes. The empty
// string is valid (it means "uncoded" — a pre-schema row or a cleared reason);
// any other unknown value is not.
func ValidQueueCode(c QueueCode) bool {
	if c == "" {
		return true
	}
	for _, v := range AllQueueCodes() {
		if c == v {
			return true
		}
	}
	return false
}

// TermCode is the typed reason an order reached a terminal status — the
// terminal-side counterpart to QueueCode.
//
// These values were free-text constants in dispatch/planning_service.go,
// matched as string literals at producer and consumer sites and serialised
// verbatim into the DB. Promoting them here does three things: it puts the
// persisted vocabulary in the module both services share, it gives the same
// exhaustiveness guarantee QueueCode has, and it replaces the substring
// matching in domain/telemetry.go — the bug class that once classified 100%
// of failures as "Robot blocked" by looking for a word in a prose sentence.
//
// The string values are a PERSISTED, COMPARED CONTRACT: renaming a constant is
// safe, changing the string it holds is not.
type TermCode string

const (
	// TermNoSourceBin: nothing could be found to source from. One of only two
	// values that occur in practice at Springfield — which is why the
	// REFERENCE matters far more than the code (see order_history.ref).
	TermNoSourceBin TermCode = "no_source_bin"
	// TermGraceTimeout: the fleet reported FAILED and the grace period expired
	// without recovery. The other of the two live values.
	TermGraceTimeout TermCode = "grace_timeout"
	// TermNoPayload: the order carried no payload code to resolve.
	TermNoPayload TermCode = "no_payload"
	// TermNoBin: no bin matched the request.
	TermNoBin TermCode = "no_bin"
	// TermNoStorage: no storage destination could be resolved.
	TermNoStorage TermCode = "no_storage"
	// TermNoShuffleSlot: a reshuffle had nowhere to park the blocking bins.
	TermNoShuffleSlot TermCode = "no_shuffle_slot"
	// TermMissingSource: the request named no source and none could be derived.
	TermMissingSource TermCode = "missing_source"
	// TermInvalidNode: a named node does not exist or is not usable.
	TermInvalidNode TermCode = "invalid_node"
	// TermSameNode: source and destination resolved to the same node.
	TermSameNode TermCode = "same_node"
	// TermNodeError: a node lookup or state read failed.
	TermNodeError TermCode = "node_error"
	// TermClaimFailed: the bin or slot claim lost its race and did not recover.
	TermClaimFailed TermCode = "claim_failed"
	// TermLaneLocked: the lane was held by another order and stayed held.
	TermLaneLocked TermCode = "lane_locked"
	// TermReshuffleError: reshuffle planning failed structurally.
	TermReshuffleError TermCode = "reshuffle_error"
	// TermStructural: the request is malformed in a way retrying cannot fix.
	TermStructural TermCode = "structural"
	// TermUnknownType: no planner is registered for the order type.
	TermUnknownType TermCode = "unknown_type"
	// TermAbandoned: reconciliation gave up on an order stuck past its window.
	TermAbandoned TermCode = "abandoned"
	// TermOperatorCancelled: a human cancelled it. Deliberate, not a failure.
	TermOperatorCancelled TermCode = "operator_cancelled"
	// TermPeerTerminal: a swap sibling died and this leg was unwound with it.
	TermPeerTerminal TermCode = "peer_terminal"
	// TermNotNeeded: the work turned out to be unnecessary (the skip case —
	// every pickup node was already empty).
	TermNotNeeded TermCode = "not_needed"
)

// AllTermCodes returns every terminal code defined in this module. Used by the
// exhaustiveness test — walk every code through the classifier so an unhandled
// one is a test failure rather than a silent Other bucket. The empty string is
// intentionally NOT a member: it means "no code / pre-migration row".
func AllTermCodes() []TermCode {
	return []TermCode{
		TermNoSourceBin,
		TermGraceTimeout,
		TermNoPayload,
		TermNoBin,
		TermNoStorage,
		TermNoShuffleSlot,
		TermMissingSource,
		TermInvalidNode,
		TermSameNode,
		TermNodeError,
		TermClaimFailed,
		TermLaneLocked,
		TermReshuffleError,
		TermStructural,
		TermUnknownType,
		TermAbandoned,
		TermOperatorCancelled,
		TermPeerTerminal,
		TermNotNeeded,
	}
}

// ValidTermCode reports whether c is a defined terminal code. The empty string
// is valid — it means "uncoded", which every row written before the column
// existed is.
func ValidTermCode(c TermCode) bool {
	if c == "" {
		return true
	}
	for _, v := range AllTermCodes() {
		if c == v {
			return true
		}
	}
	return false
}

// TermRef is what a terminal reason CONCERNS — VDA 5050's errorReferences
// idea. It is not decoration.
//
// The live terminal-code distribution at Springfield is two values,
// no_source_bin and grace_timeout, so the code alone partitions a hundred
// failures into two buckets and answers nothing. "no_source_bin" is a
// category; "no_source_bin at PLN_01.R1 for 74577-6SA0A.06" is a job. The
// reference IS the resolution, which is why it is designed in from the start
// rather than added when a bare code column turns out to age badly.
//
// Stored as JSONB so starvation-by-cause is a GROUP BY (ref->>'payload')
// rather than a LIKE over prose.
type TermRef struct {
	// Node is the process / delivery node the reason concerns, dot-named.
	Node string `json:"node,omitempty"`
	// Payload is the part number that could not be sourced, delivered or found.
	Payload string `json:"payload,omitempty"`
	// Peer is the sibling order whose fate caused this one — swap legs unwound
	// together, reshuffle children abandoned with their parent.
	Peer int64 `json:"peer,omitempty"`
	// Detail is free text for the cases the three fields above do not cover.
	// Deliberately last and deliberately optional: anything that ends up here
	// repeatedly is a missing field, not a place to put prose.
	Detail string `json:"detail,omitempty"`
}

// Empty reports whether the reference carries nothing worth storing.
func (r TermRef) Empty() bool {
	return r.Node == "" && r.Payload == "" && r.Peer == 0 && r.Detail == ""
}

// String renders the reference the way the design writes it — the form that
// belongs in a log line beside the code:
//
//	node=PLN_01.R1, payload=74577-6SA0A.06
func (r TermRef) String() string {
	parts := make([]string, 0, 4)
	if r.Node != "" {
		parts = append(parts, "node="+r.Node)
	}
	if r.Payload != "" {
		parts = append(parts, "payload="+r.Payload)
	}
	if r.Peer != 0 {
		parts = append(parts, "peer="+strconv.FormatInt(r.Peer, 10))
	}
	if r.Detail != "" {
		parts = append(parts, r.Detail)
	}
	return strings.Join(parts, ", ")
}

// ─── Status set predicates ────────────────────────────────────────────────
//
// These predicates classify statuses by *intent*. Callers across the
// codebase (SQL filters, Go-side branches, dashboard filters) ask one
// of these predicates rather than hand-enumerating status lists. When a
// new status is added, the author has to consciously classify it into
// each predicate — the drift-detection tests in status_test.go force
// that decision.
//
// Layering note: IsTerminal lives in types.go because it derives from
// the state-machine table. The other predicates live here because they
// express application semantics (operator visibility, vendor lifecycle,
// dispatcher lifecycle) that overlap with terminality but aren't
// derivable from the transition graph alone.

// IsFailureTerminal reports whether the status represents an unsuccessful
// terminal outcome — work was attempted or aborted and ended badly.
// Excludes Confirmed (successful completion) and Skipped (deliberate
// no-op, the work was never needed). Used by anomaly detection that
// distinguishes "operator should look at this" from "this is done."
func IsFailureTerminal(s Status) bool {
	return s == StatusCancelled || s == StatusFailed
}

// IsFailureTerminal is the method form; delegates to the package function.
func (s Status) IsFailureTerminal() bool { return IsFailureTerminal(s) }

// IsVendorActive reports whether the fleet vendor has the order and is
// actively working on it. The order has crossed from Core's planning
// space into the floor-execution space. Used by adapter-poll filters
// and capacity gates that key on "is the robot touching this right now."
func IsVendorActive(s Status) bool {
	return s == StatusDispatched || s == StatusInTransit || s == StatusStaged
}

// IsVendorActive is the method form.
func (s Status) IsVendorActive() bool { return IsVendorActive(s) }

// IsPreDispatch reports whether the order is still in Core's planning
// space and has not yet been sent to the fleet vendor. Used by
// source-reference guards that need to know "would re-parenting this
// node break a not-yet-dispatched order."
func IsPreDispatch(s Status) bool {
	return s == StatusPending || s == StatusSourcing || s == StatusQueued
}

// IsPreDispatch is the method form.
func (s Status) IsPreDispatch() bool { return IsPreDispatch(s) }

// IsAcquiring reports whether the order is actively trying to acquire its source
// bin — either queued (waiting for the fulfillment scanner to pick it up) or
// sourcing (mid-reserve; the scanner retries it once commit 4 moves
// MoveToSourcing to the start of the reserve attempt). The scanner's scan set and
// re-check, and DispatchPreparedComplex's entry guard, all key on this so
// "retryable pre-dispatch state" has one definition. Narrower than IsPreDispatch,
// which also includes `pending` (pre-intake, not yet a scanner-retry candidate).
func IsAcquiring(s Status) bool {
	return s == StatusQueued || s == StatusSourcing
}

// IsAcquiring is the method form.
func (s Status) IsAcquiring() bool { return IsAcquiring(s) }

// IsRuntimeStuckCandidate reports whether an order whose updated_at is
// far in the past should be flagged as runtime-stuck. Excludes Faulted
// (intentional grace-period non-terminal), Delivered (waits for operator
// confirmation, no machine deadline), and Reshuffling (compound parent
// waiting on children — has its own watchdog). Used by reconciliation's
// stuck-order detector.
func IsRuntimeStuckCandidate(s Status) bool {
	switch s {
	case StatusPending, StatusSourcing, StatusSubmitted, StatusAcknowledged,
		StatusDispatched, StatusInTransit, StatusStaged:
		return true
	}
	return false
}

// IsRuntimeStuckCandidate is the method form.
func (s Status) IsRuntimeStuckCandidate() bool { return IsRuntimeStuckCandidate(s) }

// IsStuckSweepCandidate reports whether the DESTRUCTIVE AbandonStuckOrders sweep may
// auto-cancel an order that has sat past its timeout. This is the runtime, the-vendor-
// has-it-but-nothing-is-moving set: dispatched (handed to the fleet, the robot may never
// have started) and staged (a robot parked holding a bin). It deliberately EXCLUDES
// in_transit (an actively moving robot is not stuck) and ALL pre-dispatch waiting states
// (pending/sourcing/queued) — demand is operator-driven and never evaporates, so
// a waiting order holds INDEFINITELY and is never abandoned on a timer.
//
// Narrower than both IsVendorActive (which also includes in_transit) and the informational
// IsRuntimeStuckCandidate (which flags stale pre-dispatch / in_transit orders for the
// health board but must NEVER drive a cancel). Kept as its own predicate precisely because
// "worth surfacing as stale" and "safe to auto-cancel" are different questions.
func IsStuckSweepCandidate(s Status) bool {
	return s == StatusDispatched || s == StatusStaged
}

// IsStuckSweepCandidate is the method form.
func (s Status) IsStuckSweepCandidate() bool { return IsStuckSweepCandidate(s) }

// IsOperatorVisible reports whether the status should still appear on
// operator-facing HMI surfaces (edge ListActive, kanban demand pages,
// manual-message picker). Distinct from !IsTerminal: Failed orders stay
// operator-visible so the operator can retry or acknowledge them, even
// though they're terminal. Skipped/Confirmed/Cancelled are "done from
// the operator's POV" and disappear from the surface.
func IsOperatorVisible(s Status) bool {
	return s != StatusConfirmed && s != StatusCancelled && s != StatusSkipped
}

// IsOperatorVisible is the method form.
func (s Status) IsOperatorVisible() bool { return IsOperatorVisible(s) }

// ─── Changeover-start classification ─────────────────────────────────────
//
// A changeover may not start while LIVE CHOREOGRAPHY is still running at a
// node the changeover touches. "Live choreography" is narrower than
// !IsTerminal and wider than "in flight", and the difference is the whole
// point — see BlocksChangeoverStart.

// BlocksChangeoverStart reports whether an order in this status must reach a
// terminal state before a changeover may start at a node it touches.
//
// The set is IsVendorActive (dispatched / in_transit / staged) PLUS faulted.
//
// WHY THIS NAME EXISTS RATHER THAN THE EXPRESSION. Hopkinsville, 2026-07-28:
// orders 1249/1251 held bins bound for the indexed_over seats PLN_02/PLN_05
// while a changeover armed. Its index legs could not reserve because those
// carriers were still in transit; they landed minutes later and each leg
// claimed its bin one second after. The reserve loop did its job. Had the
// (since-removed) AbortNodeOrders sweep caught those orders, their carriers
// would have been cancelled mid-delivery and the index legs would have waited
// for bins that were never coming — a permanent deadlock. The reasoning for
// each member lives here so it travels with the set:
//
//   - dispatched / in_transit / staged: IsVendorActive. The fleet has it. A
//     staged robot is holding still mid-handshake — not "in flight", but
//     absolutely live, which is why "in flight" was the wrong phrasing.
//   - faulted: it failed WHILE HOLDING. The bin's location is unknown, so the
//     only safe assumption is that recovery happens and it must go terminal.
//
// And what is deliberately NOT here:
//
// And what is deliberately NOT here — but read the second half of each entry,
// because "does not block" is only half a decision and getting the other half
// wrong is what broke SNF2 on 30 July:
//
//   - pending / sourcing / queued: no carrier has been assigned, so cancelling
//     them provably cannot strand one. That is the entire difference between
//     this gate and the sweep that was removed. THEY ARE CANCELLED.
//   - submitted / acknowledged: Edge-lifecycle words, not fleet states. RDS
//     never emits acknowledged (fleet/seerrds never mentions it) and Core's
//     vendor ladder starts at dispatched. Nothing is moving. They must also
//     never block for a second, independent reason: NOTHING REAPS THEM.
//     AbandonStuckOrders is scoped to {dispatched, staged}, no Core reconciler
//     or Edge ticker transitions them, and this HMI exposes no operator order
//     cancel — so blocking here would lock an operator out of changeover until
//     somebody restarted Edge. THEY ARE ALSO CANCELLED, and originally they
//     were not: the reasoning above stops at "must not block" and was allowed
//     to imply "therefore leave alone". Two of them outlived a changeover at
//     SNF2 and the node took deliveries for two styles at once.
//   - delivered: the bin physically arrived. The choreography is over and only
//     the operator's confirm is outstanding. Note it is NOT auto-confirmed to
//     clear the gate — ConfirmDelivery asserts a count, and asserting a count
//     against an order the changeover does not own is how ledger errors are
//     manufactured.
//   - reshuffling: a compound parent waiting on children that rearrange
//     storage, not a carrier bound for the line.
//
// DEFINED VIA THE CLASSIFIER so this gate and the cancel sweep read one source.
// It was IsVendorActive(s) || s == StatusFaulted, which is the same set, but two
// independent spellings of a set drift the moment one is edited — and the SNF2
// defect was exactly a second spelling (IsPreDispatch) quietly disagreeing.
func BlocksChangeoverStart(s Status) bool {
	return ChangeoverStartActionFor(s) == ChangeoverStartBlock
}

// BlocksChangeoverStart is the method form.
func (s Status) BlocksChangeoverStart() bool { return BlocksChangeoverStart(s) }

// ChangeoverStartAction is what changeover initiation does about an order in a
// given status. Exactly one value applies to every status, and the zero value
// means "nobody has decided" — which is what makes the exhaustiveness test
// possible (see ChangeoverStartActionFor).
type ChangeoverStartAction uint8

const (
	// ChangeoverStartUnclassified is the zero value and is never a legitimate
	// answer. It exists so that a status added to the enum without a decision
	// here fails a test instead of silently acquiring whichever behaviour the
	// fallthrough happened to give it.
	ChangeoverStartUnclassified ChangeoverStartAction = iota
	// ChangeoverStartCancel: the order is cancelled at changeover initiation.
	ChangeoverStartCancel
	// ChangeoverStartBlock: the changeover refuses to start until it clears.
	ChangeoverStartBlock
	// ChangeoverStartPass: neither cancelled nor blocking. Left alone.
	ChangeoverStartPass
)

func (a ChangeoverStartAction) String() string {
	switch a {
	case ChangeoverStartCancel:
		return "cancel"
	case ChangeoverStartBlock:
		return "block"
	case ChangeoverStartPass:
		return "pass"
	default:
		return "unclassified"
	}
}

// ChangeoverStartActionFor classifies one status for changeover initiation.
//
// DELIBERATELY AN EXPLICIT SWITCH OVER EVERY STATUS, not a chain of predicate
// calls. A predicate chain ending in "everything else passes" cannot tell a
// considered pass from a status nobody thought about, so a new status would
// silently join the pass set — the most permissive answer, arrived at by
// accident. Here a new status falls to default and TestChangeoverStartActionIsExhaustive
// fails. Same shape as FormatQueueSentence's default arm and AllQueueCodes.
//
// THIS FUNCTION IS THE AUTHORITY. Both the block gate and the cancel sweep call
// it. That is a correction: it was written as a cross-check beside two other
// predicates and nothing in production called it, so its exhaustiveness test
// proved only that a function nobody ran was total. Springfield SNF2, 30 July,
// is what that cost — below.
//
// SUBMITTED AND ACKNOWLEDGED ARE CANCELLED, NOT PASSED. They were passed, and it
// was wrong. The lifecycle runs
//
//	pending → submitted → acknowledged → sourcing/queued → dispatched → in_transit
//
// so those two sit BETWEEN pending and sourcing, all four of which are before a
// robot holds anything. Passing them punched a two-status hole in the middle of
// a contiguous pre-fleet region, and an order that happened to be sitting in the
// hole at the moment a changeover started survived it.
//
// SNF2 on 30 July: two complex orders for the outgoing style reached
// `acknowledged` thirteen seconds before the operator started a changeover. They
// did not block the start, correctly, because no robot had them. They were also
// not cancelled, so the changeover created its own orders for the incoming style
// against the same node and the line had deliveries for two different styles
// live at once.
//
// The hole came from reusing IsPreDispatch for the cancel sweep. That predicate
// belongs to the fulfillment scanner and means "retryable acquisition state",
// which is a different question from "is anything holding this yet". The name
// fit and the membership did not.
func ChangeoverStartActionFor(s Status) ChangeoverStartAction {
	switch s {
	case StatusPending, StatusSubmitted, StatusAcknowledged, StatusSourcing, StatusQueued:
		// Pre-fleet, contiguous. Nothing is carrying a bin for any of these, so
		// cancelling cannot strand a changeover the way the Hopkinsville case
		// did — that hazard begins at dispatched.
		return ChangeoverStartCancel
	case StatusDispatched, StatusInTransit, StatusStaged, StatusFaulted:
		return ChangeoverStartBlock
	case StatusDelivered, StatusReshuffling:
		return ChangeoverStartPass
	case StatusConfirmed, StatusCancelled, StatusFailed, StatusSkipped:
		// Terminal. There is nothing to cancel and nothing to wait for.
		return ChangeoverStartPass
	default:
		return ChangeoverStartUnclassified
	}
}

// ─── SQL projectors ──────────────────────────────────────────────────────
//
// Each predicate above has a matching SQL list helper that returns the
// comma-separated quoted status values for which the predicate is true,
// suitable for splicing into a SQL `status IN (...)` or `NOT IN (...)`
// clause. The list is built once at package init by walking AllStatuses()
// through the predicate; values are sorted lexically for deterministic
// output (drift tests depend on it).
//
// Splice is safe — values come from the Status enum, not user input.
// Callers use the helpers as:
//
//     fmt.Sprintf(`... WHERE status IN (%s) ...`, protocol.TerminalStatusSQLList())
//     fmt.Sprintf(`... WHERE status NOT IN (%s) ...`, protocol.TerminalStatusSQLList())

var (
	terminalStatusSQLList              = buildStatusSQLList(IsTerminal)
	nonTerminalStatusSQLList           = buildStatusSQLList(func(s Status) bool { return !IsTerminal(s) })
	failureTerminalStatusSQLList       = buildStatusSQLList(IsFailureTerminal)
	vendorActiveStatusSQLList          = buildStatusSQLList(IsVendorActive)
	preDispatchStatusSQLList           = buildStatusSQLList(IsPreDispatch)
	acquiringStatusSQLList             = buildStatusSQLList(IsAcquiring)
	runtimeStuckCandidateStatusSQLList = buildStatusSQLList(IsRuntimeStuckCandidate)
	stuckSweepStatusSQLList            = buildStatusSQLList(IsStuckSweepCandidate)
	operatorVisibleStatusSQLList       = buildStatusSQLList(IsOperatorVisible)
)

// buildStatusSQLList walks every known status, filters by the predicate,
// quotes each value, and joins with commas. Sorted lex for deterministic
// output so drift tests (status_test.go) can pin the exact string form.
func buildStatusSQLList(pred func(Status) bool) string {
	var parts []string
	for _, s := range AllStatuses() {
		if pred(s) {
			parts = append(parts, "'"+string(s)+"'")
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}

// TerminalStatusSQLList returns 'cancelled','confirmed','failed','skipped'.
// Use in `status IN (TerminalStatusSQLList())` or its NOT IN inverse.
func TerminalStatusSQLList() string { return terminalStatusSQLList }

// NonTerminalStatusSQLList returns every status that is not terminal.
// Provided as a positive form for queries that read more naturally as
// `status IN (...)` than `NOT IN (...)`. Most callers want the negation
// of TerminalStatusSQLList and should use that with NOT IN instead;
// this helper exists for the handful of cases where positive-form IN
// is clearer.
func NonTerminalStatusSQLList() string { return nonTerminalStatusSQLList }

// FailureTerminalStatusSQLList returns 'cancelled','failed'.
// Use for anomaly detection that wants "the operator should look at
// this" rather than the broader "this is done."
func FailureTerminalStatusSQLList() string { return failureTerminalStatusSQLList }

// VendorActiveStatusSQLList returns 'dispatched','in_transit','staged'.
// Use for vendor-side polling filters and floor-execution capacity gates.
func VendorActiveStatusSQLList() string { return vendorActiveStatusSQLList }

// PreDispatchStatusSQLList returns 'pending','queued','sourcing'.
// Use for source-reference guards (re-parent, delete, rename a node
// referenced by not-yet-dispatched orders).
func PreDispatchStatusSQLList() string { return preDispatchStatusSQLList }

// AcquiringStatusSQLList returns 'queued','sourcing' — the fulfillment scanner's
// retry set. Use in `status IN (AcquiringStatusSQLList())`.
func AcquiringStatusSQLList() string { return acquiringStatusSQLList }

// RuntimeStuckCandidateStatusSQLList returns the non-terminal subset
// that should be watched for stale updated_at — excludes faulted,
// delivered, reshuffling per the predicate's doc.
func RuntimeStuckCandidateStatusSQLList() string { return runtimeStuckCandidateStatusSQLList }

// StuckSweepStatusSQLList returns 'dispatched','staged' — the runtime states the
// destructive AbandonStuckOrders sweep may auto-cancel. Excludes in_transit and all
// pre-dispatch waiting (demand is operator-driven). Use in `status IN (StuckSweepStatusSQLList())`.
func StuckSweepStatusSQLList() string { return stuckSweepStatusSQLList }

// OperatorVisibleStatusSQLList returns the statuses that should still
// appear on Edge HMI surfaces. Skipped/Confirmed/Cancelled are excluded;
// Failed is intentionally included so the operator can retry.
func OperatorVisibleStatusSQLList() string { return operatorVisibleStatusSQLList }
