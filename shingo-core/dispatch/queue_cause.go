package dispatch

// QueueCause is the engineer-facing call-site tag that lands on
// orders.queue_cause — WHERE in the pipeline a wait arose, as opposed to
// protocol.QueueCode, which is WHY in words an operator reads.
//
// It is a type rather than a string so the set is enumerable and a typo is a
// compile error. Both matter more than they look: these values are what an
// engineer groups by when a queue code trends wrong, so a value that exists in
// one spelling at one site and another spelling at another is a forensic record
// that quietly under-counts.
//
// NEVER CROSSES THE WIRE. domain.Order documents it as Core-side only, and
// nothing in Edge, the UI, or a template reads it — checked before this type
// existed. So the underlying strings are free to change; they are a Core
// vocabulary, not a contract. They are NOT changing here.
//
// ── SCOPE: THE LANE/GATE FAMILY ONLY ──────────────────────────────────────
//
// The full queue_cause surface is about 28 values across dispatch/ and
// fulfillment/, including computed ones and three pass-throughs from other
// subsystems. Only the lane and gate family is named here, deliberately: this
// type exists to make the admission convergence's diff readable, and a version
// that renamed 28 values across two modules would bury the change it is meant
// to clarify.
//
// The rest are not forgotten — they are VISIBLE. setQueueReason takes this
// type, so every un-named cause now appears at its call site as an explicit
// QueueCause("...") conversion. That conversion IS the to-do list: grep for it
// and the remaining work enumerates itself, which a comment promising a
// follow-up would not.
//
// ── ONE VALUE THAT IS DELIBERATELY NOT UNIFIED ────────────────────────────
//
// CauseLaneLockRace is "lock-race", and fulfillment/scanner.go sets the same
// literal for a BIN reservation race. Two different facts wearing one string,
// and typing the lane one does not merge them — the scanner's stays untyped and
// keeps its value. Collapsing them would change what the forensic record means
// for one of the two, which is a behaviour change and not this brief's. Naming
// the lane one is what makes the collision visible at all.
type QueueCause string

// The lane/gate family. Values are unchanged from the literals they replace —
// this commit renames nothing, it only makes the names exist.
const (
	// ── Ordering: the move is safe, it is not this order's turn ───────────
	// Set by the tiered-entry classifier (lane_entry.go). These two answer
	// "which of the waiting ones goes first", NOT "may this move happen" —
	// the distinction admission is built around, kept visible here because
	// the two families otherwise look alike in a queue_cause histogram.

	// CauseLaneDeeperPending — Tier 2: a deeper cross-origin store has not placed yet.
	CauseLaneDeeperPending QueueCause = "lane-deeper-pending"
	// CauseLaneGroupActive — Tier 3: an active cross-origin group holds the lane.
	CauseLaneGroupActive QueueCause = "lane-group-active"

	// ── Admission: the lane cannot take this move now ─────────────────────

	// CauseLaneDigActive — someone else's dig holds the lane (gate retrieve classifier).
	CauseLaneDigActive QueueCause = "lane-dig-active"
	// CauseLaneTargetBuried — a shallower bin still sits in front of the wanted slot.
	CauseLaneTargetBuried QueueCause = "lane-target-buried"
	// CauseLaneHeldDig — the mouth acquire lost to a dig holder.
	CauseLaneHeldDig QueueCause = "lane-held-dig"
	// CauseLaneHeldTraffic — the mouth acquire lost to a different-mode holder.
	// DEFINITE: every one of the order's lanes was read and none held a dig.
	CauseLaneHeldTraffic QueueCause = "lane-held-traffic"
	// CauseLaneHeldUnreadable — the mouth acquire was refused and at least one of
	// the order's lanes could not be read, so a dig cannot be ruled out. It is
	// NOT a third kind of contention; it is the absence of an answer, and it
	// exists so that absence stops being reported as lane-held-traffic.
	CauseLaneHeldUnreadable QueueCause = "lane-held-unreadable"
	// CauseLaneOccupied — a robot from another order is inside the lane (Hold B).
	// Distinct from CauseLaneHeldDig: a dig CLAIMS a lane for a whole reshuffle,
	// occupancy says somebody is physically in it right now, and the two clear on
	// different signals.
	CauseLaneOccupied QueueCause = "lane-occupied"
	// CauseLaneLocked — the reshuffle planner found the lane already dug by another.
	CauseLaneLocked QueueCause = "lane-locked"
	// CauseLaneLockRace — TryLock lost the race for the lane's dig lock. See the
	// type doc: fulfillment sets this same string for an unrelated BIN race.
	CauseLaneLockRace QueueCause = "lock-race"
	// CauseIntakeBuried — the target was buried at intake; the arms after it
	// narrow to a more specific cause when they can.
	CauseIntakeBuried QueueCause = "intake-buried"
	// CauseReshuffleCongestion — the blanket tag the two scanner arms wrote for
	// every transient reshuffle refusal. Kept as the fallback for a wait whose
	// specific cause is not mapped, never as a first choice.
	CauseReshuffleCongestion QueueCause = "reshuffle-congestion"
	// CauseNoShuffleSlot — the dig had nowhere to park its blockers (findShuffleSlots
	// came up short). Clears when any order releases a slot anywhere in the group.
	CauseNoShuffleSlot QueueCause = "no-shuffle-slot"
	// CauseDigBlockerClaimed — a bin the dig must move is hard-claimed by an order
	// outside the compound, most often a dispatched retrieve whose robot is already
	// carrying it out of the lane. Distinct from CauseLaneLocked (a whole lane
	// reserved for someone else's dig) and from CauseNoShuffleSlot (nowhere to put
	// the blockers): here the lane is diggable and the parking is there, one
	// specific bin is spoken for. It clears when that order finishes, which is why
	// it must not be filed under lock-race — the wait is a robot's drive time, not
	// a lost microsecond.
	CauseDigBlockerClaimed QueueCause = "dig-blocker-claimed"

	// ── The gate's own failures ───────────────────────────────────────────

	// CauseGateRebindUnavailable — a gate-staged order's bin has no slot to rebind to.
	CauseGateRebindUnavailable QueueCause = "gate-rebind-unavailable"
	// CauseGateAppendFailed — the fleet refused the tail append past the retry threshold.
	CauseGateAppendFailed QueueCause = "gate-append-failed"

	// ── Undetermined: a read failed, so the answer is not known ───────────
	// These are the fail-closed arms, and they are their own group on purpose.
	// A wait tagged with one of these is NOT a routine wait — it is Core
	// declining to answer, and an engineer reading a histogram needs to be able
	// to tell those apart from a lane that was honestly busy.

	// CauseLaneAcquireError — the mouth acquire could not be read (fulfillment).
	CauseLaneAcquireError QueueCause = "lane-acquire-error"
	// CauseLaneEntryError — the tiered-entry check could not be read (fulfillment).
	CauseLaneEntryError QueueCause = "lane-entry-error"
	// CauseAdmissionError — a physical question could not be read, so admission
	// declined to answer and the caller held the move. The arm that had no cause
	// at all: a compound leg held on an unreadable lane wrote nothing to its row,
	// so it was indistinguishable from a leg nobody had looked at yet — the same
	// defect the refusal arm beside it had already been given a cause for.
	CauseAdmissionError QueueCause = "admission-error"
	// CauseReadFailed — a read Core needed to make a decision did not answer, so
	// the order is holding until it does. NOT lane-busy, and the separation is
	// the point: during a database outage dozens of orders park at once, and an
	// operator surface that renders that as congestion sends someone to look at
	// lanes. This says "the system could not read", which is a different
	// investigation and a different fix.
	CauseReadFailed QueueCause = "read-failed"
	// CauseLoaderSourceUnreadable — the dedicated-loader pool could not be read
	// (the source node, the loader home, the loader, its members, or the bins on
	// them). Kept out of finder-pool-empty on purpose: an empty pool is a fact
	// about the plant that an operator can act on, this is a failed read that
	// says nothing about whether material is there.
	CauseLoaderSourceUnreadable QueueCause = "loader-source-unreadable"
)
