package service

import (
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
	// Bypass is the TRIPWIRE: a hard claim buried by a placement that should have
	// gone through the guarded selector. Expected ZERO.
	Bypass int64
	// DigUncovered is the known gap, kept apart from Bypass so it can never make
	// the tripwire look dirty: a dig leg's placement resolves through
	// findShuffleSlots and never consults the selector.
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

func (b *burialShadow) recordHard(bypass bool, at time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if bypass {
		b.tally.Bypass++
	} else {
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
	if digPlacement {
		log.Printf("%s: DIG-UNCOVERED lane=%s slot=%s placed_bin=%d placed_by=%d (a dig leg, resolved "+
			"through findShuffleSlots) buried_bin=%d (%s) buried_slot=%s buried_depth=%d hold=%s "+
			"holder_order=%d holder_status=%s held_for=%s",
			burialShadowTag, at.lane, at.slot, at.placedBin, at.placedBy,
			b.BinID, b.BinLabel, b.SlotName, b.Depth,
			kind, b.HolderID, b.HolderStatus, heldFor.Round(time.Second))
		s.burials.recordHard(false, now)
		return
	}
	log.Printf("%s: GUARD BYPASS — a placement buried a bin a robot is en route to, without going "+
		"through the store-slot selector. lane=%s slot=%s placed_bin=%d placed_by=%d buried_bin=%d (%s) "+
		"buried_slot=%s buried_depth=%d hold=%s holder_order=%d holder_status=%s held_for=%s. "+
		"Expected count is ZERO: find the placement path and route it through "+
		"nodes.FindStoreSlotInLaneExcluding",
		burialShadowTag, at.lane, at.slot, at.placedBin, at.placedBy,
		b.BinID, b.BinLabel, b.SlotName, b.Depth,
		kind, b.HolderID, b.HolderStatus, heldFor.Round(time.Second))
	s.burials.recordHard(true, now)
}
