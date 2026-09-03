package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// SwapDispatch describes the per-mode complex-order dispatch shape — what
// step lists, in what arity, with what flags. Direction-agnostic: produce
// and consume both consume this for sequential / single_robot / two_robot /
// two_robot_press_index. Modes that produce no complex orders (produce
// simple = ingest only; consume simple = bare move) bypass this entirely
// and the per-direction planner handles them.
//
// The robot doesn't care whether the bin is filling or emptying; the
// choreography is the same. SwapDispatch enforces that by being the single
// source of truth for "given this swap mode, which step lists at which
// arity with which flags?" — the produce and consume planners both call
// BuildSwapDispatch instead of duplicating the switch.
type SwapDispatch struct {
	CycleMode protocol.SwapMode

	// ProcessNode is the line node both legs belong to (= claim.CoreNodeName).
	// Threaded into ComplexOrderRequest.ProcessNode so Core can pick the
	// line bin for order.BinID at claim time and at release-time fallback.
	ProcessNode string

	StepsA []protocol.ComplexOrderStep
	// DeliveryNodeA is Leg A's actual final dropoff, derived from StepsA — never
	// assumed to be the process node. It was previously hardcoded to
	// claim.CoreNodeName, which is a lie for any mode whose A-leg ends somewhere
	// else: a press-index R1 ends at the paired INDEX node, and a single-robot
	// A-leg ends at the outbound destination. Storing the press there is what let
	// wiring_delivered's gate bind the wrong bin (HK 2026-07-14).
	DeliveryNodeA string

	// AutoConfirmA / AutoConfirmB are DERIVED, never positional — see
	// confirmPolicy. A leg auto-confirms iff it leaves no bin on the process
	// node; the leg that placed one there is the leg the operator signs for.
	AutoConfirmA bool

	StepsB       []protocol.ComplexOrderStep
	AutoConfirmB bool

	// RequiresActiveSwapGuard true when the apply caller must run
	// guardNoActiveSwap before dispatching. Set by modes that don't tolerate
	// overlapping swaps (two_robot, two_robot_press_index).
	RequiresActiveSwapGuard bool
}

// BuildSwapDispatch validates per-mode required fields and returns the
// dispatch for the four complex-order swap modes. Returns (nil, nil) for
// claim.SwapMode == "simple" / any unrecognised value — the per-
// direction planner is expected to handle those (consume issues a bare
// move order; produce issues an ingest-only order). Pure — no DB or fleet
// calls.
//
// Per-mode field validation matches the inline switches in
// RequestProduceSwap and requestNodeFromClaim verbatim, so error
// messages stay diff-stable across the refactor.
//
// After building the per-mode steps, a produce claim's inbound-source pickup
// (the "fetch a fresh bin from the supermarket" leg) is marked Empty so Core
// sources and claims an EMPTY carrier to fill — the store dual of a consume
// node's full retrieve, and the same intent the simple-retrieve path already
// carries via RetrieveEmpty (changeover_planner). Without it the complex-order
// path delivers a full bin to the press.
func BuildSwapDispatch(node *processes.Node, claim *processes.NodeClaim) (*SwapDispatch, error) {
	disp, err := buildSwapDispatch(node, claim)
	if err != nil || disp == nil {
		return disp, err
	}
	if claim.Role == protocol.ClaimRoleProduce && claim.InboundSource != "" {
		markInboundEmpty(disp.StepsA, claim.InboundSource, "")
		markInboundEmpty(disp.StepsB, claim.InboundSource, "")
	}
	// Derive Leg A's delivery node from its own steps rather than asserting it is
	// the process node. The per-mode builders are the single source of truth for
	// where a leg ends; anything that restates it by hand can drift out of step
	// with them, and did — see DeliveryNodeA.
	disp.DeliveryNodeA = finalDropoff(disp.StepsA)
	return disp, nil
}

// finalDropoff returns the node of the LAST dropoff step, i.e. where a
// single-bin order's bin comes to rest. Shared with wiring_delivered's
// finalDropoffNode so the producer and the delivery gate can't disagree about
// what "this leg's destination" means. Empty when there are no dropoff steps.
func finalDropoff(steps []protocol.ComplexOrderStep) string {
	dest := ""
	for _, s := range steps {
		if s.Action == protocol.ActionDropoff && s.Node != "" {
			dest = s.Node
		}
	}
	return dest
}

// markInboundEmpty flags every pickup at inboundSource as an empty leg. The
// inbound-source pickup is the only leg that fetches a fresh carrier from the
// supermarket; the other pickups move bins already in the swap, so they keep
// their contents and must not be flagged.
//
// carrierFor is what this leg SAYS about the carrier it fetches, and it is
// blank for almost every caller. Empty drops the full-bin content match, so
// this leg no longer hunts a full bin in the empty pool — but Core still
// resolves bin-type compatibility against a payload, and a silent step resolves
// against the ORDER's. That is the right answer everywhere except one place: a
// changeover swap carries the OUTGOING style's payload by necessity (its
// opening pickup has to find the bin on the line), so its refill leg was
// fetching a carrier of the type the cell was leaving (N1-c, sim 2026-08-24).
//
// Blank where the order already answers, so the wire is unchanged for every leg
// that does not need to disagree with it. refillCarrierPayload is the one
// function that decides.
func markInboundEmpty(steps []protocol.ComplexOrderStep, inboundSource, carrierFor string) {
	for i := range steps {
		if steps[i].Action == protocol.ActionPickup && steps[i].Node == inboundSource {
			steps[i].Empty = true
			steps[i].PayloadCode = carrierFor
		}
	}
}

// refillCarrierPayload is what a CHANGEOVER's refill leg should say about the
// carrier it fetches: nothing when the order's own payload already answers, the
// incoming style's payload when it does not.
//
// Saying nothing is not a micro-optimisation. It keeps the wire byte-identical
// for every changeover that does not change carrier type, which is most of
// them, so a Core that predates the field behaves exactly as before except in
// the one case where its answer was wrong anyway.
func refillCarrierPayload(fromClaim, toClaim *processes.NodeClaim) string {
	if toClaim == nil {
		return ""
	}
	if fromClaim != nil && fromClaim.PayloadCode == toClaim.PayloadCode {
		return ""
	}
	return toClaim.PayloadCode
}

// confirmPolicy answers "does this leg need a human receipt?" the only way that
// survives a new swap mode: a leg auto-confirms IFF it leaves no bin ON THE
// PROCESS NODE — the press itself, not the wider cell.
//
// CONFIRM means "a bin is on the machine and I signed for what is in it". The
// leg that put it there is the one the operator signs; a leg that only backfills
// an on-deck index position, or only takes a bin away, self-confirms. That is
// exactly one receipt per cycle in every mode and both flip states — pinned by
// TestEverySwapLegDepartsProvablyAndConfirmsOnPlacement.
//
// AutoConfirmA/AutoConfirmB were positional literals, and position is not what
// the operator signs for. The IndexRobotSupplies flip moves the supermarket trip
// between R1 and R2 without moving the press pickup or the press dropoff, so
// under the flip the leg carrying AutoConfirmA=false became the one ending at
// the SUPERMARKET. Springfield press trial 2026-09-02: the order landed
// `delivered` at the market and sat non-terminal until somebody at the press
// tapped CONFIRM for a tote they could not see, and until they did the card
// showed CONFIRM where REQUEST SWAP belonged.
//
// legPlacesBinAt (swap_leg_role.go) is the same predicate the supply/evac
// classifier, the delivered gate and Core's two dispatch predicates use, so all
// of them answer "which leg supplied the press" identically.
func confirmPolicy(claim *processes.NodeClaim, steps []protocol.ComplexOrderStep) bool {
	if len(steps) == 0 {
		return false
	}
	return !legPlacesBinAt(steps, claim.CoreNodeName)
}

func buildSwapDispatch(node *processes.Node, claim *processes.NodeClaim) (*SwapDispatch, error) {
	switch claim.SwapMode {
	case protocol.SwapModeSequential:
		stepsA := BuildSequentialRemovalSteps(claim)
		return &SwapDispatch{
			CycleMode:   protocol.SwapModeSequential,
			ProcessNode: claim.CoreNodeName,
			StepsA:      stepsA,
			// Falls out of the derived rule: the removal leg places nothing.
			AutoConfirmA: confirmPolicy(claim, stepsA),
		}, nil

	case protocol.SwapModeSingleRobot:
		if claim.InboundStaging == "" || claim.OutboundStaging == "" {
			return nil, fmt.Errorf("node %s: single-robot swap requires inbound and outbound staging nodes", node.Name)
		}
		stepsA := BuildSingleSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:    protocol.SwapModeSingleRobot,
			ProcessNode:  claim.CoreNodeName,
			StepsA:       stepsA,
			AutoConfirmA: confirmPolicy(claim, stepsA),
		}, nil

	case protocol.SwapModeTwoRobot:
		if claim.InboundStaging == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires inbound staging node", node.Name)
		}
		// The EVAC leg's only dropoff, validated alongside the SUPPLY leg's
		// field because both legs come out of one builder and the caller cannot
		// decline half a dispatch. applyConsumePlan / applyProducePlan create
		// the supply leg and SHIP IT TO CORE before they build this one, behind
		// a durable outbox with no compensating cancel — so a field only the
		// evac leg reads is unrecoverable by the time the evac leg fails.
		//
		// Nor can Core catch it: buildStep emits a dropoff with NO NODE when
		// this is blank, and a node-less dropoff is the legitimate deferred
		// destination reserved for dedicated-loader placement
		// (dispatch/complex_steps.go). Core cannot tell a missing config from a
		// deliberate deferral, so this is the last boundary that can.
		//
		// two_robot_press_index has checked this since it was written; this
		// mode never did. Springfield, 2026-06-23 17:34:38: the ALN_001 claim
		// had no outbound_destination, the evac leg went out as
		// [wait ALN_001, pickup ALN_001, dropoff <nowhere>] and never got a Core
		// row, and its supply leg — Core order 2217 — is unpaired there to this
		// day while Edge's sibling_order_id still says the pair is intact.
		if claim.OutboundDestination == "" {
			return nil, fmt.Errorf("node %s: two-robot swap requires outbound destination (the evac leg's dropoff)", node.Name)
		}
		stepsA, stepsB := BuildTwoRobotSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               protocol.SwapModeTwoRobot,
			ProcessNode:             claim.CoreNodeName,
			StepsA:                  stepsA,
			StepsB:                  stepsB,
			AutoConfirmA:            confirmPolicy(claim, stepsA),
			AutoConfirmB:            confirmPolicy(claim, stepsB),
			RequiresActiveSwapGuard: true,
		}, nil

	case protocol.SwapModeTwoRobotPressIndex:
		if claim.PairedCoreNode == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires paired_core_node (back position)", node.Name)
		}
		if claim.OutboundDestination == "" {
			return nil, fmt.Errorf("node %s: two_robot_press_index requires outbound_destination", node.Name)
		}
		stepsR1, stepsR2 := BuildTwoRobotPressIndexSwapSteps(claim)
		return &SwapDispatch{
			CycleMode:               protocol.SwapModeTwoRobotPressIndex,
			ProcessNode:             claim.CoreNodeName,
			StepsA:                  stepsR1,
			StepsB:                  stepsR2,
			AutoConfirmA:            confirmPolicy(claim, stepsR1),
			AutoConfirmB:            confirmPolicy(claim, stepsR2),
			RequiresActiveSwapGuard: true,
		}, nil
	}
	return nil, nil
}
