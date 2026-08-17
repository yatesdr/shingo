package service

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/shared/clock"
	"shingocore/store"
)

// THE BURIAL SHADOW — what a placement buried, recorded at the moment it lands.
//
// A "burial" is one placement landing SHALLOWER in a lane than a bin some live
// order still has a hold on. It reads, logs and counts; it never refuses.
//
// ── TWO JOBS, AND THEY ARE NOT THE SAME JOB ───────────────────────────────
//
// The burial guard (store/nodes/lanes.go findStoreSlot) refuses a store slot in
// front of a HARD-claimed bin, and does not look at soft holds. So this
// instrument's two halves now mean opposite things:
//
//	SOFT holds  → DATA. Burying a soft hold is intended: a soft hold is a plan,
//	              and the held-bin path re-resolves a buried plan into a dig.
//	              Counting them sizes how often that recalculation is actually
//	              being paid for, which is the input to any later protection for
//	              waiting retrieves. Collect from deploy day, decide afterwards.
//
//	HARD claims → TRIPWIRE. The guard makes this impossible through the store
//	              selector, so an event means a placement reached a lane WITHOUT
//	              consulting it. Expected value zero. One path is legitimately
//	              uncovered — see noteHardBurial — and is counted apart so it can
//	              never make the tripwire look dirty.
//
// ── WHY ARRIVAL, POST-COMMIT ──────────────────────────────────────────────
//
// The seam has to be a committed placement, once per order, never a candidate
// scan — the group resolver calls the selector while SCANNING lanes, so counting
// there would multiply by the width of the scan. Arrival wins over the slot
// confirm at dispatch on four counts:
//
//  1. It is the only moment a burial is a FACT. A confirm-time count records an
//     INTENDED burial, and an order cancelled — or a holder that dispatches and
//     pulls its bin out — in that window buried nothing.
//  2. The population is already exactly right. ApplyArrival / ApplyMultiBinArrival
//     are the ORDER-DRIVEN arrival funnels; an operator's manual Move
//     deliberately bypasses them. Hooking EventBinUpdated{moved} instead would
//     have swept manual moves in, and the event carries no field separating them.
//  3. Once per placement, with no reasoning about retries: the confirm path is
//     owner-idempotent and re-run every dispatch tick, and a gate re-bind confirms
//     a second slot for one order. A bin lands once.
//  4. "It cannot refuse" becomes structural. Every caller invokes this AFTER the
//     arrival transaction commits and discards what it returns, so there is
//     nothing for a caller to branch on.
//
// The cost is one lane lookup per arrival; only an arrival into a LANE slot pays
// for more.
//
// ── THE LOG LINE IS THE DATASET ───────────────────────────────────────────
//
// One line per buried bin, prefixed burialShadowTag so a soak greps out of the
// journal in one command. The counters are a reading aid: in-process, reset on
// restart, and nothing should be concluded from them that the lines do not say.
//
// Each line names holder_order, deliberately. held_for is the hold's age AT
// BURIAL, which is a LOWER BOUND on what protecting that class would have cost —
// a refusal would have lasted until the hold ended, not until the burial. The
// full interval is recoverable afterwards by joining holder_order to
// order_history, which is durable and already timestamped, so naming the order
// buys the exact number with no sampler, no second hook and no new table.
const burialShadowTag = "burial-shadow"

// The two hard-burial markers, exported because the reconciliation tally line and
// its drift test both have to know what they are — and must NOT contain them.
//
// A should-be-zero tally that quotes its own search string is counted by that
// search, so the number read back is tally-lines-plus-events and the counter
// reads non-zero forever (PLAN §R.9). Naming them here means the emitter, the
// summariser and the guard test share one definition instead of three string
// literals that can drift apart — which is the mistake underneath B1.
const (
	// BurialBypassMarker prefixes the SHOULD-BE-ZERO line: the claim already
	// existed when the selector was consulted for the placing order, so it was
	// never asked.
	BurialBypassMarker = "GUARD BYPASS"
	// BurialChurnMarker prefixes the accepted population: approved, then
	// invalidated by a claim that arrived between the resolve and the placement
	// (§R.4).
	BurialChurnMarker = "APPROVED-THEN-INVALIDATED"
)

// BurialHoldKind names which hold class was buried.
type BurialHoldKind string

const (
	// BurialHoldPending — a soft (pending) bin reservation and no hard claim: the
	// class the guard deliberately buries, and the dataset.
	BurialHoldPending BurialHoldKind = "pending-hold"
	// BurialHoldHard — bins.claimed_by set by a live non-compound order: a robot
	// en route. The guard refuses these, so a burial is a bypass.
	BurialHoldHard BurialHoldKind = "hard-claim"
	// BurialHoldCompoundLeg — a dig leg's claim, stamped at plan creation. Also a
	// hard claim as far as the guard's clause is concerned (it reads claimed_by),
	// named apart because the holder behaves differently: held from creation, and
	// living as long as the dig.
	BurialHoldCompoundLeg BurialHoldKind = "compound-leg"
)

// BurialTally is the since-boot count. Snapshot type: the getter copies under
// the lock so a reader never sees a torn set.
type BurialTally struct {
	// Soft is the dataset — placements that buried a plan. Expected non-zero.
	Soft int64
	// SoftLongestHeld is the largest hold age at burial among the soft events, the
	// cheap read on how much recalculation those burials are costing.
	SoftLongestHeld time.Duration
	// Bypass is the TRIPWIRE: a hard claim that ALREADY EXISTED when the selector
	// was consulted for the placing order, buried by a placement that therefore
	// should have gone through the guarded selector and seen it. Expected ZERO.
	//
	// IT USED TO KEY ON THE FLEET-COMMIT, which is a later event than the
	// consultation and sometimes a much later one — so a claim that landed in
	// between was accused of a bypass no guard could have prevented. Fixed by
	// recording the resolve (orders.destination_resolved_at); see
	// burialWasApprovedThenInvalidated for the rig specimen that priced it.
	//
	// IT USED TO MEAN SOMETHING WIDER. Until the §R.4 split it counted every hard
	// burial that was not a dig leg, which lumped in the population below — so it
	// read 3-5 every soak and its own sentence ("find the placement path") was
	// false for most of them. A should-be-zero that is never zero for reasons
	// nobody can act on stops being read, which is standing law 9 from the other
	// direction.
	Bypass int64
	// Churn is APPROVED-THEN-INVALIDATED: the claim was created AFTER the selector
	// was consulted for the placing order, so no check at any Core moment could
	// have seen it. Ruled accepted and healed (§R.4) — the cascade dissolves and
	// re-plans at ~2.5 min of re-work. Expected non-zero on a busy plant; it is
	// the measured price of law 6, not a defect.
	Churn int64
	// DigUncovered is the known gap, kept apart from both so it can never make the
	// tripwire look dirty: a dig leg's placement resolves through findShuffleSlots
	// and never consults the selector.
	DigUncovered int64
	LastBuriedAt time.Time
}

// burialShadow holds the since-boot tally. In-process and crash-volatile on
// purpose: it is a reading aid, not a fact. The facts are the log lines, which
// survive a restart because the journal does.
type burialShadow struct {
	mu    sync.Mutex
	tally BurialTally
}

func (b *burialShadow) recordSoft(heldFor time.Duration, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tally.Soft++
	if heldFor > b.tally.SoftLongestHeld {
		b.tally.SoftLongestHeld = heldFor
	}
	b.tally.LastBuriedAt = at
}

// hardBurialKind is which of the three things a hard-claim burial actually is.
// Three, not two: PLAN §R.4 ruled that most of what the tripwire was calling a
// bypass is a different population, and a counter that cannot say which is a
// counter nobody can act on.
type hardBurialKind int

const (
	// hardNeverAsked is the SHOULD-BE-ZERO. The buried claim already existed when
	// the selector was consulted for this order, so it — had it been asked — would
	// have seen the claim and refused.
	hardNeverAsked hardBurialKind = iota
	// hardApprovedThenInvalidated is churn the design accepts. The claim was
	// created AFTER the selector was consulted for this order, so no check at any
	// Core moment could have seen it: the window runs resolve→placement, measured
	// at 27ms and 32s on the two rig specimens. The cascade dissolves and re-plans;
	// cost ~2.5 min of re-work, no wedge (§R.3, §R.4).
	hardApprovedThenInvalidated
	// hardDigUncovered is the known, named gap: a dig leg resolves its shuffle
	// slots through findShuffleSlots and never consults the selector, on purpose.
	hardDigUncovered
)

func (b *burialShadow) recordHard(kind hardBurialKind, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch kind {
	case hardNeverAsked:
		b.tally.Bypass++
	case hardApprovedThenInvalidated:
		b.tally.Churn++
	case hardDigUncovered:
		b.tally.DigUncovered++
	}
	b.tally.LastBuriedAt = at
}

func (b *burialShadow) snapshot() BurialTally {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.tally
}

// BurialShadowTally returns the since-boot counts. Read once per reconciliation
// sweep so a soak reads off one line.
func (s *BinService) BurialShadowTally() BurialTally { return s.burials.snapshot() }

// NoteBurialShadow records every live-held bin that `binID` buried by landing at
// `toNodeID`. placedBy is the order whose placement it was, used only to tell a
// guarded path from the one uncovered path.
//
// ── IT RETURNS NOTHING, AND THAT IS THE POINT ─────────────────────────────
//
// No error, no verdict, no bool. The placement has already committed by the time
// this runs, so an instrument that could be branched on would be a guard wearing
// a counter's name. Every failure inside is logged and swallowed for the same
// reason: a measurement that can break an arrival is worse than no measurement.
func (s *BinService) NoteBurialShadow(binID, toNodeID, placedBy int64) {
	lane, err := s.db.LaneForNode(toNodeID)
	if err != nil {
		log.Printf("%s: resolve lane for node %d: %v (not counted)", burialShadowTag, toNodeID, err)
		return
	}
	if lane == nil {
		return // not a lane slot — nothing can be behind it
	}
	buried, err := s.db.SpokenForBinsBehind(toNodeID)
	if err != nil {
		log.Printf("%s: read spoken-for bins in lane %s: %v (not counted)", burialShadowTag, lane.Name, err)
		return
	}
	if len(buried) == 0 {
		return
	}

	// Resolved once, and only when there is something to classify. An unreadable
	// placer counts as a GUARDED placement, which is the loud direction: a
	// tripwire that under-reports is worse than one that over-reports.
	digPlacement := false
	if placedBy != 0 {
		if isLeg, dErr := s.db.OrderIsCompoundLeg(placedBy); dErr == nil {
			digPlacement = isLeg
		} else {
			log.Printf("%s: placer %d shape unknown: %v (counting as a guarded placement)",
				burialShadowTag, placedBy, dErr)
		}
	}
	slotName := ""
	if slot, sErr := s.db.GetNode(toNodeID); sErr == nil && slot != nil {
		slotName = slot.Name
	}

	now := clock.Now().UTC()
	at := burialSite{lane: lane.Name, slot: slotName, placedBin: binID, placedBy: placedBy}
	for _, b := range buried {
		// THE AGE COMES FROM THE DATABASE, NOT FROM A SUBTRACTION HERE.
		//
		// This read `now.Sub(b.HeldSince.UTC())` with `now = clock.Now()`, and
		// HeldSince is a DB-default `created_at`. Two clocks, one subtraction: under
		// the honest running clock sim runs a year ahead of wall and an eight-second
		// hold printed as `held_for=7355h32m48s` — in the single line an engineer
		// reads to judge whether a burial mattered. The old negative-clamp was the
		// same defect wearing a guard; it caught the sign and not the magnitude.
		heldFor := b.HeldFor
		if b.HardClaim {
			s.noteHardBurial(at, b, heldFor, digPlacement, now)
			continue
		}
		s.noteSoftBurial(at, b, heldFor, now)
	}
}

// burialSite is the "where" half of an event, so the two recorders take one
// argument instead of four and cannot get them out of order.
type burialSite struct {
	lane      string
	slot      string
	placedBin int64
	placedBy  int64
}

// noteSoftBurial records a placement that buried a PLAN — the intended case, and
// the dataset.
//
// One line per event, field order = reading order: where, what got buried, who
// holds it, how long they had held it. Deliberately not a warning: the design
// says these happen, and the held-bin path turns them into digs.
func (s *BinService) noteSoftBurial(at burialSite, b store.SpokenForBin, heldFor time.Duration, now time.Time) {
	log.Printf("%s: lane=%s slot=%s placed_bin=%d placed_by=%d buried_bin=%d (%s) buried_slot=%s buried_depth=%d "+
		"hold=%s holder_order=%d holder_status=%s held_for=%s",
		burialShadowTag, at.lane, at.slot, at.placedBin, at.placedBy,
		b.BinID, b.BinLabel, b.SlotName, b.Depth,
		BurialHoldPending, b.HolderID, b.HolderStatus, heldFor.Round(time.Second))
	s.burials.recordSoft(heldFor, now)
}

// noteHardBurial is THE TRIPWIRE. A hard claim means a robot is en route to that
// bin, and the burial guard refuses exactly this through the store-slot selector
// — so reaching here means the placement did not come through it.
//
// ── THE ONE LEGITIMATE UNCOVERED PATH ─────────────────────────────────────
//
// A reshuffle resolves its shuffle slots through findShuffleSlots
// (dispatch/reshuffle.go), which carries its own candidate predicate and never
// calls the selector. That is deliberate — a guard able to refuse a dig would
// refuse the moves that exist to unbury things — so a dig leg burying a claimed
// bin in some OTHER lane is a known gap rather than a defect.
//
// Counted apart, not suppressed. Suppressing would hide the gap exactly when it
// starts to matter, and the two numbers answer different questions: Bypass says
// the guard has a hole, DigUncovered says how much extending it to dig planning
// would buy.
func (s *BinService) noteHardBurial(at burialSite, b store.SpokenForBin, heldFor time.Duration, digPlacement bool, now time.Time) {
	kind := BurialHoldHard
	if b.HolderIsChild {
		kind = BurialHoldCompoundLeg
	}
	where := fmt.Sprintf("lane=%s slot=%s placed_bin=%d placed_by=%d buried_bin=%d (%s) "+
		"buried_slot=%s buried_depth=%d hold=%s holder_order=%d holder_status=%s held_for=%s",
		at.lane, at.slot, at.placedBin, at.placedBy,
		b.BinID, b.BinLabel, b.SlotName, b.Depth,
		kind, b.HolderID, b.HolderStatus, heldFor.Round(time.Second))

	if digPlacement {
		log.Printf("%s: DIG-UNCOVERED (a dig leg, resolved through findShuffleSlots) %s",
			burialShadowTag, where)
		s.burials.recordHard(hardDigUncovered, now)
		return
	}

	if s.burialWasApprovedThenInvalidated(at.placedBy, b.HeldSince) {
		log.Printf("%s: %s — order %d's destination was approved and a later claim invalidated it. "+
			"Order %d claimed the buried bin at %s, AFTER the selector was consulted for order %d, so "+
			"no check at any Core moment could have seen it — the window runs from the resolve to the "+
			"placement (27ms and 32s on the two rig specimens; longer for an order that queued behind "+
			"capacity before dispatching). Accepted and healed: the cascade dissolves and re-plans, "+
			"~2.5 min of re-work. This is the measured price of law 6, not a defect. %s",
			burialShadowTag, BurialChurnMarker, at.placedBy,
			b.HolderID, b.HeldSince.UTC().Format(time.RFC3339), at.placedBy, where)
		s.burials.recordHard(hardApprovedThenInvalidated, now)
		return
	}

	log.Printf("%s: %s — a placement buried a bin a robot is en route to, without going through the "+
		"store-slot selector. The claim was ALREADY HELD when the selector was consulted for order %d, "+
		"so it would have seen it and refused. Expected count is ZERO: find the placement path and "+
		"route it through nodes.FindStoreSlotInLaneExcluding. %s",
		burialShadowTag, BurialBypassMarker, at.placedBy, where)
	s.burials.recordHard(hardNeverAsked, now)
}

// burialWasApprovedThenInvalidated answers PLAN §R.4's question: did this claim
// exist AT THE MOMENT THE SELECTOR LOOKED?
//
// If it did NOT, the selector could not have seen it however diligently it was
// consulted, and the burial is churn the design accepts. If it DID, the selector
// would have refused — so reaching a burial means it was never asked.
//
// ── THE MOMENT THE SELECTOR LOOKED IS NOT THE MOMENT IT DISPATCHED ────────
//
// This compared against the fleet-commit time alone, and that is the defect the
// column below was added to close. Choosing a destination and committing to the
// fleet are different events: at intake the selector runs BEFORE the order row
// exists, and an order whose group is full queues behind capacity and commits
// minutes later. Every claim landing in that gap was reported as a GUARD BYPASS.
//
// It was not theoretical. Lane-stress rig, 2026-08-15: order 53 resolved onto
// LSC_032 at 03:46:54.344, committed at 03:46:54.385, and was accused of
// ignoring a claim held by order 54 — an order that did not exist until
// 03:46:54.475. The instrument's only should-be-zero read 1, for a race that no
// guard anywhere could have won. An engineer (and then a reviewer) spent real
// time chasing it before the log timestamps gave it up.
//
// So the question is asked of the RESOLVE, and only falls back to the commit for
// orders that have no resolve stamp — those whose destination was named
// concretely by the sender, or chosen at dispatch by planMove, where commit is
// the right moment anyway.
//
// FAIL-LOUD ON A DOUBT, UNCHANGED. An unreadable placer, an unreadable stamp, a
// missing history row, or a zero claim time all return false, which sends the
// event to the should-be-zero bucket. A tripwire that under-reports is worse
// than one that over-reports. What changed is only that a KNOWN-ANSWERABLE case
// stopped being counted as a doubt.
func (s *BinService) burialWasApprovedThenInvalidated(placedBy int64, heldSince time.Time) bool {
	if placedBy == 0 || heldSince.IsZero() {
		return false
	}
	askedAt, ok, err := s.selectorLookedAt(placedBy)
	if err != nil || !ok {
		return false
	}
	return heldSince.UTC().After(askedAt)
}

// selectorLookedAt reports when the store-slot guard was consulted for this
// order, preferring the recorded resolve and falling back to the fleet-commit.
//
// TWO SOURCES, ONE QUESTION (law 3): callers get a single instant and never have
// to know which of the two answered. ok=false means no moment could be
// established and the caller must take the loud arm; an error is returned
// alongside it purely so the log line can say which read failed.
func (s *BinService) selectorLookedAt(placedBy int64) (time.Time, bool, error) {
	resolvedAt, ok, err := s.db.OrderDestinationResolvedAt(placedBy)
	if err != nil {
		log.Printf("%s: placer %d destination-resolve time unreadable: %v "+
			"(counting as a never-asked bypass)", burialShadowTag, placedBy, err)
		return time.Time{}, false, err
	}
	if ok {
		return resolvedAt, true, nil
	}
	// No intake stamp. The destination was named by the sender or chosen at
	// dispatch, and for both the commit is the moment the selector last had a say.
	committedAt, ok, err := s.db.OrderCommittedToFleetAt(placedBy)
	if err != nil {
		log.Printf("%s: placer %d commit time unknown: %v (counting as a never-asked bypass)",
			burialShadowTag, placedBy, err)
		return time.Time{}, false, err
	}
	return committedAt, ok, nil
}
