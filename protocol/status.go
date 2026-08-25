package protocol

import (
	"database/sql/driver"
	"sort"
	"strconv"
	"strings"
	"time"
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
	// TermReadFailed: a read Core needed did not answer. Transient — the planner
	// parks and retries — so it is not expected on a terminal row; declared here
	// because every planning code is bound to this vocabulary. A node that is
	// genuinely absent is TermInvalidNode, and the two must not be confused: one
	// is a database hiccup, the other is a human's job to fix.
	TermReadFailed TermCode = "read_failed"
	// TermBlockerClaimed: a bin a dig had to move was claimed by an order outside
	// the compound. Congestion — the planner treats it as transient and parks —
	// so it is not expected on a terminal row; declared here because every
	// planning code except the loader one is bound to this vocabulary, and a
	// future terminal path for it must not have to invent a string.
	TermBlockerClaimed TermCode = "blocker_claimed"
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
		TermReadFailed,
		TermBlockerClaimed,
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

// TermRef is what a reason CONCERNS — VDA 5050's errorReferences idea. It is
// not decoration.
//
// Named for the terminal rows it was built for, it rides non-terminal reason
// rows too — queued and faulted history rows already carry {node, payload}
// through historyReason's default. That is deliberate: "where" is worth the
// same on a row that explains a wait as on one that explains an ending.
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
	// VendorCode is the fleet's own numeric reason, as RDS reported it on the
	// ORDER (OrderDetail.Errors[].Code) — not a robot's standing alarm, which
	// is a different stream and is background rather than cause. Zero means the
	// fleet gave no reason, which is the common case: ~94% of faulted orders
	// carry no errors[] entry at all, so this field is absent far more often
	// than it is present and its absence is information, not a gap.
	//
	// A reference, not a category. It is deliberately NOT a typed code
	// vocabulary: one vendor code over 22 events is not a population to
	// classify, and order_history.code stays empty on faulted rows. Classify at
	// read time on ref->>'vendor_code' if a plant ever produces the variety.
	VendorCode int `json:"vendor_code,omitempty"`
	// VendorDesc is the fleet's text for VendorCode, stored as received. No
	// translation table until there is something to translate — the live values
	// are "cannot replan" and "Robot Suspended", which need none.
	VendorDesc string `json:"vendor_desc,omitempty"`
	// Detail is free text for the cases the fields above do not cover.
	// Deliberately last and deliberately optional: anything that ends up here
	// repeatedly is a missing field, not a place to put prose.
	Detail string `json:"detail,omitempty"`
}

// Empty reports whether the reference carries nothing worth storing.
func (r TermRef) Empty() bool {
	return r.Node == "" && r.Payload == "" && r.Peer == 0 &&
		r.VendorCode == 0 && r.VendorDesc == "" && r.Detail == ""
}

// String renders the reference the way the design writes it — the form that
// belongs in a log line beside the code:
//
//	node=PLN_01.R1, payload=74577-6SA0A.06
func (r TermRef) String() string {
	parts := make([]string, 0, 6)
	if r.Node != "" {
		parts = append(parts, "node="+r.Node)
	}
	if r.Payload != "" {
		parts = append(parts, "payload="+r.Payload)
	}
	if r.Peer != 0 {
		parts = append(parts, "peer="+strconv.FormatInt(r.Peer, 10))
	}
	if r.VendorCode != 0 {
		parts = append(parts, "vendor="+strconv.Itoa(r.VendorCode))
	}
	if r.VendorDesc != "" {
		parts = append(parts, "vendor_desc="+r.VendorDesc)
	}
	if r.Detail != "" {
		parts = append(parts, r.Detail)
	}
	return strings.Join(parts, ", ")
}

// ─── The fault sentence ───────────────────────────────────────────────────

// FaultPhase selects which of the fault sentences to render. A faulted order
// passes through at most three of them: it is live, and then it either
// recovered or Core gave up on it.
//
// It exists because the times alone cannot tell those apart — a recovery and a
// grace expiry are both "a faulted row followed by something else", and the
// difference is which transition fired, not how long it took.
type FaultPhase string

const (
	// FaultPhaseLive is a faulted order that is still faulted: the robot may
	// yet recover it and an operator may yet finish or cancel it. Renders
	// "Replanning" or "Fault", never anything with "fail" in it.
	FaultPhaseLive FaultPhase = "live"
	// FaultPhaseRecovered is the history row written when the fleet reports the
	// order moving again.
	FaultPhaseRecovered FaultPhase = "recovered"
	// FaultPhaseGaveUp is the terminal row written when the grace window closes
	// with the order still faulted.
	FaultPhaseGaveUp FaultPhase = "gave_up"
)

// FormatFaultSentence renders the operator-visible sentence for a faulted
// order. This is the ONE place the wording lives — Core's board, the order
// modal, the robots page, the health strip and the Edge board all print what
// this returns, so they agree by construction rather than by five files
// happening to say the same thing.
//
// It renders the STATIC part only. The two live durations — how long the order
// has been faulted, and how long until Core gives up — tick in the browser from
// data-since / data-until attributes, because a sentence with a baked-in
// "3m 12s" is wrong one second after it is rendered and the page that would
// have to re-fetch to fix it is the page this design just stopped reloading.
// The recovered and gave-up phases DO carry their duration: those are history
// rows, and a history row's duration is a fact that has stopped changing.
//
// THE WORDING RULE, and the reason for the test that pins it: a faulted order
// is still live. The robot can recover it, an operator can finish or cancel it.
// No sentence for a live order may contain "fail" in any form — that word
// belongs to the `failed` badge of an order that actually is one, and an
// operator who reads "failing" on a 20-second replan learns to ignore the word
// on the 45-minute one. "Gives up" is what Core does at grace expiry; "gave up"
// is what it did.
//
// notice is the server's decision (now-since >= config FaultNoticeAfter) about
// whether this is a replan or a fault worth the word. It is passed in rather
// than computed here because the threshold is config and protocol does not read
// config; every caller gets it from the same place.
//
// There is deliberately NO deadline parameter. The grace deadline is a live
// countdown ("gives up in 41 m"), so it belongs to the browser beside the other
// live duration; a formatter that accepted it and ignored it would invite
// someone to render it here, which is the bug this comment prevents.
func FormatFaultSentence(phase FaultPhase, ref TermRef, since, now time.Time, notice bool) string {
	switch phase {
	case FaultPhaseRecovered:
		return "Recovered after " + FormatDuration(faultElapsed(since, now))
	case FaultPhaseGaveUp:
		return joinFaultParts("Gave up after "+FormatDuration(faultElapsed(since, now)), vendorReason(ref))
	case FaultPhaseLive:
		// Under the threshold this is a replan, and a replan is not a fault an
		// operator should walk toward. The vendor reason is withheld with the
		// word: "cannot replan (60011)" beside "Replanning" reads as a
		// contradiction, and at 14 seconds it is not yet true.
		if !notice {
			return "Replanning"
		}
		return joinFaultParts("Fault", vendorReason(ref))
	default:
		return ""
	}
}

// vendorReason renders the fleet's own words for the fault, or "" when it gave
// none — which is the common case. Lower-cased as received and never
// translated: the two live values ("cannot replan", "Robot Suspended") are
// already sentences, and a mapping table built on one code over 22 events would
// be a vocabulary invented ahead of its data.
//
// The code rides in parentheses because it is the thing a plant quotes to the
// vendor, and the description alone is not searchable in RDS.
func vendorReason(ref TermRef) string {
	desc := strings.ToLower(strings.TrimSpace(ref.VendorDesc))
	switch {
	case desc != "" && ref.VendorCode != 0:
		return desc + " (" + strconv.Itoa(ref.VendorCode) + ")"
	case desc != "":
		return desc
	case ref.VendorCode != 0:
		// A code with no text still names the thing to look up.
		return "fleet code " + strconv.Itoa(ref.VendorCode)
	}
	return ""
}

// faultElapsed is now-since, floored at zero and rounded to the second. A
// negative elapsed means the two clocks disagree, not that the fault is in the
// future, and "-3 s" on the floor reads as a bug in the page.
func faultElapsed(since, now time.Time) time.Duration {
	if since.IsZero() || now.IsZero() {
		return 0
	}
	d := now.Sub(since)
	if d < 0 {
		return 0
	}
	return d.Round(time.Second)
}

// joinFaultParts joins the sentence's clauses with the middot the rest of the
// UI uses, skipping the ones that are absent so a missing vendor reason never
// leaves a dangling separator.
func joinFaultParts(parts ...string) string {
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		if p != "" {
			kept = append(kept, p)
		}
	}
	return strings.Join(kept, " · ")
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

// IsVendorTracked reports whether the fleet still holds this order and Core must
// keep watching it. IsVendorActive plus faulted.
//
// The difference from IsVendorActive is the whole point, and it is a bug this
// predicate exists to close. IsVendorActive asks "is the robot touching this
// right now", and a faulted order is not — it failed and stopped. But Core is
// still waiting on it: the grace period that ends a fault only runs while the
// poller is watching, and the poller only watches what it was told to track.
//
// On a restart, orders are re-registered with the tracker from the database, and
// that query used IsVendorActive. So a faulted order came back from a restart
// untracked, was never polled, and its grace period never expired — nothing
// could move it, and nothing anywhere noticed:
//
//   - IsRuntimeStuckCandidate excludes faulted deliberately (it is a grace-period
//     state and its own timer is supposed to end it), so no anomaly was raised.
//   - IsStuckSweepCandidate excludes it, so no sweep touched it.
//   - BlocksChangeoverStart INCLUDES it — see the reasoning there — so it went on
//     blocking changeovers at its node, permanently.
//
// That last one is the invariant that was missing and is now a test:
// EVERY STATUS THAT BLOCKS A CHANGEOVER MUST BE TRACKED. A blocking status that
// nothing polls is a block nothing can clear, and the block outlives the shift.
func IsVendorTracked(s Status) bool {
	return IsVendorActive(s) || s == StatusFaulted
}

// IsVendorTracked is the method form.
func (s Status) IsVendorTracked() bool { return IsVendorTracked(s) }

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
//
// QUEUED IS IN THIS SET AND NOT IN IsStuckSweepCandidate, and the split is the
// whole point.
//
// Being flagged here means "a person should look at this". Being in the sweep
// set means "cancel it on a timer". A queued order waiting on material must get
// the first and must not get the second: demand does not evaporate, so
// cancelling it would delete the ask while the need is still real — and, since
// c3abe1dc, would also hand the window straight back to a replenishment that
// would recreate it. The config comment on AbandonStuck states the same rule
// from the sweep's side.
//
// It was in NEITHER set until 2026-08-03, which made `queued` the least
// observable state in the system — no sweep, no anomaly, nothing. Springfield
// accumulated 290 duplicate orders at one window over three and a half hours
// and raised nothing, and the pile was found by a person reading the board.
// `sourcing` was already here; `queued` is its other half in IsAcquiring, and
// its absence was an omission rather than a decision — the exclusion list above
// never named it.
//
// This matters MORE now that duplicates are bounded, not less. A demand that
// cannot be filled used to announce itself by piling up; now it is one quiet
// row holding a window indefinitely, and this predicate is the only thing that
// says so.
func IsRuntimeStuckCandidate(s Status) bool {
	switch s {
	case StatusPending, StatusQueued, StatusSourcing, StatusSubmitted,
		StatusAcknowledged, StatusDispatched, StatusInTransit, StatusStaged:
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
// orders 1249/1251 held bins bound for the indexed_over positions PLN_02/PLN_05
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
	vendorTrackedStatusSQLList         = buildStatusSQLList(IsVendorTracked)
	preDispatchStatusSQLList           = buildStatusSQLList(IsPreDispatch)
	acquiringStatusSQLList             = buildStatusSQLList(IsAcquiring)
	runtimeStuckCandidateStatusSQLList = buildStatusSQLList(IsRuntimeStuckCandidate)
	stuckSweepStatusSQLList            = buildStatusSQLList(IsStuckSweepCandidate)
	operatorVisibleStatusSQLList       = buildStatusSQLList(IsOperatorVisible)
)

// StatusSQLList is buildStatusSQLList for callers OUTSIDE this package that
// need a population the named projectors above do not already spell.
//
// The named projectors stay the way to ask for a predicate this package owns —
// they are precomputed, and predicateProjectorPairs makes adding one a
// deliberate act. This is for a consumer whose population is its own: soakstat's
// stall checker partitions the non-terminal statuses into three progress kinds
// with different thresholds, and that split belongs to the checker, not here.
//
// Exported so such a consumer derives its set from the ENUM rather than typing
// the values out. Hand-listed populations are how `pending` and `sourcing` came
// to fall through all three of the stall checker's kinds — watched by nothing,
// in the exact statuses where a held leg waits.
//
// Returns "" when nothing matches. A caller splicing this into `status IN (%s)`
// must handle that: `IN ()` is a syntax error, and an empty population is a
// question about nothing rather than a query returning nothing.
func StatusSQLList(pred func(Status) bool) string { return buildStatusSQLList(pred) }

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

// VendorTrackedStatusSQLList returns 'dispatched','faulted','in_transit','staged'
// — the orders the fleet still holds, which the poller must keep watching.
func VendorTrackedStatusSQLList() string { return vendorTrackedStatusSQLList }

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
