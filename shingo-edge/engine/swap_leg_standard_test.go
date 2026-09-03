package engine

import (
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// TestEverySwapLegDepartsProvablyAndConfirmsOnPlacement IS THE STANDARD.
//
// ── THE RULE IT ENFORCES ──────────────────────────────────────────────────
//
// A cell is done when every bin it needs is on its nodes. Done = confirmable =
// ready to order. A robot carrying a bin AWAY from the cell is not the cell's
// business, whatever mode built the leg.
//
// Two consequences, and BOTH are derived from a leg's steps against the claim's
// cell set — never from claim.SwapMode:
//
//  1. DEPARTURE IS PROVABLE. Every leg's last cell step is either a PICKUP (the
//     fleet confirms it via BinPickedUp) or the leg's own final step (terminal
//     covers it). Those are the only two proof events there are.
//  2. CONFIRM BELONGS TO THE LEG THAT PLACED ON THE PRESS. Exactly the legs that
//     leave a bin on claim.CoreNodeName are confirm-required — and there is
//     EXACTLY ONE of them per cycle, in every mode and both flip states.
//
// ── WHY A WALKER AND NOT A TABLE ──────────────────────────────────────────
//
// A table asserts what its author already believed. This walks
// ConfigurableSwapModes × both flip states × 2- and 3-position, so a swap mode
// added to that list is a mode this test covers on the day it is added — which
// is the only way "every mode must work" can be a property rather than a
// promise. The positional confirm literals this replaces were correct for
// two_robot and wrong for press-index, and nothing was watching.
func TestEverySwapLegDepartsProvablyAndConfirmsOnPlacement(t *testing.T) {
	t.Parallel()

	for _, mode := range protocol.ConfigurableSwapModes() {
		for _, flipped := range []bool{false, true} {
			for _, second := range []string{"", "STANDARD-C"} {
				name := string(mode)
				if flipped {
					name += "/flipped"
				}
				if second != "" {
					name += "/3pos"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					claim := standardClaim(mode, second, flipped)
					disp, err := BuildSwapDispatch(&processes.Node{ID: 1, Name: claim.CoreNodeName}, claim)
					if err != nil {
						t.Fatalf("BuildSwapDispatch: %v", err)
					}
					if disp == nil {
						// manual_swap issues no complex orders — it uses a
						// multi-order queue and has no swap choreography to
						// depart from. Nothing to walk.
						return
					}

					cell := cellSetFor(claim)
					legs := []struct {
						label string
						steps []protocol.ComplexOrderStep
						auto  bool
					}{
						{"leg A", disp.StepsA, disp.AutoConfirmA},
						{"leg B", disp.StepsB, disp.AutoConfirmB},
					}
					if mode == protocol.SwapModeSequential {
						// SEQUENTIAL'S OTHER HALF IS NOT IN THE DISPATCH. Its
						// backfill leg is minted later by handleSequentialBackfill
						// when the removal reaches in_transit, and it never passes
						// through BuildSwapDispatch — so a walker that only read
						// the dispatch would certify half a cycle. It runs at the
						// cell like any other leg and is held to the same standard.
						// createComplexOrder gives it autoConfirm=false.
						legs = append(legs, struct {
							label string
							steps []protocol.ComplexOrderStep
							auto  bool
						}{"backfill leg (auto-created)", BuildSequentialBackfillSteps(claim), false})
					}
					receipts := 0
					for _, leg := range legs {
						if len(leg.steps) == 0 {
							continue
						}
						assertDepartureIsProvable(t, leg.label, mode, leg.steps, cell)
						assertConfirmFollowsPlacement(t, leg.label, leg.steps, claim.CoreNodeName, leg.auto)
						if legPlacesBinAt(leg.steps, claim.CoreNodeName) {
							receipts++
						}
					}
					// EXACTLY ONE, not "at least one". Zero means the cycle puts
					// nothing on the machine and nobody ever counts a bin in; two
					// means the operator taps twice for one swap, which is what the
					// whole-cell rule cost unflipped press-index (R1's index
					// backfill alongside R2's press placement).
					if receipts != 1 {
						t.Errorf("this cycle asks for %d operator receipts; exactly one leg must leave a bin "+
							"on %s. Zero means nothing lands on the machine; more than one means the operator "+
							"signs twice for one swap.\nA: %v\nB: %v",
							receipts, claim.CoreNodeName, disp.StepsA, disp.StepsB)
					}
				})
			}
		}
	}
}

// assertDepartureIsProvable is consequence 1. The message names the builder and
// says what the two honest fixes are, because the person who trips this will be
// writing a new step builder and will not have read this file.
func assertDepartureIsProvable(t *testing.T, label string, mode protocol.SwapMode,
	steps []protocol.ComplexOrderStep, cell map[string]bool) {
	t.Helper()

	idx, touches := lastCellStep(steps, cell)
	if !touches {
		t.Errorf("%s of %s never touches a cell node. Every leg of a swap works the cell it belongs "+
			"to; a leg that does not is either mis-built or belongs to another cell.\nsteps: %v",
			label, mode, steps)
		return
	}

	kind, node, ok := legDepartsAt(steps, cell)
	if !ok {
		t.Errorf("%s of %s cannot prove it has left the cell.\n"+
			"Its last cell step is %d (%+v) — a step that is neither a pickup nor the leg's final step, "+
			"so neither of the two proof events (BinPickedUp, IsTerminal) can speak about it. The cell "+
			"would stay shut until this leg went terminal, costing a whole swap cycle of press time.\n"+
			"EITHER end the leg at the cell, OR make its last cell step a pickup, OR teach legDepartsAt "+
			"a new proof event and add it to the standard in docs/order-lifecycle.md.\nsteps: %v",
			label, mode, idx, steps[idx], steps)
		return
	}

	// The stamp keys on (order_uuid, location), so a departure node that this
	// leg picks up from TWICE is ambiguous — the first BinPickedUp there would
	// stamp, and MarkDeparted is stamp-once, so the leg would depart early.
	// No builder does this today; the pin is what stops one starting.
	if kind == departureKindPickup {
		pickups := 0
		for _, s := range steps {
			if s.Action == protocol.ActionPickup && s.Node == node {
				pickups++
			}
		}
		if pickups != 1 {
			t.Errorf("%s of %s departs at %q, but picks up from that node %d times. The departure stamp "+
				"keys on (order, location) and stamps once, so the FIRST pickup there would win and the "+
				"leg would read as departed while it is still working the cell. Give the departure step "+
				"a node this leg lifts from exactly once.\nsteps: %v",
				label, mode, node, pickups, steps)
		}
	}
}

// assertConfirmFollowsPlacement is consequence 2, asked about the PROCESS NODE.
// Not the whole cell: an index backfill leaves a bin on an on-deck position, and
// nobody signs for a tote that has not reached the machine yet.
func assertConfirmFollowsPlacement(t *testing.T, label string,
	steps []protocol.ComplexOrderStep, processNode string, auto bool) {
	t.Helper()

	places := legPlacesBinAt(steps, processNode)
	if places && auto {
		t.Errorf("%s leaves a bin on %s but AUTO-CONFIRMS. CONFIRM is the count receipt for the bin "+
			"that just went on the machine; auto-confirming it closes the order with nobody having "+
			"looked at what is in it.\nsteps: %v", label, processNode, steps)
	}
	if !places && !auto {
		t.Errorf("%s leaves no bin on %s but asks for an operator CONFIRM. Nobody at the press can see "+
			"this tote come to rest — it is going to the supermarket or to an on-deck position — so the "+
			"tap can only ever be a guess, and until it happens the cell reads as busy.\nsteps: %v",
			label, processNode, steps)
	}
}

// TestEveryChangeoverLegDepartsProvably extends consequence 1 over the OTHER
// two builders that emit cell legs.
//
// The departure stamp fires in HandleBinPickedUp on any order with a process
// node and a claim — it has no idea whether the leg came from a steady-state
// swap or a changeover. Today a changeover leg is additionally held by the
// changeover participant guard, so an unprovable one would not wedge a cell;
// but "a new builder inherits departure for free" is only true if the standard
// covers every builder that emits a cell leg, and these two do.
//
// Confirm is NOT asserted here: the changeover builders still carry positional
// AutoConfirmA/B literals (material_orders.go), which is a boarded item and not
// this test's subject.
func TestEveryChangeoverLegDepartsProvably(t *testing.T) {
	t.Parallel()

	// pressPositionSwapMode is appended BY HAND, and it is the one arm of these
	// two builders that ConfigurableSwapModes cannot reach: it is synthesized by
	// the press-index different-bin-type fan-out and UpsertClaim rejects it, so
	// no configured claim ever carries it. It still emits a cell leg the stamp
	// fires on, which is the only thing that decides whether it belongs here.
	modes := append(protocol.ConfigurableSwapModes(), pressPositionSwapMode)
	for _, mode := range modes {
		for _, second := range []string{"", "STANDARD-C"} {
			name := string(mode)
			if second != "" {
				name += "/3pos"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				// ONE GEOMETRY, TWO STYLES. from and to are the same physical
				// cell — a changeover swaps the payload, not the node names —
				// so cellSetFor is the same set either way, which is what makes
				// the question well-posed: the stamp resolves the claim through
				// findActiveClaim and does not know which of the two it got.
				from := standardClaim(mode, second, false)
				to := standardClaim(mode, second, false)
				to.PayloadCode = "WIDGET-B"
				cell := cellSetFor(from)

				for _, situation := range []struct {
					label string
					disp  ChangeoverDispatch
				}{
					{"swap", BuildSwapChangeoverSteps(from, to, from.PairedCoreNode, from.CoreNodeName)},
					{"evacuate", BuildEvacuateChangeoverSteps(from, to, from.PairedCoreNode, from.CoreNodeName)},
				} {
					legs := map[string][]protocol.ComplexOrderStep{
						situation.label + " A": situation.disp.StepsA,
						situation.label + " B": situation.disp.StepsB,
					}
					if r := situation.disp.Roles; r != nil {
						legs[situation.label+" supply"] = r.supply.steps
						legs[situation.label+" evac"] = r.evac.steps
					}
					walked := 0
					for label, steps := range legs {
						if len(steps) == 0 {
							continue
						}
						walked++
						assertDepartureIsProvable(t, label, mode, steps, cell)
					}
					if walked == 0 {
						t.Errorf("%s changeover for %s produced no legs at all, so this mode is walked "+
							"vacuously. Either the claim fixture is missing a field the builder needs, or "+
							"the builder has an arm that returns nothing", situation.label, mode)
					}
				}
			})
		}
	}
}

// standardClaim is one cell configured for whichever mode is being walked. All
// six position/endpoint fields are always set: a builder that needs one and
// finds it blank returns no steps, and a walker that silently skipped such a
// mode would be a walker that covers nothing.
func standardClaim(mode protocol.SwapMode, secondPaired string, flipped bool) *processes.NodeClaim {
	return &processes.NodeClaim{
		Role:                 protocol.ClaimRoleProduce,
		SwapMode:             mode,
		PayloadCode:          "WIDGET-A",
		UOPCapacity:          100,
		CoreNodeName:         "STANDARD-A",
		PairedCoreNode:       "STANDARD-B",
		SecondPairedCoreNode: secondPaired,
		InboundSource:        "STANDARD-MARKET-IN",
		InboundStaging:       "STANDARD-IN-STAGE",
		OutboundStaging:      "STANDARD-OUT-STAGE",
		OutboundDestination:  "STANDARD-MARKET-OUT",
		IndexRobotSupplies:   flipped,
	}
}

// TestTheStandardCoversEveryConfigurableMode is the walker's own guard. A mode
// whose builder returns no steps — a missing claim field, a switch arm that
// falls through — makes the walker above pass vacuously for that mode, which is
// the one failure a walker cannot report on itself.
func TestTheStandardCoversEveryConfigurableMode(t *testing.T) {
	t.Parallel()
	for _, mode := range protocol.ConfigurableSwapModes() {
		if mode == protocol.SwapModeManualSwap {
			// The one deliberate exemption: manual_swap issues no complex
			// orders at all (multi-order queue, no swap choreography), so
			// BuildSwapDispatch returns nil for it by design.
			d, err := BuildSwapDispatch(&processes.Node{ID: 1, Name: "STANDARD-A"}, standardClaim(mode, "", false))
			if err != nil || d != nil {
				t.Errorf("manual_swap is exempt because it produces NO dispatch; it produced %+v (err %v). "+
					"If that changed, remove the exemption rather than the coverage", d, err)
			}
			continue
		}
		for _, flipped := range []bool{false, true} {
			for _, second := range []string{"", "STANDARD-C"} {
				d, err := BuildSwapDispatch(&processes.Node{ID: 1, Name: "STANDARD-A"},
					standardClaim(mode, second, flipped))
				if err != nil {
					t.Errorf("mode %s (flipped=%v, second=%q) fails to build: %v — the standard walks it "+
						"vacuously", mode, flipped, second, err)
					continue
				}
				if d == nil || len(d.StepsA) == 0 {
					t.Errorf("mode %s (flipped=%v, second=%q) produces no leg A, so the standard walks it "+
						"vacuously. Either the claim fixture is missing a field this builder needs, or the "+
						"builder has an arm that returns nothing", mode, flipped, second)
				}
			}
		}
	}
}
