package dispatch

// The capability decision: which robots may carry this order.
//
// A heavy payload used to be pinned to one robot group for the whole life of
// its bins, so a 600kg robot was excluded from a bin that was nearly drained
// and well inside its capacity. The plant teams asked for the smaller robots to
// answer empty and near-empty pickups on payloads that only need the big ones
// when full.
//
// SHINGO HAS NO WEIGHT MODEL and this does not add one. bin_types carries
// dimensions and no tare; payloads carries a unit count and no mass. The
// engineer configuring a payload supplies the threshold, because the engineer
// is the one who knows what the part weighs. Nothing here reasons in kilograms.
//
// THE COUNT IS CORE'S, NEVER EDGE'S. bins.uop_remaining only moves on an
// explicit delta, so a cell whose PLC counter is not wired leaves it at the
// value the bin was loaded with — the bin reads FULL and the relaxation
// silently never fires. Edge's RemainingUOPCached fails the other way: no ticks
// leaves it at 0, which reads EMPTY, and that is how a 600 ends up under a full
// bin. Both failures are real at Springfield today. Only one of them is safe,
// and it is the one on this side of the seam.

import (
	"fmt"
	"log"

	"shingocore/store/bins"
	"shingocore/store/orders"
)

// rule names which branch decided a robot group. It exists so the dispatch log
// records the REASON and not just the answer: "strict" is the verdict of four
// different branches, and a wrong-robot report that cannot tell them apart
// cannot be chased. Stringer rather than a bare int so renumbering the
// constants cannot silently change what an old log line meant.
type rule int

const (
	// ruleNoPayload: a bare carrier. ClearForReuse blanks payload_code, so an
	// emptied bin has no template to read and no count worth reading. It is
	// empty by definition, and the only thing that can hold it back is its
	// carrier type — which is exactly what an empty FG rack needs.
	ruleNoPayload rule = iota + 1
	// ruleEmpty: a payload-bearing bin the count says is drained.
	ruleEmpty
	// ruleNegativeCount: the count went below zero, which is not a drain
	// signal. See the comment on decideRobotGroup.
	ruleNegativeCount
	// ruleNoCapacity: the payload template carries no capacity, so there is no
	// denominator and no fraction to compare. A config gap, not a bin state.
	ruleNoCapacity
	// ruleNearEmpty: at or below the configured fraction of capacity.
	ruleNearEmpty
	// ruleAboveThreshold: everything else. NOT "full" — a bin at 80% is here
	// too, and naming it full would invite someone to read the log as a
	// statement about the bin rather than about the threshold.
	ruleAboveThreshold
)

func (r rule) String() string {
	switch r {
	case ruleNoPayload:
		return "no_payload"
	case ruleEmpty:
		return "empty"
	case ruleNegativeCount:
		return "negative_count"
	case ruleNoCapacity:
		return "no_capacity"
	case ruleNearEmpty:
		return "near_empty"
	case ruleAboveThreshold:
		return "above_threshold"
	default:
		return fmt.Sprintf("rule(%d)", int(r))
	}
}

// groupFacts is every input to the capability decision.
//
// Plain data, gathered by the caller, so decideRobotGroup is a pure function
// and its whole test surface is a table of struct literals — no database, no
// fake, no Dispatcher. Every field arrives on a single joined bin read
// (store/bins.BinJoinQuery already joins bin_types and payloads), so assembling
// this costs nothing beyond the read dispatch was already doing.
//
// THE ZERO VALUE MUST DECIDE WHAT THE OLD CODE DECIDED: all-blank facts return
// the payload's own group, which is "" — the vendor default. That is the
// unconfigured plant, and it has to stay byte-identical.
type groupFacts struct {
	requiredGroup  string // bin_types.required_robot_group — a refusal, see below
	payloadCode    string // bins.payload_code
	remaining      int    // bins.uop_remaining
	capacity       int    // payloads.uop_capacity
	payloadGroup   string // payloads.robot_group — the group for a loaded bin
	nearEmptyGroup string // payloads.near_empty_robot_group
	allowNearEmpty bool   // payloads.near_empty_enabled
	thresholdPct   int    // payloads.near_empty_threshold_pct
}

// decideRobotGroup answers which SEER robot group may carry this bin, and which
// branch said so.
//
// ── THE CARRIER GROUP IS A REFUSAL, NOT AN OVERRIDE ──────────────────────────
//
// bin_types.required_robot_group replaces the RELAXED outcome only. It does not
// apply to a loaded bin, and that asymmetry is the whole point: a carrier can
// make the decision heavier and never lighter. An empty FG rack goes to the
// group its carrier names no matter what its payload permits; a full one goes
// to the payload's group, which already accounts for the rack's own weight
// because whoever set it was configuring a loaded move.
//
// Applying it at every fill level instead — the first draft of this — is wrong
// in the direction that matters: a carrier misconfigured to a lighter group
// would then take a FULL heavy load, which is the exact failure this feature
// exists to prevent.
//
// It is also why "cap" is the wrong word for it. Robot groups are opaque
// strings; nothing can know whether Group-002 contains Group-001, so "never
// relaxed below it" would have no testable meaning. "Replaces the relaxed
// outcome" does.
//
// ── WHY A NEGATIVE COUNT IS NOT A DRAIN SIGNAL ───────────────────────────────
//
// Not a probability argument: a negative count cannot bound the remaining
// fraction in EITHER direction, so it says nothing about how full the bin is.
// Every measured mechanism that produces one — overpack, a stale binding across
// a bin swap, epoch-boundary coalescing, duplicate process_nodes counting PLC
// ticks three times — leaves the bin heavier than it reads.
//
// The guard is also load-bearing rather than cautious. Without it the
// near-empty test relaxes on every negative, because -1497 of 2160 is -69%,
// which is at or below any threshold anyone would set. Five of sixteen carriers
// at Hopkinsville were sitting negative when this was written; they all exit at
// ruleNoPayload, which is why that branch is ordered first.
func decideRobotGroup(f groupFacts) (string, rule) {
	// The relaxed outcome, decided once so every branch below agrees on it.
	//
	// A blank nearEmptyGroup under allowNearEmpty is LEGITIMATE and means the
	// vendor-default pool — any robot. That is frequently what a plant wants,
	// and it is why near_empty_enabled is a real column instead of being
	// inferred from the group being non-empty: the flag is the only thing that
	// can tell "relax to anybody" apart from "not configured".
	relaxed := f.payloadGroup
	if f.allowNearEmpty {
		relaxed = f.nearEmptyGroup
	}
	if f.requiredGroup != "" {
		relaxed = f.requiredGroup
	}

	switch {
	case f.payloadCode == "":
		return relaxed, ruleNoPayload
	case f.remaining == 0:
		return relaxed, ruleEmpty
	case f.remaining < 0:
		return f.payloadGroup, ruleNegativeCount
	case f.capacity <= 0:
		return f.payloadGroup, ruleNoCapacity
	// CROSS-MULTIPLIED, NEVER DIVIDED. remaining/capacity in integers truncates
	// to 0 for every partially-drained bin (1000/2160 is 0), and 0 <= any
	// threshold, so the naive form relaxes the whole plant. In floats it
	// misreads the boundary operators will actually set. This form is exact,
	// cannot divide by zero, and the largest product in play is about 12000*100.
	case f.remaining*100 <= f.capacity*f.thresholdPct:
		return relaxed, ruleNearEmpty
	default:
		return f.payloadGroup, ruleAboveThreshold
	}
}

// robotGroupForOrder resolves the SEER robot-dispatch group for an order and
// logs which rule decided it.
//
// ONE SITE, BECAUSE THERE ARE THREE CALLERS AND THEY USED TO DISAGREE ABOUT
// WHAT THEY LOGGED. The three fleet.CreateOrderRequest builders — plain
// (dispatcher.go), complex (complex_dispatch.go) and gated
// (lane_gate_dispatch.go, which serves the gated form of both) — each resolved
// the group for themselves and only the plain one recorded it. A wrong-robot
// report on a complex or gated leg was unreconstructable, which is the one
// thing a capability decision must never be. The log line lives in here so a
// fourth caller cannot be added without it.
//
// ── THE TWO DEGRADATIONS POINT OPPOSITE WAYS, ON PURPOSE ─────────────────────
//
// Resolving the payload has always degraded to "" — the vendor default, any
// robot — because a config read must never stall material flow. That stays.
//
// The COUNT degrades the other way: if the bin cannot be read, the payload's
// own group is used and the relaxation never fires. Relaxing on a failed read
// is the one outcome that cannot be allowed, because the failure is silent and
// the consequence is a 600 under a load it cannot lift.
//
// So an unreadable bin lands on the payload's group, and an unresolvable
// payload lands on "" exactly as it did before this feature existed. They are
// different failures with different answers, and the next reader is going to
// want to "fix" one of them to match the other. Do not.
func (d *Dispatcher) robotGroupForOrder(order *orders.Order) string {
	bin := d.capabilityBin(order)
	if bin == nil {
		// No bin to read. Fall back to the payload the ORDER names, which is
		// the pre-feature behaviour verbatim, and say bin=none so the verdict
		// is not mistaken for a rule that actually looked at a count.
		group := d.robotGroupForPayload(order.PayloadCode)
		d.dbg("robot group: order=%d bin=none payload=%q group=%q (no bin resolved; payload group only)",
			order.ID, order.PayloadCode, group)
		return group
	}

	facts := groupFacts{
		requiredGroup:  bin.CarrierRequiredRobotGroup,
		payloadCode:    bin.PayloadCode,
		remaining:      bin.UOPRemaining,
		capacity:       bin.UOPCapacity,
		payloadGroup:   bin.PayloadRobotGroup,
		nearEmptyGroup: bin.PayloadNearEmptyGroup,
		allowNearEmpty: bin.PayloadNearEmptyEnabled,
		thresholdPct:   bin.PayloadNearEmptyPct,
	}

	// AN EMPTY-INTENT FETCH IS CARRYING NOTHING, whatever tag the row still
	// holds. The order says the bin is wanted AS an empty carrier, and a bin
	// picked as an empty can still carry a stale payload_code — the manifest is
	// cleared when the bin is released, not when somebody decides to come and
	// get it. Reading that leftover tag would ask for the robot group of the
	// part the carrier used to hold.
	//
	// It is the same carve-out payloadForDispatch has always made for this
	// intent, and it has to be made here too now that the group is resolved from
	// the bin rather than from the order. Blanking the code drops it to
	// ruleNoPayload, which is what an empty carrier is.
	//
	// The CARRIER's own requirement is deliberately left standing: a bare FG
	// rack is exactly the case that must not be relaxed, and it is the reason
	// bin_types.required_robot_group exists.
	if order.SourceIntent == SourceIntentEmpty {
		facts.payloadCode = ""
		facts.payloadGroup, facts.nearEmptyGroup = "", ""
		facts.allowNearEmpty, facts.thresholdPct = false, 0
	}
	group, r := decideRobotGroup(facts)

	d.dbg("robot group: order=%d bin=%s payload=%q remaining=%d/%d pct=%d rule=%s group=%q",
		order.ID, bin.Label, bin.PayloadCode, bin.UOPRemaining, bin.UOPCapacity,
		nearEmptyPct(bin.UOPRemaining, bin.UOPCapacity), r, group)

	// A payload-bearing bin reading below zero is a data defect, not a bin
	// state. Dispatch is the only place positioned to notice it, so it says so
	// once, here, rather than leaving the count to be quietly distrusted.
	if r == ruleNegativeCount {
		log.Printf("dispatch: bin %s carries %s but reads %d units — negative count, robot group not relaxed",
			bin.Label, bin.PayloadCode, bin.UOPRemaining)
	}
	return group
}

// capabilityBin returns the bin whose contents decide this order's robot group,
// or nil when none can be resolved.
//
// Two ways in, and the second is the one that matters for a swap. An order that
// already holds its bin is re-read fresh, because the scanner writes bin_id
// moments before dispatch is called and the copy in hand is routinely stale.
//
// An order with no bin_id is the coordinated-pair evac leg: it dispatches as
// wait(LINE) and its pickup is appended at release, so at the moment the fleet
// order is created there is nothing claimed. findFallbackBinAtSource is the
// existing answer to "which bin does this order mean" — claim-first, then the
// bin at the line node, already preferring ProcessNode when several are
// claimed. Writing a second node lookup here would be a second derivation of
// the same fact, and it would pick arbitrarily when two bins sit at the line.
func (d *Dispatcher) capabilityBin(order *orders.Order) *bins.Bin {
	binID := order.BinID
	if fresh, err := d.db.GetOrder(order.ID); err == nil && fresh != nil && fresh.BinID != nil {
		binID = fresh.BinID
	}
	if binID != nil {
		bin, err := d.db.GetBin(*binID)
		if err == nil && bin != nil {
			return bin
		}
		return nil
	}
	if bin, ok := d.findFallbackBinAtSource(order); ok {
		return bin
	}
	return nil
}

// nearEmptyPct is the fill percentage for the dispatch log. Reported separately
// from the decision so the log can show the number a human would check the
// threshold against, without the decision ever dividing. -1 when there is no
// denominator to divide by.
func nearEmptyPct(remaining, capacity int) int {
	if capacity <= 0 {
		return -1
	}
	return remaining * 100 / capacity
}
