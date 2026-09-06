package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// The stamp site: HandleBinPickedUp, above the location gate.
//
// swap_leg_standard_test.go walks the builders' shapes against the predicates.
// These drive the REAL handler with the REAL builders' step lists, so they
// exercise the wiring — claim resolution, the steps decode, the location
// compare, stamp-once — and in particular the thing the predicate cannot see:
// that the stamp fires on the proof step and on NO EARLIER cell pickup.
//
// The cell here is seedSwapClaim's: PRESS (process node), INDEX-B (paired),
// IN-STAGING, OUT-STAGING, with MARKET-EMPTIES / MARKET off-cell.

// stampFixture is one leg under test: a claim seeded in the DB, an order row
// carrying that leg's real steps, and a log capture.
type stampFixture struct {
	eng     *Engine
	db      *store.DB
	nodeID  int64
	claim   *processes.NodeClaim
	order   *storeorders.Order
	logs    *[]string
	pickups []string // every cell node this leg picks up from, in plan order
	places  bool     // the leg leaves a bin on claim.CoreNodeName
}

// newStampFixture seeds the cell, flips the claim if asked, builds the leg's
// steps from the shipping builder, and stores them on an order at the node.
func newStampFixture(t *testing.T, mode protocol.SwapMode, secondPaired string, flipped bool, legB bool) *stampFixture {
	t.Helper()
	db := testEngineDB(t)
	nodeID, node, claim := seedSwapClaim(t, db, mode, secondPaired)
	if flipped {
		// Persisted, not just set in memory: the handler re-reads the claim
		// through requestedClaimAtNode, so an in-memory flip would build flipped
		// steps and stamp them against an unflipped claim.
		yes := true
		_, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
			StyleID: claim.StyleID, CoreNodeName: claim.CoreNodeName,
			Role: claim.Role, SwapMode: claim.SwapMode, PayloadCode: claim.PayloadCode,
			UOPCapacity:          claim.UOPCapacity,
			InboundSource:        claim.InboundSource,
			InboundStaging:       claim.InboundStaging,
			OutboundStaging:      claim.OutboundStaging,
			OutboundDestination:  claim.OutboundDestination,
			PairedCoreNode:       claim.PairedCoreNode,
			SecondPairedCoreNode: claim.SecondPairedCoreNode,
			IndexRobotSupplies:   &yes,
		})
		testutil.MustNoErr(t, err, "flip claim")
		claim = requestedClaimAtNode(db, node)
		if claim == nil || !claim.IndexRobotSupplies {
			t.Fatal("the flip did not persist — the fixture would test the unflipped shape")
		}
	}

	disp, err := BuildSwapDispatch(node, claim)
	testutil.MustNoErr(t, err, "build dispatch")
	steps := disp.StepsA
	if legB {
		steps = disp.StepsB
	}
	if len(steps) == 0 {
		t.Fatal("builder produced no steps — the claim seed is missing a required field")
	}

	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	// A live cell always has a runtime row, and its active_bin_id is nil across
	// exactly the window these cases drive: cleared by the leg’s own press pickup,
	// re-set by its step-7 dropoff. Seeding it makes an UNREADABLE runtime a
	// deliberate case rather than an accident of the fixture — the departure
	// gate fails closed on one, and a fixture that never had a row would take
	// that path everywhere without saying so.
	_, rerr := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, rerr, "ensure runtime")

	f := &stampFixture{
		eng: eng, db: db, nodeID: nodeID, claim: claim,
		order: mkSwapLeg(t, db, nodeID, "uuid-stamp-leg", steps, ""),
		logs:  &logs,
	}
	cell := cellSetFor(claim)
	for _, s := range steps {
		if s.Action == protocol.ActionPickup && cell[s.Node] {
			f.pickups = append(f.pickups, s.Node)
		}
	}
	f.places = legPlacesBinAt(steps, claim.CoreNodeName)
	return f
}

// placedBinID is the carrier the leg leaves on the machine at its step-7
// dropoff. Any id will do; it is named so the assertions read as a fact about
// a bin rather than a magic number.
const placedBinID = int64(77)

// recordPlacement drives the step-7 record: Core's intermediate dropoff
// broadcasts UOPAdjustment{Bound} and the Edge binds the arrived bin to the
// cell. The REAL handler, not a direct runtime write — settleCellPlacement is
// wired inside it, and a fixture that wrote active_bin_id by hand would
// exercise the stamp while leaving the second trigger untested.
func (f *stampFixture) recordPlacement(t *testing.T) {
	t.Helper()
	f.eng.HandleUOPAdjustment(protocol.UOPAdjustment{
		BinID:        placedBinID,
		CoreNodeName: f.claim.CoreNodeName,
		NewRemaining: 40,
		Epoch:        1,
		Bound:        true,
	})
	rt, err := f.db.GetProcessNodeRuntime(f.nodeID)
	testutil.MustNoErr(t, err, "runtime after the bind")
	if rt.ActiveBinID == nil || *rt.ActiveBinID != placedBinID {
		t.Fatalf("the placement did not bind: active_bin_id=%v, want %d", rt.ActiveBinID, placedBinID)
	}
}

// heldLogs returns the "left the cell but its placement is not recorded" lines
// — the sentence a cell held by the conjunction is owed.
func (f *stampFixture) heldLogs() []string {
	var out []string
	for _, l := range *f.logs {
		if strings.Contains(l, "placement at") && strings.Contains(l, "is not recorded") {
			out = append(out, l)
		}
	}
	return out
}

func (f *stampFixture) pickUpAt(t *testing.T, location string) {
	t.Helper()
	f.eng.HandleBinPickedUp(f.order.UUID, 0, location)
}

func (f *stampFixture) departedAt(t *testing.T) *storeorders.Order {
	t.Helper()
	o, err := f.db.GetOrder(f.order.ID)
	testutil.MustNoErr(t, err, "re-read order")
	return o
}

func (f *stampFixture) departureLogs() []string {
	var out []string
	for _, l := range *f.logs {
		if strings.Contains(l, "DEPARTED the cell") {
			out = append(out, l)
		}
	}
	return out
}

// TestBinPickedUp_StampsOnTheProofStepAndNoEarlierOne walks every leg that has
// an off-cell tail — the only shape a BinPickedUp can stamp — and fires the
// pickup event at each of the leg's cell pickups IN PLAN ORDER. The stamp must
// land on the last one, on none before it, and only once.
//
// single_robot is the row this exists for: its press pickup is FIVE steps
// before the leg is done with the cell, and it is the step the old pointer
// clear declared the cell free at.
//
// The re-fire at the end is the Core-restart pin: blockStates is in-memory, so
// a restart re-fires every already-FINISHED block once. A second stamp would
// move the instant to a time the robot was nowhere near the cell, and a second
// log line would make a restart look like a second departure.
func TestBinPickedUp_StampsOnTheProofStepAndNoEarlierOne(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name      string
		mode      protocol.SwapMode
		second    string
		flipped   bool
		legB      bool
		wantAt    string
		wantEarly []string // cell pickups that must NOT stamp
	}{
		{
			name: "single_robot/A", mode: protocol.SwapModeSingleRobot,
			wantAt:    "OUT-STAGING",
			wantEarly: []string{"PRESS", "IN-STAGING"},
		},
		{
			name: "two_robot/B evac", mode: protocol.SwapModeTwoRobot, legB: true,
			wantAt: "PRESS",
		},
		{
			name: "press_index/flipped/2pos/R1", mode: protocol.SwapModeTwoRobotPressIndex,
			flipped: true, wantAt: "PRESS",
		},
		{
			name: "press_index/flipped/3pos/R1", mode: protocol.SwapModeTwoRobotPressIndex,
			second: "INDEX-C", flipped: true, wantAt: "PRESS",
		},
		{
			name: "sequential/removal", mode: protocol.SwapModeSequential,
			wantAt: "PRESS",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStampFixture(t, tc.mode, tc.second, tc.flipped, tc.legB)

			// Sanity: the fixture must actually exercise the pickups named.
			last := f.pickups[len(f.pickups)-1]
			if last != tc.wantAt {
				t.Fatalf("fixture drift: the leg's last cell pickup is %q, table says the proof step is %q (all cell pickups: %v)",
					last, tc.wantAt, f.pickups)
			}
			if len(tc.wantEarly) > 0 && len(f.pickups) != len(tc.wantEarly)+1 {
				t.Fatalf("fixture drift: cell pickups %v, table names %d early ones plus the proof step", f.pickups, len(tc.wantEarly))
			}

			for _, early := range tc.wantEarly {
				f.pickUpAt(t, early)
				if o := f.departedAt(t); o.Departed {
					t.Fatalf("the leg departed at %q — that is an EARLIER cell pickup. It is still holding a "+
						"cell slot and a second swap dropped there would collide. Proof step is %q.", early, tc.wantAt)
				}
			}

			// A leg that leaves a bin on the machine owes the cell a placement,
			// and departure is now the CONJUNCTION of leaving the cell's nodes
			// with that placement being recorded. single_robot is the only row
			// here that places, and its step-7 dropoff falls between the early
			// pickups and the proof step — so that is where the record goes. The
			// legs that place nothing need no record and must not be given one,
			// or the case would stop testing the conjunction at all.
			if f.places {
				f.recordPlacement(t)
				if o := f.departedAt(t); o.Departed {
					t.Fatal("the leg departed at its own PLACEMENT — the robot is still standing at the " +
						"cell holding a staging slot; leaving the cell's nodes is the other half")
				}
			}

			f.pickUpAt(t, tc.wantAt)
			o := f.departedAt(t)
			if !o.Departed || o.DepartedAt == nil {
				t.Fatalf("the leg did not depart at its proof step %q (departed=%v at=%v); logs=%v",
					tc.wantAt, o.Departed, o.DepartedAt, *f.logs)
			}
			if got := f.departureLogs(); len(got) != 1 {
				t.Errorf("departure log lines = %d, want exactly 1: %v", len(got), got)
			}

			// Core restarts and re-fires the same FINISHED block.
			f.pickUpAt(t, tc.wantAt)
			f.pickUpAt(t, tc.wantAt)
			again := f.departedAt(t)
			if again.DepartedAt == nil || !again.DepartedAt.Equal(*o.DepartedAt) {
				t.Errorf("departed_at moved on replay: %v then %v", o.DepartedAt, again.DepartedAt)
			}
			if got := f.departureLogs(); len(got) != 1 {
				t.Errorf("departure log lines after 3 identical events = %d, want 1: %v", len(got), got)
			}
		})
	}
}

// TestBinPickedUp_NeverStampsALegThatEndsAtTheCell is the dual. A leg whose
// last cell step is its OWN FINAL step has not left the cell until it is
// finished — terminal (for a placing leg, the operator's CONFIRM) is what
// releases the cell, and there is nothing for BinPickedUp to stamp. Firing the
// event at every one of its cell pickups must leave departed_at NULL.
func TestBinPickedUp_NeverStampsALegThatEndsAtTheCell(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name    string
		mode    protocol.SwapMode
		second  string
		flipped bool
		legB    bool
	}{
		{name: "two_robot/A supply", mode: protocol.SwapModeTwoRobot},
		{name: "press_index/unflipped/2pos/R1", mode: protocol.SwapModeTwoRobotPressIndex},
		{name: "press_index/unflipped/3pos/R1", mode: protocol.SwapModeTwoRobotPressIndex, second: "INDEX-C"},
		{name: "press_index/unflipped/2pos/R2", mode: protocol.SwapModeTwoRobotPressIndex, legB: true},
		{name: "press_index/unflipped/3pos/R2", mode: protocol.SwapModeTwoRobotPressIndex, second: "INDEX-C", legB: true},
		{name: "press_index/flipped/2pos/R2", mode: protocol.SwapModeTwoRobotPressIndex, flipped: true, legB: true},
		{name: "press_index/flipped/3pos/R2", mode: protocol.SwapModeTwoRobotPressIndex, second: "INDEX-C", flipped: true, legB: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newStampFixture(t, tc.mode, tc.second, tc.flipped, tc.legB)
			for _, p := range f.pickups {
				f.pickUpAt(t, p)
			}
			// The market pickup too — an off-cell location must be inert.
			f.pickUpAt(t, "MARKET-EMPTIES")
			if o := f.departedAt(t); o.Departed {
				t.Errorf("this leg ends ON the cell and must never carry a departure stamp; it departed at one of %v",
					append(f.pickups, "MARKET-EMPTIES"))
			}
			if got := f.departureLogs(); len(got) != 0 {
				t.Errorf("departure log lines = %d, want 0: %v", len(got), got)
			}
		})
	}
}

// TestBinPickedUp_StampIsAboveTheLocationGate pins the placement. The gate
// returns early unless location == the process node's CoreNodeName, and
// single_robot's proof step is OUT-STAGING — a node the gate rejects. A stamp
// written below the gate would therefore never fire for the one mode whose
// latent hole this batch closes.
func TestBinPickedUp_StampIsAboveTheLocationGate(t *testing.T) {
	t.Parallel()
	f := newStampFixture(t, protocol.SwapModeSingleRobot, "", false, false)
	// The step-7 record first: this case is about WHERE the stamp sits relative
	// to the location gate, and a leg held back by the unrecorded-placement arm
	// would never reach the question.
	f.recordPlacement(t)

	f.pickUpAt(t, "OUT-STAGING")

	if o := f.departedAt(t); !o.Departed {
		t.Fatal("no departure stamped for a pickup at OUT-STAGING — the stamp is below the location gate, " +
			"which only admits the process node")
	}
	// And the gate itself still ran and still refused: the handler must have
	// logged its "not our slot" line for this same event.
	var refused bool
	for _, l := range *f.logs {
		if strings.Contains(l, "not our slot") {
			refused = true
		}
	}
	if !refused {
		t.Error("the location gate did not run for this event — the stamp must be inserted ABOVE it, not instead of it")
	}
}

// TestBinPickedUp_UnprovableShapeSaysSo pins the fail-closed sentence. No
// shipping builder emits a cell-dropoff-then-off-cell-tail leg — the walker is
// what keeps it that way — so this drives the handler with one directly.
func TestBinPickedUp_UnprovableShapeSaysSo(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobot, "")
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	leg := mkSwapLeg(t, db, nodeID, "uuid-unprovable", []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "MARKET-EMPTIES"},
		{Action: protocol.ActionDropoff, Node: "PRESS"},
		{Action: protocol.ActionPickup, Node: "MARKET-EMPTIES"},
		{Action: protocol.ActionDropoff, Node: "MARKET"},
	}, "")

	eng.HandleBinPickedUp(leg.UUID, 0, "MARKET-EMPTIES")

	o, err := db.GetOrder(leg.ID)
	testutil.MustNoErr(t, err, "re-read order")
	if o.Departed {
		t.Error("a leg whose departure cannot be proved must NOT be stamped — fail closed, wait for terminal")
	}
	var said bool
	for _, l := range logs {
		if strings.Contains(l, "departure unprovable") {
			said = true
		}
	}
	if !said {
		t.Errorf("a cell sitting on an unprovable leg gets no sentence explaining why; logs=%v", logs)
	}
}

// TestBinPickedUp_KanbanOrderIsSilent pins the common case. An order with no
// process node has no cell to leave; the stamp must be a no-op AND say
// nothing, or every kanban pickup in the plant writes a log line.
func TestBinPickedUp_KanbanOrderIsSilent(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	var logs []string
	eng.logFn = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	id, err := db.CreateOrder("uuid-kanban", "retrieve", nil, false, 1, "SOMEWHERE", "", "", "", false, "WIDGET-A")
	testutil.MustNoErr(t, err, "create kanban order")
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "get kanban order")

	eng.HandleBinPickedUp(o.UUID, 0, "MARKET-EMPTIES")

	for _, l := range logs {
		if strings.Contains(l, "departure") || strings.Contains(l, "DEPARTED") {
			t.Errorf("a kanban order produced a departure log line: %q", l)
		}
	}
}

// TestBinPickedUp_HoldsTheCellUntilThePlacementIsRecorded is the case the whole
// conjunction exists for, and its second half is the escape.
//
// single_robot places the fresh carrier on the line at step 7 and lifts the
// spent one off OutboundStaging at step 8. Step 8 is its last cell step, so the
// old rule stamped it departed there — hiding the leg from every admission
// reader while the position it had just filled still read empty to Core, to the
// level sweep and to the downgrade. The leg has left the cell's NODES; it has
// not left the cell's BUSINESS until somebody has recorded what it put down.
//
// So: no stamp at step 8 with nothing recorded, one sentence saying why, and
// the departure completed by the bind whenever it lands. One departure log line
// across both events — the stamp happens once, however it is reached.
func TestBinPickedUp_HoldsTheCellUntilThePlacementIsRecorded(t *testing.T) {
	t.Parallel()
	f := newStampFixture(t, protocol.SwapModeSingleRobot, "", false, false)
	if !f.places {
		t.Fatal("fixture drift: single_robot leg A must leave a bin on PRESS, or this case is vacuous")
	}
	rt, err := f.db.GetProcessNodeRuntime(f.nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveBinID != nil {
		t.Fatalf("fixture drift: active_bin_id = %d, want nil — the step-4 press pickup cleared it, and the "+
			"window under test is exactly the interval where it is nil", *rt.ActiveBinID)
	}

	// Step 8 with nothing recorded. The robot is driving away; the cell is not
	// free, because nobody can say a carrier is on it.
	f.pickUpAt(t, "OUT-STAGING")

	o := f.departedAt(t)
	if o.Departed {
		t.Fatal("the leg departed at step 8 with its own step-7 placement unrecorded — that is the " +
			"double-supply race: the cell reads free, the sweep downgrades to a bare move, and the move " +
			"arrives at a position this leg has already filled")
	}
	if o.CellLeftAt == nil {
		t.Error("cell_left_at was not stamped — the robot HAS left the cell's nodes, and that half of the " +
			"departure is unconditional; without it the bind has nothing to complete")
	}
	if got := f.heldLogs(); len(got) != 1 {
		t.Errorf("held-cell log lines = %d, want exactly 1: %v — a cell shut by this rule is owed a "+
			"sentence naming what it is waiting for", len(got), got)
	}
	if got := f.departureLogs(); len(got) != 0 {
		t.Errorf("departure log lines = %d, want 0: %v", len(got), got)
	}

	// THE ESCAPE. Core's intermediate dropoff lands and the Edge binds the
	// carrier. Without this half the fix is replenishment switched off.
	f.recordPlacement(t)

	o = f.departedAt(t)
	if !o.Departed || o.DepartedAt == nil {
		t.Fatalf("the placement was recorded and the leg still has not departed (departed=%v at=%v); logs=%v — "+
			"the cell would now stay shut for the whole supermarket trip", o.Departed, o.DepartedAt, *f.logs)
	}
	if protocol.IsTerminal(o.Status) {
		t.Fatal("fixture drift: the leg must still be non-terminal — a departure that only arrives with " +
			"terminal proves nothing")
	}
	if got := f.departureLogs(); len(got) != 1 {
		t.Errorf("departure log lines = %d, want exactly 1 across both events: %v", len(got), got)
	}

	// A replayed bind (Core restarts and re-fires the FINISHED block) must not
	// stamp twice or speak twice.
	first := *o.DepartedAt
	f.recordPlacement(t)
	again := f.departedAt(t)
	if again.DepartedAt == nil || !again.DepartedAt.Equal(first) {
		t.Errorf("departed_at moved on a replayed bind: %v then %v", first, again.DepartedAt)
	}
	if got := f.departureLogs(); len(got) != 1 {
		t.Errorf("departure log lines after a replayed bind = %d, want 1: %v", len(got), got)
	}
}
