package dispatch

import (
	"encoding/json"

	"shingo/protocol"
)

// The two step-shape predicates that decide a swap leg's ROLE. Both read the
// leg's own steps, which are the only thing that actually describes what it
// does. They exist because role used to be inferred from geometry —
// `DeliveryNode != ProcessNode` — and that inference is wrong:
//
//   - Core's order.DeliveryNode is not the Edge's. Edge never sends one
//     (ComplexOrderRequest has no such field); Core DERIVES it from the steps
//     via extractEndpoints, which takes the last pickup-OR-dropoff. For a
//     press-index R1 that is the index node, so R1 has always read as "delivers
//     away from the line" — i.e. as a removal leg needing a sibling's help —
//     even though it fetches its own replacement carrier.
//   - A 3-position press-index R2 drops a bin ON the line and then carries on to
//     re-index the next position, so it too ends away from the line while being
//     the leg that supplies it.
//
// Ask the steps what the leg does to the LINE BIN. Nothing else answers it.
//
// Relationship to legPlacesBinAt in shingo-edge/engine/swap_leg_role.go: the
// two predicates are COMPLEMENTS, not copies. Edge's answers "does this leg
// leave a bin at the node" (true = supply); legTakesLineBin below answers "does
// it lift the node's bin and not put one back" (true = evac). Run the same leg
// through both and they disagree by construction — two_robot B is TRUE here and
// false there, press-index R2 is false here and true there. Their verification
// tables agree on every shape; the predicates are opposite readings of it. This
// header used to claim "same question, same answer, both sides of the wire",
// which would make either one a drop-in for the other. It is not.

// ── THE SIX-MODE CENSUS: WHERE EVERY SWAP LEG LANDS ───────────────────────
//
// The per-predicate tables below were verified against THREE modes of six.
// sequential, simple and manual_swap had never been checked, and the omission
// hid a third shape neither predicate names. This is all six, across BOTH
// populations that reach Core as complex orders — steady state and changeover —
// because a mode's legs differ between them.
//
// CITED BY SYMBOL, NOT BY LINE, and the reason is this census's own history. It
// was written with a file:line per row, and every one of them rotted: re-derived
// 2026-09-10, the changeover builders had moved +43 lines, the tooling helper
// +88, the keep-staged trio +82 to +87, and the two swap_dispatch.go references
// had gone the OTHER way by 58 and 71. A correction that called the drift
// "uniformly +43" was itself wrong — it matched two rows of a dozen. Line
// numbers drift silently, en masse, and in both directions; a symbol either
// resolves or it does not.
//
// T = legTakesLineBin (a pure evac). P = legPlacesLineBin (a pure filler).
// Builders live in shingo-edge/engine/material_orders.go unless the row names
// another file.
//
// STEADY STATE — BuildSwapDispatch (shingo-edge/engine/swap_dispatch.go):
//
//	simple                  no complex order at all: buildSwapDispatch returns
//	                        (nil, nil) and consume_plan.go issues a bare move,
//	                        which carries no ProcessNode.           n/a
//	manual_swap             same — buildSwapDispatch. And the level sweep skips
//	                        these nodes outright (demand_reconciler.go,
//	                        sweepNodeLevel): a loader/unloader is forklift-managed
//	                        staging, not a cell with a level.        n/a
//	single_robot            BuildSingleSwapSteps: pickup(LINE) AND dropoff(LINE)
//	                                                    T=false P=false  <- both
//	two_robot A (supply)    BuildTwoRobotSwapSteps orderA: dropoff(LINE)
//	                                                    T=false P=TRUE
//	two_robot B (evac)      BuildTwoRobotSwapSteps orderB: pickup(LINE)
//	                                                    T=TRUE  P=false
//	press-index R1          BuildTwoRobotPressIndexSwapSteps R1: pickup(LINE)
//	                                                    T=TRUE  P=false
//	press-index R2          BuildTwoRobotPressIndexSwapSteps R2: dropoff(LINE)
//	                                                    T=false P=TRUE
//	sequential A (removal)  BuildSequentialRemovalSteps: pickup(LINE)
//	                                                    T=TRUE  P=false
//	sequential B (backfill) BuildSequentialBackfillSteps: dropoff(LINE)
//	                                                    T=false P=TRUE
//
// Sequential needed no change and that is worth having checked: its removal leg
// is a pure evac and its backfill a pure filler, so the two predicates already
// agree about both. The press-index rows hold under BOTH sides of the
// IndexRobotSupplies flip — the flip moves the supermarket fetch between the
// legs, and neither the line pickup nor the line dropoff moves with it.
//
// CHANGEOVER — BuildSwapChangeoverSteps / BuildEvacuateChangeoverSteps. This is
// where "simple" and "manual_swap" DO reach Core despite building nothing in
// steady state: both switches route every unrecognised mode through the default
// arm into the single-robot shape.
//
//	single_robot | simple | manual_swap | unrecognised — the default arm,
//	buildSingleRobotChangeoverSwap:
//	  stage leg             BuildStageSteps: no step at the line at all
//	                                                    T=false P=false
//	  swap/evac leg         stepsB: pickup(LINE) AND dropoff(LINE)
//	                                                    T=false P=false  <- both
//	two_robot supply        buildTwoRobotChangeoverSwap stepsA: dropoff(LINE)
//	                                                    T=false P=TRUE
//	two_robot evac          buildTwoRobotChangeoverSwap stepsB: pickup(LINE)
//	                                                    T=TRUE  P=false
//	press-index R1          buildPressIndexChangeoverSwap R1: pickup(LINE),
//	                        refills the BACK position   T=TRUE  P=false
//	press-index R2          buildPressIndexChangeoverSwap R2: dropoff(LINE)
//	                                                    T=false P=TRUE
//	press_position          buildPressIndexPerPositionSwap: pickup(pos) AND
//	  (per-position fan-out) dropoff(pos); the order's ProcessNode IS pos
//	                        (changeover_planner.go takes diff.CoreNodeName)
//	                                                    T=false P=false  <- both
//	sequential swap         buildSequentialPerPositionSwap: pickup(pos) AND
//	                        dropoff(pos)                T=false P=false  <- both
//	sequential evacuate     buildSequentialPerPositionEvacuate, via
//	                        buildToolingEvacSteps       T=false P=false  <- both
//	press tooling evac      the same helper via changeover_tooling.go, whose spec
//	                        is complexSpecWithPayload(position, position, …) — so
//	                        pos IS the ProcessNode      T=false P=false  <- both
//	tooling carry-over      pickup(position) → dropoff(staging) → wait →
//	                        pickup(staging) → dropoff(position)
//	                        (changeover_tooling.go)     T=false P=false  <- both
//	keep-staged evac        BuildKeepStagedEvacSteps: pickup(LINE)
//	                                                    T=TRUE  P=false
//	keep-staged deliver     BuildKeepStagedDeliverSteps: dropoff(LINE)
//	                                                    T=false P=TRUE
//	keep-staged combined    BuildKeepStagedCombinedSteps: dropoff(LINE); its
//	                        pickups are all at staging or the source
//	                                                    T=false P=TRUE
//
// ── THE THIRD SHAPE, AND WHAT READS IT ────────────────────────────────────
//
// Every "<- both" row is SELF-CONTAINED: one robot lifts the line's bin and
// sets a fresh one down in the same trip. It is neither a pure evac nor a pure
// filler, so it answers false to BOTH predicates, and it is not unique to
// single_robot — five changeover builders produce it, running at both plants.
//
// Two sites read that false, and they mean different things by it:
//
//   - THE INDEX ANTI-COLLISION ARM asked placesLine && !takesLine, because it
//     was deciding whether this leg needed a SIBLING to clear the line first. A
//     self-contained leg clears it itself, so false was the right answer. That
//     arm is DELETED — dispatch only ever parked the robot, so it was gating a
//     moment where no bin could move — and legPlacesLineBin now has exactly one
//     production reader, the next row.
//   - allocator.go's moot-disposition arm excludes a leg from "the work is void" with
//     !legPlacesLineBin, so a self-contained leg is not excluded and a moot
//     disposition SKIPS it terminally, where a two_robot supply leg in the same
//     physical situation parks on waiting_for_material.
//
// THAT SECOND ONE LOOKS LIKE A DEFECT AND IS NOT, because of what it takes to
// reach the arm. Every self-contained row has a NON-RELAY pickup at the process
// node (the line pickup precedes the line dropoff in all of these builders, so
// complexPickups never marks it potentialRelay), and the arm requires
// len(assigned) == 0 && !anyMissWithBins — every distinct need missed, at a node
// holding nothing. So the arm is UNREACHABLE for a self-contained leg unless the
// line is also empty. With a bin anywhere, the leg reserves it, holds partials,
// and falls to reserveHolding.
//
// In the one state that does reach it — line empty AND every source empty — the
// skip is both correct and self-healing. There is no bin to lift, so the leg's
// premise is gone; the skip terminalises, which releases the partials and lets
// the level keeper ask again on its next sweep; and that next ask re-plans
// against an empty head position, where consume_plan.go's node-empty arm downgrades the swap
// to a plain delivery move. A park would do the opposite: waiting_for_material
// is non-terminal, ListActiveByProcessNode counts it, and the keeper's node
// dedup then goes quiet for as long as the order sits there.
//
// TestReserve_SelfContainedLegWithALineBinAlreadyParks pins the reachability
// half, so the exposure cannot be re-derived larger than it is.

// legTakesLineBin reports whether the leg lifts the line node's bin and does not
// put one back: a pickup at processNode with no dropoff at processNode. That is
// the evac shape.
//
// Verified against the Edge builders (material_orders.go):
//
//	two_robot A            dropoff(LINE), no pickup(LINE)         → false (supply)
//	two_robot B            pickup(LINE), no dropoff(LINE)         → TRUE  (evac)
//	press-index R1 (2&3)   pickup(LINE), dropoff(OUT/IN/B|C)      → TRUE  (evac)
//	press-index R2 (2&3)   pickup(B), dropoff(LINE)               → false (supply)
//	single_robot           pickup(LINE) AND dropoff(LINE)         → false (self-contained)
//	sequential removal     pickup(LINE), dropoff(OUT)             → TRUE  (evac, but sibling-less)
//
// Three modes of six. The census above carries all six, across steady state and
// changeover both.
func legTakesLineBin(steps []resolvedStep, processNode string) bool {
	if processNode == "" {
		return false
	}
	tookBin := false
	for _, s := range steps {
		if s.Node != processNode {
			continue
		}
		switch s.Action {
		case protocol.ActionPickup:
			tookBin = true
		case protocol.ActionDropoff:
			return false // it puts a bin back on the line — not an evac
		}
	}
	return tookBin
}

// legPlacesLineBin reports whether the leg drives a bin ONTO the line node and
// does not also lift one off: a dropoff at processNode with no pickup at
// processNode. That is the supply/index shape — the leg that fills the shared
// line position, and so must not commit until whatever occupies that position
// has been cleared. The mirror of legTakesLineBin, same both sides of the wire.
//
// Verified against the Edge builders (material_orders.go):
//
//	two_robot A (supply)   dropoff(LINE), no pickup(LINE)         → TRUE  (filler)
//	press-index R2 (2&3)   pickup(B), dropoff(LINE)               → TRUE  (filler/index)
//	press-index R1 (2&3)   pickup(LINE), dropoff(OUT/IN/B|C)      → false (evac)
//	two_robot B (evac)     pickup(LINE), no dropoff(LINE)         → false (evac)
//	single_robot           pickup(LINE) AND dropoff(LINE)         → false (self-contained)
//
// Three modes of six. The census above carries all six, and names the two sites
// that read this predicate's false for different reasons.
func legPlacesLineBin(steps []resolvedStep, processNode string) bool {
	if processNode == "" {
		return false
	}
	placedBin := false
	for _, s := range steps {
		if s.Node != processNode {
			continue
		}
		switch s.Action {
		case protocol.ActionDropoff:
			placedBin = true
		case protocol.ActionPickup:
			return false // it also lifts the line's bin — evac / self-contained, not a pure filler
		}
	}
	return placedBin
}

// decodeSteps parses a stored steps_json. Returns nil (and false) when the steps
// cannot be read — callers must decide what "unknown shape" means for them
// rather than silently treating it as a particular role.
func decodeSteps(stepsJSON string) ([]resolvedStep, bool) {
	if stepsJSON == "" {
		return nil, false
	}
	var steps []resolvedStep
	if err := json.Unmarshal([]byte(stepsJSON), &steps); err != nil {
		return nil, false
	}
	return steps, true
}
