package scenesim

import (
	"fmt"
	"sort"

	"shingocore/fleet"
)

// Action is what a block does at its location — the physics-relevant subset of a
// binTask. Pickups are outbound at a lane, dropoffs inbound; a plain move is a
// no-action waypoint.
type Action int

const (
	ActionMove Action = iota
	ActionPickup
	ActionDropoff
	ActionWait
)

func actionFor(binTask string) Action {
	switch binTask {
	case "JackLoad":
		return ActionPickup
	case "JackUnload":
		return ActionDropoff
	case "Wait":
		return ActionWait
	default:
		return ActionMove
	}
}

// Flags model the RDS vendor behaviors the bench has not yet pinned. Every
// unknown is a FLAG with a CONSERVATIVE default, never an assumption baked in;
// the soak matrix (later stages) sweeps both settings. S1 does not yet act on
// them beyond wiring — they are here so the reproductions and later arms read
// one honest place for "we don't know this yet."
type Flags struct {
	// ZoneCapacity: robots allowed inside a zone at once (1 = binary; conservative).
	ZoneCapacity int
	// QueueAtBoundary: true = a waiter holds at the zone boundary (managed);
	// false = it blocks on the approach aisle. Conservative = false (worse case).
	QueueAtBoundary bool
	// PassThroughGated: true = even non-stopping transit is gated by the zone.
	// Conservative = true (assume transit is gated until proven otherwise).
	PassThroughGated bool
	// PriorityAdmission: true = the vendor admits the highest-priority waiter
	// first; false = FCFS/path-order. Conservative = false (no priority help).
	PriorityAdmission bool
	// RestartPreservesZone: true = zone/mutex state survives an RDS restart;
	// false = it is lost. Conservative = false (assume state is lost).
	RestartPreservesZone bool
}

// ConservativeFlags returns the worst-case defaults — the setting a correct
// design must survive.
func ConservativeFlags() Flags {
	return Flags{
		ZoneCapacity:         1,
		QueueAtBoundary:      false,
		PassThroughGated:     true,
		PriorityAdmission:    false,
		RestartPreservesZone: false,
	}
}

// Block is one step of an order: go to Location and do Action.
type Block struct {
	Location string
	Action   Action
	done     bool
}

// Order is a robot's work: a block sequence. Dig marks a reshuffle compound that
// works a lane in BOTH directions and is therefore mode-exclusive (§2).
type Order struct {
	ID     string
	Blocks []Block
	Dig    bool

	// DigGroup names the reshuffle this order is a LEG of. Several robots can
	// carry legs of one dig, and a sibling leg is not a foreign dig.
	//
	// A dig submitted without a group becomes a group of ONE named after its
	// order — which is both what every pre-existing dig test means and the
	// harness's spelling of retrieve = dig(N=1). Nothing about those tests
	// changes.
	DigGroup string
}

// digGroup is a reshuffle's CLAIM on the lanes it works — the sim's spelling of
// the durable dig mouth row, and the reason this is not a one-line change.
//
// The claim used to be DERIVED: a dig held its lane while its order was active.
// With one order that is sound, because there is no gap. With several legs there
// is a gap between leg 1 completing and leg 2 being submitted, and a derived
// claim EVAPORATES in that gap and lets a store in — which is bit-for-bit the
// production bug fixed by exempting dig rows from the per-block early release. A
// harness that derives the claim from active legs reproduces that bug and is
// then structurally unable to catch a regression of it.
//
// So the claim has its own lifetime, owned by the group:
//
//	held(lane, G) == G claims lane AND (G is unsealed OR G has an active leg)
//
// ONE writer, one predicate, two inputs. A group of one is born SEALED, so its
// claim is exactly "has an active leg" — today's behaviour, unchanged. A
// multi-leg group stays claimed across the inter-leg gap until it is both sealed
// and finished.
//
// SEALEDNESS IS NOT req.Complete, though brief 2 §2 recommended it. Submit never
// read that field, so it carries no meaning today — and carryStoreWait
// (s1_test.go:35) omits it entirely while mode_share_test.go and s2_test.go
// submit it AS A DIG. Keying the close on it would silently give those two
// digs a claim that never closes. Sealedness is stated explicitly instead, by
// the constructor that knows: Submit seals a group of one at birth,
// SubmitDigLeg leaves a group open, SealDigGroup closes it.
type digGroup struct {
	sealed bool
	lanes  map[string]bool
	// seq orders claims so a lane has at most ONE holder. Two groups wanting the
	// same lane both record a claim; the lower seq HOLDS it and the other WAITS.
	//
	// Without this the two claims are symmetric and each group sees the other's,
	// so neither may enter and the lane deadlocks — which is exactly what the
	// two-different-digs case surfaced. Production never had the problem because
	// its claim is ACQUIRED, not recorded: AcquireLanes refuses a second dig
	// outright and the planner queues it. seq is the sim's spelling of
	// first-acquirer-wins.
	seq int
}

// cell is a robot position: a plain node, or a lane slot (Lane + Index).
type cell struct {
	Node  string
	Lane  string
	Index int
}

func plainCell(n string) cell       { return cell{Node: n} }
func laneCell(l string, i int) cell { return cell{Lane: l, Index: i} }
func (c cell) inLane() bool         { return c.Lane != "" }
func (c cell) key() string {
	if c.inLane() {
		return fmt.Sprintf("%s#%d", c.Lane, c.Index)
	}
	return c.Node
}

// Robot is a token on the scene.
type Robot struct {
	ID       string
	pos      cell
	entry    string // the plain node a lane was entered from (exit target)
	order    *Order
	block    int
	path     []cell // cells still to traverse toward the current block
	hop      int    // ticks remaining on the current cell-step
	idle     bool
	waiting  bool // parked on a Wait block until ReleaseWait
	carrying bool // holding a bin between a pickup and its dropoff (outbound state)

	approach int // coarse travel distance: extra aisle hops before the first lane entry (experiment)

	blockedBy string // set each tick to the robot blocking our next step ("" = free/moving)
}

// Options tune the coarse physics. HopTicks is the ticks per cell-step (ordering
// fidelity only — never seconds). Watchdog is the no-progress deadlock bound: if
// nothing changes for this many ticks while work is outstanding, the no-deadlock
// checker fires.
type Options struct {
	HopTicks int
	Watchdog int
}

// Sim is the scene-physics simulator: robots executing orders over a Scene under
// single-file lane occupancy, advanced one Tick at a time.
type Sim struct {
	scene        *Scene
	opts         Options
	flags        Flags
	mouthGate    bool // when true, lane entry is gated by mode + deepest-first discipline (§2)
	capacity1    bool // with mouthGate: one robot per lane (the baseline the soak compares against)
	priorityOnly bool // with mouthGate: model production-as-landed — mode gate only, NO deepest-first hold
	robots       map[string]*Robot
	order        []string             // robot ids, stable order for deterministic ticking
	occ          map[string]string    // cell key → robot id (lane cells only)
	bins         map[string]bool      // slot name → a dropped bin sits there (persists; walls deeper slots)
	digClaims    map[string]*digGroup // dig group name → its lane claim
	digClaimSeq  int                  // monotonic, orders competing claims

	tick         int
	lastProgress int // tick of the last observed state change (deadlock watchdog)
}

// New builds a Sim over a loaded scene.
func New(scene *Scene, opts Options) *Sim {
	if opts.HopTicks <= 0 {
		opts.HopTicks = 3
	}
	if opts.Watchdog <= 0 {
		opts.Watchdog = 50
	}
	return &Sim{
		scene:     scene,
		opts:      opts,
		flags:     ConservativeFlags(),
		robots:    map[string]*Robot{},
		occ:       map[string]string{},
		bins:      map[string]bool{},
		digClaims: map[string]*digGroup{},
	}
}

// SetMouthGate turns the lane mouth gate on (the arm under test). Off (the
// default) the sim reproduces today's ungated physics — the S1 wounds. On, the
// sim models the real mode-share mouth: same-kind robots co-occupy a single-file
// lane (no pass-through), mixed kind and dig are excluded, entrants stack
// deepest-first, and a departing robot waits until co-occupants have parked so it
// never crosses one on the way out. That turns the head-on deadlock and the
// entry-order air bubble green while exercising same-kind sharing. (See
// admitToLane + laneClearToExit.)
func (s *Sim) SetMouthGate(on bool) { s.mouthGate = on }

// SetLaneCapacity1 restricts a mouth-gated lane to ONE robot at a time — the
// conservative baseline, disabling same-kind co-occupancy. Used by the soak to
// measure what mode-share concurrency buys over capacity-1.
func (s *Sim) SetLaneCapacity1(on bool) { s.capacity1 = on }

// SetPriorityOnly models the CURRENT production arms as landed: the mode-based
// mouth gate (same-kind co-occupy, mixed/dig excluded) and single-file physics,
// but WITHOUT the deepest-first admission hold. In production the deepest-first
// ordering is only the RDS priority HINT — which influences the START/assignment
// order of orders (modeled here by submit order), NOT the arrival of an already-
// moving robot — not a hard mouth hold. That hold is exactly what tiered entry
// would add; leaving it OFF is how the wall experiment tests whether priority
// alone prevents the wall. Requires the mouth gate on; no effect otherwise.
func (s *Sim) SetPriorityOnly(on bool) { s.priorityOnly = on }

// SetRobotApproach sets a robot's coarse travel distance — extra aisle hops it
// burns before it first reaches a lane mouth (0 = adjacent). This lets the
// experiment vary robot start positions so a later-spawned but CLOSER robot can
// still win the race to the mouth (the crux of the wall). Harness knob only.
func (s *Sim) SetRobotApproach(robotID string, hops int) {
	if r := s.robots[robotID]; r != nil {
		r.approach = hops
	}
}

// SetFlags overrides the vendor-unknown flag defaults (for the soak matrix).
func (s *Sim) SetFlags(f Flags) { s.flags = f }

// Flags returns the current vendor-unknown flag settings.
func (s *Sim) Flags() Flags { return s.flags }

// PlaceBin marks a slot as already holding a bin at scene setup (pre-seeded
// inventory), so reachability/packing start from a real lane state.
func (s *Sim) PlaceBin(slot string) { s.bins[slot] = true }

// HasBin reports whether a bin currently sits in a slot. A store's bin appears
// here the tick its dropoff block completes, which makes this the harness's
// PLACEMENT signal — the physical event Core observes as a dropoff block reaching
// FINISHED, and therefore the moment a store stops blocking the lane behind it.
func (s *Sim) HasBin(slot string) bool { return s.bins[slot] }

// Carrying reports whether a robot is holding a bin between its pickup and
// dropoff — the harness's outbound state. A retrieve robot is carrying from the
// tick it picks up at the lane slot until it sets the bin down at its destination.
func (s *Sim) Carrying(robotID string) bool {
	if r := s.robots[robotID]; r != nil {
		return r.carrying
	}
	return false
}

// OrderActive reports whether an order is still being executed by some robot —
// the harness's COMPLETION signal, and the counterpart to HasBin. The gap between
// the two is the whole point of the A′ predicate: a store's bin is down (HasBin)
// well before its robot has backed out of the lane and gone idle (OrderActive), and
// releasing on the former rather than the latter is what placement-release means.
func (s *Sim) OrderActive(orderID string) bool {
	for _, id := range s.order {
		if r := s.robots[id]; r.order != nil && r.order.ID == orderID && !r.idle {
			return true
		}
	}
	return false
}

// ReleaseWait completes the Wait block a robot is parked on, letting it proceed.
// Mirrors the lifecycle sim's ReleaseOrder-appends-and-continues machinery.
func (s *Sim) ReleaseWait(orderID string) bool {
	for _, id := range s.order {
		r := s.robots[id]
		if r.order != nil && r.order.ID == orderID && r.waiting {
			r.waiting = false
			r.block++ // the wait block is satisfied; advance
			s.lastProgress = s.tick
			return true
		}
	}
	return false
}

// AppendBlocks adds blocks to a robot's in-flight order — the harness's /addBlocks,
// and the half of the lane-gate valve that Submit alone could not express.
//
// Submit takes the whole block list up front, so before this a DEFERRED TAIL was
// unmodellable and a gated order could only be faked as a pre-known plan behind a
// flag. That tests a mock of the design rather than the design: the real waybill
// genuinely does not contain its dropoff block until Core appends one.
//
// If the robot is already parked on the wait, appending satisfies it and the robot
// resumes — the same effect ReleaseWait has, because in production they are the
// same event (the append IS the release). If the robot has not reached the wait
// yet, the tail is simply there when it arrives and it never dwells at all: that
// is the open valve, and it is why an early append makes the gate invisible.
func (s *Sim) AppendBlocks(orderID string, blocks []fleet.OrderBlock) error {
	for _, id := range s.order {
		r := s.robots[id]
		if r.order == nil || r.order.ID != orderID {
			continue
		}
		for _, b := range blocks {
			if s.scene.Node(b.Location) == nil {
				return fmt.Errorf("scenesim: appended block location %q not in scene", b.Location)
			}
			r.order.Blocks = append(r.order.Blocks, Block{Location: b.Location, Action: actionFor(b.BinTask)})
		}
		if r.waiting && r.block+1 < len(r.order.Blocks) {
			r.waiting = false
			r.block++ // the wait is satisfied by the work that just arrived
		}
		r.idle = false
		s.lastProgress = s.tick
		return nil
	}
	return fmt.Errorf("scenesim: no robot is running order %q", orderID)
}

// WaitingOrders returns the ids of orders whose robot is currently parked on a
// Wait block, sorted. The observable the lane-gate scenarios assert on: an OPEN
// valve must never produce one (the robot rolls through), a CONTENDED valve must
// produce exactly the dwelling order.
func (s *Sim) WaitingOrders() []string {
	var out []string
	for _, id := range s.order {
		if r := s.robots[id]; r.waiting && r.order != nil {
			out = append(out, r.order.ID)
		}
	}
	sort.Strings(out)
	return out
}

// AddRobot places a robot at a plain start node.
func (s *Sim) AddRobot(id, startNode string) error {
	if _, dup := s.robots[id]; dup {
		return fmt.Errorf("scenesim: duplicate robot %q", id)
	}
	if s.scene.Node(startNode) == nil {
		return fmt.Errorf("scenesim: robot %q start node %q not in scene", id, startNode)
	}
	s.robots[id] = &Robot{ID: id, pos: plainCell(startNode), idle: true}
	s.order = append(s.order, id)
	sort.Strings(s.order)
	return nil
}

// Submit assigns an order (from a real fleet request) to a robot. The robot must
// be idle. Blocks are derived from the request's block list; dig marks the whole
// order mode-exclusive.
func (s *Sim) Submit(robotID string, req fleet.CreateOrderRequest, dig bool) error {
	r := s.robots[robotID]
	if r == nil {
		return fmt.Errorf("scenesim: no robot %q", robotID)
	}
	if !r.idle {
		return fmt.Errorf("scenesim: robot %q is busy", robotID)
	}
	ord := &Order{ID: req.OrderID, Dig: dig}
	if dig {
		// A dig submitted through Submit is a group of ONE, named after its order
		// and sealed at birth. Its claim is therefore exactly "has an active leg",
		// which is what the derived claim has always meant — so every dig test
		// that predates groups keeps its behaviour with no edit. Multi-leg groups
		// come in through SubmitDigLeg.
		ord.DigGroup = req.OrderID
	}
	for _, b := range req.Blocks {
		if s.scene.Node(b.Location) == nil {
			return fmt.Errorf("scenesim: order %s block location %q not in scene", req.OrderID, b.Location)
		}
		ord.Blocks = append(ord.Blocks, Block{Location: b.Location, Action: actionFor(b.BinTask)})
	}
	if dig {
		s.claimLanesFor(ord, true)
	}
	r.order = ord
	r.block = 0
	r.idle = len(ord.Blocks) == 0
	r.path = nil
	return nil
}

// Tick advances the world one step and returns any checker violations observed
// after the step. A robot with a current block plans a path (once), then steps
// one cell per HopTicks ticks when its next cell is free.
func (s *Sim) Tick() []Violation {
	s.tick++
	moved := false

	for _, id := range s.order {
		r := s.robots[id]
		r.blockedBy = ""
		if r.idle || r.order == nil {
			continue
		}
		if r.waiting {
			continue // parked on a Wait block; ReleaseWait resumes it
		}
		// Finished all blocks? Exit the lane if inside, then go idle.
		if r.block >= len(r.order.Blocks) {
			if r.pos.inLane() {
				if !s.laneClearToExit(r) {
					continue // hold: another robot in this lane is still entering
				}
				s.ensurePath(r, plainCell(r.exitTarget()))
			} else {
				r.idle = true
				r.order = nil
				r.path = nil
				moved = true
				continue
			}
		} else {
			s.ensurePath(r, s.targetCell(r.order.Blocks[r.block].Location))
		}

		if len(r.path) == 0 {
			// Arrived at the current block's location.
			if r.block < len(r.order.Blocks) {
				b := &r.order.Blocks[r.block]
				if b.Action == ActionWait && r.block+1 >= len(r.order.Blocks) {
					// Nothing follows the wait, so the order is still UNSEALED and
					// there is no work to go to: hold here until the tail arrives
					// (AppendBlocks) or ReleaseWait fires.
					//
					// The test is structural — "is there a block after this one" —
					// not a flag. A wait with work already behind it is satisfied on
					// arrival and the robot rolls straight through, which is exactly
					// the open lane gate: Core appended the tail before the robot got
					// here, so the wait costs nothing. A wait that is last is the
					// contended gate, or the operator-release dwell the swap fixtures
					// model (their Wait is always the final block, so they park
					// exactly as before).
					r.waiting = true
					continue
				}
				if b.Action == ActionDropoff {
					if s.scene.slotLane[b.Location] != "" {
						s.bins[b.Location] = true // a bin now sits in this slot (persists, walls deeper)
					}
					r.carrying = false // a held bin (from a prior pickup) is now set down
				}
				if b.Action == ActionPickup {
					if s.scene.slotLane[b.Location] != "" {
						delete(s.bins, b.Location) // the bin leaves the slot, carried off by the robot
					}
					r.carrying = true // the robot is now holding a bin (set down at the next dropoff)
				}
				b.done = true
				r.block++
				moved = true
			}
			continue
		}

		// Step toward path[0], honoring lane single-file occupancy AND bins: a
		// robot cannot pass a slot that already holds a bin (single-file wall) —
		// this is what makes a walled-off deep slot physical.
		next := r.path[0]
		if next.inLane() {
			// Mouth gate: a FRESH entry into a lane (from a plain node onto the
			// mouth) is gated by mode + deepest-first discipline. Held robots wait
			// outside the lane; they do not set blockedBy (they are not trapped by
			// a specific robot, so they don't form a deadlock cycle) — the holder
			// they wait on is making progress, which the watchdog sees.
			if !r.pos.inLane() && !s.admitToLane(r, next) {
				continue
			}
			if holder, occupied := s.occ[next.key()]; occupied && holder != id {
				r.blockedBy = holder // trapped behind another robot
				continue
			}
			if slot := s.cellSlot(next); slot != "" && s.bins[slot] {
				// A bin walls a robot still heading to its target — it cannot reach or
				// place behind a bin in a single-file lane (the air bubble;
				// reachability reports it). But a robot that has FINISHED its in-lane
				// work transits the aisle out past parked bins, so co-occupying
				// same-kind robots (leader deep, follower shallow) can both leave.
				//
				// The ONE bin a robot may always step onto is its own current pickup
				// target: that bin is the destination, not an obstacle, and the robot
				// must reach it to pick it up. (A retrieve's first block is the lane
				// slot it pulls from; without this exemption a buried bin could never
				// be picked at all.)
				if r.order != nil && r.block < len(r.order.Blocks) && !s.pickingTarget(r, slot) {
					continue
				}
			}
		}
		if r.hop <= 0 {
			r.hop = s.opts.HopTicks
		}
		r.hop--
		if r.hop > 0 {
			moved = true
			continue
		}
		// Commit the cell-step: release the old lane cell, occupy the new one.
		if r.pos.inLane() {
			delete(s.occ, r.pos.key())
		}
		if next.inLane() {
			s.occ[next.key()] = id
		}
		if !r.pos.inLane() && next.inLane() {
			r.entry = r.pos.Node // remember where we entered from, to exit later
		}
		r.pos = next
		r.path = r.path[1:]
		moved = true
	}

	if moved {
		s.lastProgress = s.tick
	}
	return s.check()
}

// exitTarget is the plain node a robot leaves a lane back onto.
func (r *Robot) exitTarget() string {
	if r.entry != "" {
		return r.entry
	}
	return "" // unknown — will be caught as an invalid path
}

// headingToGate reports whether dst is the plain wait/gate point the robot is
// PRE-POSITIONING toward — the leg a gated retrieve drives before its lane opens.
//
// Only the FIRST block qualifies: a retrieve's create is [wait@gate] and nothing
// else, so its gate drive is its first real movement. A store's create is
// [pickup@source, wait@gate, …] — its gate is a mid-order waypoint after the pickup,
// and shifting that store's approach onto the gate leg would re-time every gate-
// controller scenario. Restricting to block 0 keeps stores on their original lane-
// entry approach semantics and reserves the gate-drive overlap for retrieves.
func (s *Sim) headingToGate(r *Robot, dst cell) bool {
	if dst.inLane() || r.order == nil || r.block != 0 || len(r.order.Blocks) == 0 {
		return false
	}
	b := r.order.Blocks[0]
	return b.Action == ActionWait && b.Location == dst.Node
}

// claimLanesFor records the lanes an order's blocks touch as claimed by its dig
// group, creating the group if new. sealed says whether the group is complete at
// this moment: true for a group of one (Submit), false for a group still taking
// legs (SubmitDigLeg).
func (s *Sim) claimLanesFor(ord *Order, sealed bool) {
	if ord.DigGroup == "" {
		return
	}
	g := s.digClaims[ord.DigGroup]
	if g == nil {
		s.digClaimSeq++
		g = &digGroup{lanes: map[string]bool{}, seq: s.digClaimSeq}
		s.digClaims[ord.DigGroup] = g
	}
	g.sealed = g.sealed || sealed
	for _, b := range ord.Blocks {
		if lane := s.scene.LaneForNode(b.Location); lane != "" {
			g.lanes[lane] = true
		}
	}
}

// SubmitDigLeg assigns ONE LEG of a multi-robot dig. The group is created on the
// first leg and stays OPEN — claiming its lanes across the gaps between legs —
// until SealDigGroup closes it.
//
// This is the entry point that makes several robots able to work one reshuffle.
// Submit remains the group-of-one door and is unchanged in effect.
func (s *Sim) SubmitDigLeg(robotID string, req fleet.CreateOrderRequest, group string) error {
	if group == "" {
		return fmt.Errorf("scenesim: SubmitDigLeg needs a group name")
	}
	if err := s.Submit(robotID, req, true); err != nil {
		return err
	}
	ord := s.robots[robotID].order
	// Submit sealed it as a group of one under its own name; re-home it into the
	// real group, which is open until sealed.
	delete(s.digClaims, ord.ID)
	ord.DigGroup = group
	if g := s.digClaims[group]; g != nil {
		g.sealed = false
	}
	s.claimLanesFor(ord, false)
	return nil
}

// SealDigGroup declares that a dig group will take no further legs. Its claim
// then ends when its last active leg finishes — not before, and not on any other
// signal. Sealing a group that does not exist is a no-op.
//
// This is the ONLY closer. There is deliberately no second path that also
// releases a claim (an explicit close mixed with an auto-close on
// last-leg-completion would be two writers for one fact, which is the mistake
// this whole line of work has twice paid to undo).
func (s *Sim) SealDigGroup(group string) {
	if g := s.digClaims[group]; g != nil {
		g.sealed = true
	}
}

// digGroupHasActiveLeg reports whether any robot is currently running a leg of
// the group.
func (s *Sim) digGroupHasActiveLeg(group string) bool {
	for _, id := range s.order {
		o := s.robots[id]
		if o.order != nil && !o.idle && o.order.DigGroup == group {
			return true
		}
	}
	return false
}

// laneClaimedByOtherDigGroup reports whether a dig group OTHER than myGroup holds
// lane — the dig's mode-exclusive hold, spanning its legs AND the gaps between
// them.
//
// It replaces laneHasActiveDig, and the difference is the point. The old form
// keyed on the ROBOT and derived the claim from "some order is active", so it
// could not tell a sibling leg of my own dig from a foreign one, and it dropped
// the claim the instant a leg finished. This keys on the GROUP and reads the
// group's own claim, which outlives any single leg.
//
// A group of one behaves exactly as before: born sealed, so its claim is live
// precisely while its one leg is.
func (s *Sim) laneClaimedByOtherDigGroup(lane, myGroup string) bool {
	holder := s.digClaimHolder(lane)
	return holder != "" && holder != myGroup
}

// digClaimHolder returns the ONE dig group holding lane, or "" if none.
//
// At most one, by claim order: several groups may want a lane, and the earliest
// live claim holds it while the rest wait at the mouth. Symmetric claims would
// deadlock the lane — each group seeing the other's and neither entering — which
// is what the two-different-digs case found. Production is a first-acquirer-wins
// row (AcquireLanes refuses the second dig and the planner queues it); this is
// the same rule.
//
// A claim is live while its group is unsealed OR has a leg running, so it spans
// the gaps between legs and ends when a sealed group finishes.
func (s *Sim) digClaimHolder(lane string) string {
	holder, best := "", 0
	for name, g := range s.digClaims {
		if !g.lanes[lane] {
			continue
		}
		if g.sealed && !s.digGroupHasActiveLeg(name) {
			continue // finished: claim released
		}
		if holder == "" || g.seq < best {
			holder, best = name, g.seq
		}
	}
	return holder
}

// pickingTarget reports whether the robot's current block is a pickup AT slot —
// the one bin it is allowed to step onto despite the wall, because the bin is its
// destination, not an obstacle. A dig's unbury pickups use the same path.
func (s *Sim) pickingTarget(r *Robot, slot string) bool {
	if r.order == nil || r.block >= len(r.order.Blocks) {
		return false
	}
	b := r.order.Blocks[r.block]
	return b.Action == ActionPickup && b.Location == slot
}

// cellSlot returns the slot name for a lane cell, or "".
func (s *Sim) cellSlot(c cell) string {
	if !c.inLane() {
		return ""
	}
	lane := s.scene.lanes[c.Lane]
	if lane == nil || c.Index < 0 || c.Index >= len(lane.Slots) {
		return ""
	}
	return lane.Slots[c.Index]
}

// admitToLane decides whether robot r may enter lane next.Lane now (called only
// on a fresh entry from a plain node). Returns true when the gate is off.
//
// With the gate on it models the REAL mode-share mouth (§1–§2): same-KIND robots
// legally co-occupy a single-file lane (press left+right, store-behind-store),
// mixed kind and dig are excluded, and there is NO pass-through — an entrant must
// stack SHALLOWER than every occupant's current cell (it can never pass one), so
// the leader parks deepest and followers line up toward the mouth. Among same-mode
// waiters the DEEPEST target enters first, so bins pack back-to-front (no
// entry-order air bubble). Exit crossing is prevented separately (laneClearToExit).
func (s *Sim) admitToLane(r *Robot, next cell) bool {
	if !s.mouthGate {
		return true
	}
	lane := next.Lane
	myMode, ok := s.orderLaneMode(r, lane)
	if !ok {
		return true // r doesn't actually work this lane — nothing to gate
	}
	myDepth := s.currentTargetDepth(r, lane)

	// Capacity-1 baseline (comparison mode): one robot works the lane at a time,
	// so it must be clear of every other robot before entry.
	if s.capacity1 {
		for _, id := range s.order {
			o := s.robots[id]
			if o.ID != r.ID && o.pos.inLane() && o.pos.Lane == lane {
				return false
			}
		}
	}

	// Dig hold (the sim's ModeDig mouth row). A dig works a lane across MULTIPLE
	// legs — it leaves to relocate a blocker to a shuffle slot, then returns for
	// the buried target. Production holds the lane exclusive for the whole compound
	// via a durable 'dig' mouth reservation (ModeDig is always-exclusive: it admits
	// no other holder and no other holder admits it). The occupant scan below only
	// sees robots currently IN the lane, so during a dig's out-and-back leg the lane
	// would read empty and a store would slip in — then the dig returns and the two
	// collide. So an active dig claims its lane for the lifetime of the order, not
	// just the ticks its robot is physically inside. (The dig itself is admitted by
	// the same-origin/mode logic below; only OTHERS are kept out here.)
	// NO myMode != "dig" GUARD. There used to be one, and it left a hole: a
	// FOREIGN dig skipped the claim check entirely and was caught only by the
	// occupant loop below — which sees nothing while the holding dig is out on a
	// parking leg. Two digs could interleave on one lane. It was latent only
	// because nothing submitted two digs to one lane; groups make it reachable,
	// so the claim check now applies to dig entrants of a different group too.
	myGroup := ""
	if r.order != nil {
		myGroup = r.order.DigGroup
	}
	if s.laneClaimedByOtherDigGroup(lane, myGroup) {
		return false
	}

	// Against robots already inside the lane: same-kind shares, mixed/dig waits,
	// and no entrant may pass or reach an occupant.
	for _, id := range s.order {
		o := s.robots[id]
		if o.ID == r.ID || !o.pos.inLane() || o.pos.Lane != lane {
			continue
		}
		om, _ := s.orderLaneMode(o, lane)
		if myMode == "dig" || om == "dig" || om != myMode {
			return false // mixed mode / dig — wait
		}
		if myDepth >= o.pos.Index {
			return false // would pass or reach an occupant — wait for it to advance
		}
	}

	// Deepest-first among same-mode waiters still outside: a robot whose order is
	// bound DEEPER into this lane enters first, so the stack builds leader-deepest
	// (no entry-order air bubble). This looks at the order's eventual in-lane target
	// (currentTargetDepth scans forward), not just its current block — else a fast
	// shallow store could reach the mouth and drop before a slower deep store, still
	// picking, has even declared its lane target.
	//
	// SKIPPED under priorityOnly: production has no such hold — the deepest-first
	// ordering there is only the RDS priority start-hint (modeled via submit order),
	// which cannot hold an already-moving shallower robot. This is the arm the wall
	// experiment leaves out to test whether priority alone suffices.
	if !s.priorityOnly {
		for _, id := range s.order {
			o := s.robots[id]
			if o.ID == r.ID || o.idle || o.order == nil || o.pos.inLane() {
				continue
			}
			om, ok := s.orderLaneMode(o, lane)
			if !ok || om != myMode || myMode == "dig" {
				continue // o doesn't work this lane in my mode
			}
			if s.currentTargetDepth(o, lane) > myDepth {
				return false
			}
		}
	}
	return true
}

// laneClearToExit reports whether robot r (done with its in-lane work, sitting in
// a lane) may start moving out. A departing robot moves toward the mouth; if
// another robot is still ENTERING (moving deeper toward its park slot) they would
// cross in the single-file aisle. So an exit waits until every other occupant has
// reached its target (all parked). Then exits proceed shallowest-first — the
// single-file occupancy handles that ordering. A no-op when the gate is off.
func (s *Sim) laneClearToExit(r *Robot) bool {
	if !s.mouthGate {
		return true
	}
	lane := r.pos.Lane
	for _, id := range s.order {
		o := s.robots[id]
		if o.ID == r.ID || !o.pos.inLane() || o.pos.Lane != lane {
			continue
		}
		if td := s.currentTargetDepth(o, lane); td > o.pos.Index {
			return false // o is still moving deeper — hold so we don't cross it
		}
	}
	return true
}

// currentTargetDepth is the depth of r's current block if it targets lane, else
// the first pending in-lane block's depth, else -1.
func (s *Sim) currentTargetDepth(r *Robot, lane string) int {
	if r.order == nil {
		return -1
	}
	for i := r.block; i < len(r.order.Blocks); i++ {
		loc := r.order.Blocks[i].Location
		if s.scene.slotLane[loc] == lane {
			d, _ := s.scene.SlotDepth(loc)
			return d
		}
	}
	return -1
}

// targetCell resolves a block location to a cell (a lane slot or a plain node).
func (s *Sim) targetCell(location string) cell {
	if lane := s.scene.slotLane[location]; lane != "" {
		idx, _ := s.scene.SlotDepth(location)
		return laneCell(lane, idx)
	}
	return plainCell(location)
}

// ensurePath (re)plans a robot's cell path toward dst if it doesn't already lead
// there. Coarse: plain→plain is a single hop; lane entry/exit steps cell by cell
// through the mouth so single-file trapping is physical.
func (s *Sim) ensurePath(r *Robot, dst cell) {
	if len(r.path) > 0 && r.path[len(r.path)-1] == dst {
		return
	}
	r.path = s.planPath(r.pos, dst)
	// Coarse travel distance: burn `approach` aisle hops (self-steps on the robot's
	// current node) before it reaches its work, so a later-but-closer robot can still
	// win the race to the mouth. One-shot — consumed the first time the robot burns it.
	//
	// Two destination kinds trigger it:
	//   - a fresh LANE entry (an inbound store's first real leg is the drive to the
	//     mouth) — the original case; and
	//   - a drive to the order's GATE/wait point (an outbound retrieve's pre-positioning
	//     leg is plain→gate). Without this, a gated retrieve's approach lands AFTER the
	//     dig it was meant to overlap, and pre-positioning buys nothing.
	if r.approach > 0 && !r.pos.inLane() && (dst.inLane() || s.headingToGate(r, dst)) {
		lead := make([]cell, r.approach)
		for i := range lead {
			lead[i] = r.pos
		}
		r.path = append(lead, r.path...)
		r.approach = 0
	}
	r.hop = 0
}

func (s *Sim) planPath(from, to cell) []cell {
	// Already there.
	if from == to {
		return nil
	}
	var path []cell
	cur := from
	// If inside a lane and the destination isn't a deeper cell of the SAME lane,
	// walk out to the mouth and onto the entry node first.
	if cur.inLane() && !(to.inLane() && to.Lane == cur.Lane && to.Index > cur.Index) {
		for i := cur.Index - 1; i >= 0; i-- {
			path = append(path, laneCell(cur.Lane, i))
		}
		exit := to.Node
		if to.inLane() { // moving to a different lane — exit onto whatever entry we had
			exit = ""
		}
		if exit != "" {
			path = append(path, plainCell(exit))
		}
		cur = plainCell(exit)
	}
	// Same-lane deeper move: step through intervening cells.
	if to.inLane() && cur.inLane() && to.Lane == cur.Lane {
		for i := cur.Index + 1; i <= to.Index; i++ {
			path = append(path, laneCell(cur.Lane, i))
		}
		return path
	}
	// Entering a lane from outside: step mouth → target depth.
	if to.inLane() {
		for i := 0; i <= to.Index; i++ {
			path = append(path, laneCell(to.Lane, i))
		}
		return path
	}
	// Plain → plain: one coarse hop.
	if to.Node != cur.Node {
		path = append(path, to)
	}
	return path
}

// AllIdle reports whether every robot has finished its order.
func (s *Sim) AllIdle() bool {
	for _, r := range s.robots {
		if !r.idle {
			return false
		}
	}
	return true
}

// BusyCount returns how many robots still have unfinished work. At the end of a
// store-only run this is the number of robots left STUCK — walled behind a bin
// they can never pass in the single-file lane — so it counts walled stores.
func (s *Sim) BusyCount() int {
	n := 0
	for _, r := range s.robots {
		if !r.idle {
			n++
		}
	}
	return n
}

// RunUntilIdle ticks until all robots are idle or maxTicks is reached. Returns
// the tick count, any violations seen along the way, and whether it settled.
func (s *Sim) RunUntilIdle(maxTicks int) (ticks int, violations []Violation, settled bool) {
	for range maxTicks {
		v := s.Tick()
		violations = append(violations, v...)
		if s.AllIdle() {
			return s.tick, violations, true
		}
	}
	return s.tick, violations, false
}

// Tick returns the current tick count.
func (s *Sim) TickCount() int { return s.tick }
