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
	// existed when the placing order was committed, so the selector was not asked.
	BurialBypassMarker = "GUARD BYPASS"
	// BurialChurnMarker prefixes the accepted population: approved, then
	// invalidated by a claim that arrived during the mouth-to-slot drive (§R.4).
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
	// Bypass is the TRIPWIRE: a hard claim that ALREADY EXISTED when the placing
	// order was committed, buried by a placement that therefore should have gone
	// through the guarded selector and seen it. Expected ZERO.
	//
	// IT USED TO MEAN SOMETHING WIDER. Until the §R.4 split it counted every hard
	// burial that was not a dig leg, which lumped in the population below — so it
	// read 3-5 every soak and its own sentence ("find the placement path") was
	// false for most of them. A should-be-zero that is never zero for reasons
	// nobody can act on stops being read, which is standing law 9 from the other
	// direction.
	Bypass int64
	// Churn is APPROVED-THEN-INVALIDATED: the claim was created AFTER the placing
	// order was committed and driving, so no check at any Core moment could have
	// seen it. Ruled accepted and healed (§R.4) — the cascade dissolves and
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
	// this order's destination was committed, so the selector — had it been
	// consulted — would have seen it and refused.
	hardNeverAsked hardBurialKind = iota
	// hardApprovedThenInvalidated is churn the design accepts. The claim was
	// created AFTER this order was committed and driving, so no check at any Core
	// moment could have seen it: the window is the mouth→slot drive, measured at
	// 27ms and 32s on the two rig specimens. The cascade dissolves and re-plans;
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
		heldFor := now.Sub(b.HeldSince.UTC())
		if heldFor < 0 {
			heldFor = 0 // clock skew between the DB default and the injectable clock
		}
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
			"Order %d claimed the buried bin at %s, AFTER order %d was committed and driving, so no "+
			"check at any Core moment could have seen it — the window is the mouth-to-slot drive "+
			"(27ms and 32s on the two rig specimens). Accepted and healed: the cascade dissolves and "+
			"re-plans, ~2.5 min of re-work. This is the measured price of law 6, not a defect. %s",
			burialShadowTag, BurialChurnMarker, at.placedBy,
			b.HolderID, b.HeldSince.UTC().Format(time.RFC3339), at.placedBy, where)
		s.burials.recordHard(hardApprovedThenInvalidated, now)
		return
	}

	log.Printf("%s: %s — a placement buried a bin a robot is en route to, without going through the "+
		"store-slot selector. The claim was already held when order %d was committed, so the selector "+
		"would have seen it. Expected count is ZERO: find the placement path and route it through "+
		"nodes.FindStoreSlotInLaneExcluding. %s",
		burialShadowTag, BurialBypassMarker, at.placedBy, where)
	s.burials.recordHard(hardNeverAsked, now)
}

// burialWasApprovedThenInvalidated answers PLAN §R.4's question: did this claim
// exist when the placing order's destination was committed?
//
// If it did NOT, the selector could not have seen it however diligently it was
// consulted, and the burial is churn the design accepts. If it DID, the selector
// would have refused — so reaching a burial means it was never asked.
//
// FAIL-LOUD ON A DOUBT, in both arms. An unreadable placer, a missing history
// row, or a zero claim time all return false, which sends the event to the
// should-be-zero bucket. A tripwire that under-reports is worse than one that
// over-reports — the same direction this file's dig-placement lookup already
// chooses, and the direction §R.4's ruling depends on to stay honest: the whole
// value of the split is that the remaining Bypass count means something.
func (s *BinService) burialWasApprovedThenInvalidated(placedBy int64, heldSince time.Time) bool {
	if placedBy == 0 || heldSince.IsZero() {
		return false
	}
	committedAt, ok, err := s.db.OrderCommittedToFleetAt(placedBy)
	if err != nil {
		log.Printf("%s: placer %d commit time unknown: %v (counting as a never-asked bypass)",
			burialShadowTag, placedBy, err)
		return false
	}
	if !ok {
		return false
	}
	return heldSince.UTC().After(committedAt)
}
