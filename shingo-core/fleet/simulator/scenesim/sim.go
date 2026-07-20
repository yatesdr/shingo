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
	ID      string
	pos     cell
	entry   string // the plain node a lane was entered from (exit target)
	order   *Order
	block   int
	path    []cell // cells still to traverse toward the current block
	hop     int    // ticks remaining on the current cell-step
	idle    bool
	waiting bool // parked on a Wait block until ReleaseWait

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
	scene  *Scene
	opts   Options
	flags  Flags
	robots map[string]*Robot
	order  []string          // robot ids, stable order for deterministic ticking
	occ    map[string]string // cell key → robot id (lane cells only)
	bins   map[string]bool   // slot name → a dropped bin sits there (persists; walls deeper slots)

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
		scene:  scene,
		opts:   opts,
		flags:  ConservativeFlags(),
		robots: map[string]*Robot{},
		occ:    map[string]string{},
		bins:   map[string]bool{},
	}
}

// SetFlags overrides the vendor-unknown flag defaults (for the soak matrix).
func (s *Sim) SetFlags(f Flags) { s.flags = f }

// Flags returns the current vendor-unknown flag settings.
func (s *Sim) Flags() Flags { return s.flags }

// PlaceBin marks a slot as already holding a bin at scene setup (pre-seeded
// inventory), so reachability/packing start from a real lane state.
func (s *Sim) PlaceBin(slot string) { s.bins[slot] = true }

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
	for _, b := range req.Blocks {
		if s.scene.Node(b.Location) == nil {
			return fmt.Errorf("scenesim: order %s block location %q not in scene", req.OrderID, b.Location)
		}
		ord.Blocks = append(ord.Blocks, Block{Location: b.Location, Action: actionFor(b.BinTask)})
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
				if b.Action == ActionWait {
					r.waiting = true // hold here until ReleaseWait; do NOT advance
					continue
				}
				if b.Action == ActionDropoff {
					if s.scene.slotLane[b.Location] != "" {
						s.bins[b.Location] = true // a bin now sits in this slot (persists, walls deeper)
					}
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
			if holder, occupied := s.occ[next.key()]; occupied && holder != id {
				r.blockedBy = holder // trapped behind another robot
				continue
			}
			if slot := s.cellSlot(next); slot != "" && s.bins[slot] {
				continue // walled by a bin; reachability reports the unreachable slot
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
