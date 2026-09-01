package engine

import (
	"shingo/protocol"
	"shingoedge/store/processes"
)

// Material movement step builders.
// These are pure functions that return ComplexOrderStep sequences from a
// StyleNodeClaim's routing config. They are used by both routine
// replenishment and changeover order construction.

// buildStep constructs a single ComplexOrderStep. Core auto-detects whether
// the node is a group (NGRP) and resolves it. Empty node triggers global
// fallback via payloadCode.
func buildStep(action, node string) protocol.ComplexOrderStep {
	if node != "" {
		return protocol.ComplexOrderStep{Action: action, Node: node}
	}
	return protocol.ComplexOrderStep{Action: action}
}

// stationWait builds a wait THIS STATION owns and advances — every wait in this
// file is one. The swap choreography's gates are all station facts: the line has
// cleared, the tooling is done, the operator is ready, the changeover may cut
// over. Core cannot observe any of them, which is precisely why they are waits.
//
// ── IT IS A CONSTRUCTOR SO THE NEXT ONE CANNOT FORGET ─────────────────────
//
// These were twenty-two raw `{Action: "wait", ...}` literals. Every one carried
// its ownership implicitly, in the fact that Core's splice had not touched it —
// which the Edge could not see and the HMI could not render. A twenty-third
// literal would have been added unstamped without anything noticing, so the
// stamp lives in one function and TestEveryEdgeAuthoredWaitIsStamped walks every
// builder's output to keep it that way.
//
// node may be empty: a bare wait is a split point with no drive-to (the shared
// "tooling done" / "ready" gates), and it is no less station-owned for it.
func stationWait(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{
		Action:   "wait",
		Node:     node,
		WaitKind: waitKindStation,
	}
}

// waitKindStation mirrors dispatch.WaitKindStation. Edge cannot import Core, so
// the value is duplicated and pinned by TestWaitKindStation_MatchesCore, which
// fails if the two ever drift — the same shape as every other cross-module
// constant in this repo.
const waitKindStation = "station"

// stagingDropoff builds a dropoff at a STAGING node — one that holds a single
// bin and must be reserved before a robot is sent to it.
//
// ── WHY THE STATION HAS TO SAY SO ─────────────────────────────────────────
//
// Core gates its destination checks on node ROLE, and a staging node fails that
// test: it is seeded as a station with no parent, so Core reads it as "not a
// storage slot" and BOTH destination gates stand down. Nothing reserves it,
// nothing checks it is free, and a second order is free to take it.
//
// Core cannot fix that by looking harder. Every station shares one node type,
// and the inbound/outbound staging designation lives HERE, in the cell config,
// which Core does not have. We are the only party that knows.
//
// Undeclared, nothing asks whether the node is free: a second order takes it
// while the first robot is on its way, and that robot arrives holding a bin it
// cannot put down. (A Springfield 2026-08-12 attribution stood here; §R.112's
// plant queries falsified it — protocol.ComplexOrderStep.ExclusiveSlot carries
// the one full record. The gap is reachable on its own terms.)
//
// ── A CONSTRUCTOR, FOR THE REASON stationWait IS ONE ──────────────────────
//
// There are eight of these across this file and they are easy to add a ninth to.
// An untagged one is not a compile error and not a test failure anywhere near
// itself — it is a robot standing at an occupied node weeks later, which is
// exactly how the first eight came to be untagged. TestEveryStagingDropoffIsDeclared
// walks every builder's output and keeps it that way.
//
// NOT FOR LINE NODES. Declaring a line dropoff exclusive would gate a supply leg
// on a node its sibling evac is on the way to clear, which re-creates the
// deadlock Core's 2b05dce fixed. Line dropoffs stay plain literals.
func stagingDropoff(node string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{
		Action:        "dropoff",
		Node:          node,
		ExclusiveSlot: true,
	}
}

// BuildReleaseSteps builds steps to remove material from a node and send it
// to the configured outbound destination.
func BuildReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
}

// BuildStagedReleaseSteps is BuildReleaseSteps with a leading wait-with-node
// at the source. The robot drives to the lineside, parks, and the order
// reaches status=staged before pickup — giving the operator a chance to
// inspect the partial bin and enter the remaining count at the standard
// release-prompt dialog. After release, the bin is picked up and routed to
// the outbound destination unattended.
//
// Used by the drop planner so a partial bin removed during a changeover gets
// the same release-with-count gate the swap-evac path uses, rather than
// silently disappearing. EvacuateNode (the manual EMPTY FOR TOOL CHANGE path)
// still uses BuildReleaseSteps directly because the operator has already
// confirmed the partial count at the EMPTY click; no second confirmation
// needed.
func BuildStagedReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName),
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
}

// refillPickup is the ONE way to express "fetch a fresh carrier from the
// inbound source". Every builder that opens a leg at InboundSource must use it.
//
// ── WHY A CONSTRUCTOR, AND NOT A CALL AFTER THE STEP LIST ─────────────────
//
// The Empty flag was set by a markInboundEmpty call placed AFTER the builder's
// step literal, and each builder had to remember it on its own. Eight builders
// emit an inbound pickup; five of them forgot. The forgetting is invisible:
// an unflagged pickup is not a compile error and not a test failure anywhere
// near itself — it is an order that asks Core for a FULL bin of the incoming
// part number inside the empty-carrier supermarket, which no inventory can
// satisfy, so the order parks on "no bin of requested payload" and retries
// until a person cancels it. That is Hopkinsville PLN_03/PLN_06 on 2026-08-26
// and 2026-08-27: buildTwoRobotChangeoverSwap has never carried the call since
// it was written (e02a526b, 2026-05-03) and has never produced a working order.
//
// git log -S markInboundEmpty tells the rest: ten commits, most of them fixes
// re-adding the same call to one more builder after it stalled a floor —
// b62e1552 (sequential backfill), 3832e88d (single_robot backfill), 30630c70
// (press-index). Same defect, seven times, because the invariant lived at the
// leaf instead of at the boundary.
//
// This is stagingDropoff's remedy applied to the sibling flag: make the wrong
// step hard to write, so the mistake is "used the wrong constructor" — visible
// in review, at the line where the step is built — rather than "omitted a call
// eight lines below". TestEveryChangeoverRefillFetchesAnEmpty walks
// ConfigurableSwapModes and keeps it that way.
//
// ── WHY IT NAMES A PAYLOAD, AND ONLY SOMETIMES ────────────────────────────
//
// Empty drops the full-bin CONTENT match but not bin-type compatibility, which
// still resolves against the ORDER's payload — the OUTGOING style's on a
// changeover. So a refill leg that changes carrier type has to say which style
// its carrier is for, or it fetches the type the cell is leaving (N1-c, sim
// 2026-08-24). refillCarrierPayload answers that and returns "" when the two
// styles agree, keeping the wire byte-identical for changeovers that do not
// change carrier type.
//
// fromClaim is nil at the call sites that have no outgoing claim to compare
// against (a bare pre-stage, a keep-staged deliver). Those say nothing about
// the payload rather than guessing, which is exactly what they said before.
func refillPickup(fromClaim, toClaim *processes.NodeClaim) protocol.ComplexOrderStep {
	step := buildStep("pickup", toClaim.InboundSource)
	if toClaim.Role != protocol.ClaimRoleProduce || toClaim.InboundSource == "" {
		// A consume node's inbound leg is a payload-matched FULL retrieve —
		// the dual of this, and the reason the flag is not unconditional.
		return step
	}
	step.Empty = true
	if fromClaim != nil {
		step.PayloadCode = refillCarrierPayload(fromClaim, toClaim)
	}
	return step
}

// BuildStageSteps builds steps to pre-stage material at the inbound staging
// node in preparation for a swap. Material is fetched and placed at the
// inbound staging node but NOT yet delivered to the production node.
func BuildStageSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil // no inbound staging configured, cannot pre-stage
	}
	return []protocol.ComplexOrderStep{
		refillPickup(nil, claim),
		stagingDropoff(claim.InboundStaging),
	}
}

// BuildStagedDeliverSteps builds steps to move already-staged material from
// the inbound staging node to the production node. Used after staging + evacuation.
func BuildStagedDeliverSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" {
		return nil
	}
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: claim.InboundStaging},
		{Action: "dropoff", Node: claim.CoreNodeName},
	}
}

// BuildSingleSwapSteps builds a 9-step single-robot swap sequence:
//  1. pickup(InboundSource)           — pick new from source
//  2. dropoff(InboundStaging)         — park new at inbound staging
//  3. wait(CoreNodeName)              — drive to node and hold (RDS BinTask=Wait)
//  4. pickup(CoreNodeName)            — pick up old from line
//  5. dropoff(OutboundStaging)        — quick-park old nearby
//  6. pickup(InboundStaging)          — grab new from staging
//  7. dropoff(CoreNodeName)           — deliver new to line
//  8. pickup(OutboundStaging)         — grab old from staging
//  9. dropoff(OutboundDestination)    — deliver old to final destination
func BuildSingleSwapSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	if claim.InboundStaging == "" || claim.OutboundStaging == "" {
		return nil
	}
	steps := []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),        // 1
		stagingDropoff(claim.InboundStaging),            // 2
		stationWait(claim.CoreNodeName),                 // 3 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},    // 4
		stagingDropoff(claim.OutboundStaging),           // 5
		{Action: "pickup", Node: claim.InboundStaging},  // 6
		{Action: "dropoff", Node: claim.CoreNodeName},   // 7
		{Action: "pickup", Node: claim.OutboundStaging}, // 8
		buildStep("dropoff", claim.OutboundDestination), // 9
	}
	// Produce backfill pulls a fresh EMPTY carrier (the store dual of a consume's
	// full retrieve). Step 1's pickup defaults to a full retrieve, so without this a
	// PRODUCE node's single-robot swap hunts a full payload bin in the empty pool,
	// the dispatch fails ("no bin of..."), and the node is left with no carrier to
	// fill — the cell stalls. Mirrors BuildSequentialBackfillSteps.
	if claim.Role == protocol.ClaimRoleProduce && claim.InboundSource != "" {
		markInboundEmpty(steps, claim.InboundSource, "")
	}
	return steps
}

// BuildTwoRobotSwapSteps builds steps for a two-robot coordinated swap.
// Returns two step lists — one for each robot order:
//
// Order A (resupply robot): pickup new from source → stage → wait → pickup from staging → deliver to node
// Order B (removal robot): wait at node → pickup old from node → deliver to outbound destination
//
// Edge coordinates: releases Order B first (remove old), then releases Order A (deliver new).
func BuildTwoRobotSwapSteps(claim *processes.NodeClaim) (orderA, orderB []protocol.ComplexOrderStep) {
	if claim.InboundStaging == "" {
		return nil, nil
	}
	// Robot A: fetch new material, stage, wait for node clear, then deliver.
	// The wait is wait-with-node at InboundStaging — robot drops the new bin
	// at staging and holds there. wait-with-node produces an RDS Wait block,
	// so RDS reports WAITING and the order reliably transitions to "staged"
	// on Edge. Pre-2026-04-27 this was a bare wait (no node at all),
	// which split the order at the dispatcher level and depended on
	// the seerrds adapter correctly reporting WAITING on incremental
	// (complete=false) orders. That path was fragile and Order A would often
	// stay at in_transit while physically parked, breaking swap_ready and
	// requiring two RELEASE clicks. See shingo_todo.md.
	orderA = []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),       // pick new from source
		stagingDropoff(claim.InboundStaging),           // stage new
		stationWait(claim.InboundStaging),              // hold at staging until line clears
		{Action: "pickup", Node: claim.InboundStaging}, // pick new from staging
		{Action: "dropoff", Node: claim.CoreNodeName},  // deliver to production
	}
	// Robot B: drive to node and hold, wait for release, remove old to destination
	orderB = []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName),                 // drive to node + hold (RDS BinTask=Wait)
		{Action: "pickup", Node: claim.CoreNodeName},    // remove old from production
		buildStep("dropoff", claim.OutboundDestination), // deliver to destination
	}
	return orderA, orderB
}

// BuildTwoRobotPressIndexSwapSteps builds steps for a press-indexing two-robot
// swap. The press has either two or three positions:
//
//	2-position layout (claim.SecondPairedCoreNode == ""):
//	  A (front/output, CoreNodeName), B (back/input, PairedCoreNode)
//	  R1: wait(A) → pickup(A) → dropoff(OutboundDestination)
//	             → pickup(InboundSource) → dropoff(B)
//	  R2: wait(B) → pickup(B) → dropoff(A)
//
//	3-position layout (claim.SecondPairedCoreNode set, = C):
//	  A (front), B (middle, PairedCoreNode), C (back, SecondPairedCoreNode)
//	  R1: wait(A) → pickup(A) → dropoff(OutboundDestination)
//	             → pickup(InboundSource) → dropoff(C)
//	  R2: wait(B) → pickup(B) → dropoff(A) → pickup(C) → dropoff(B)
//
// Both robots fire on operator release. The fleet manager handles cross-leg
// sequencing on shared nodes (R2's dropoff(A) waits for R1's pickup(A);
// R1's dropoff(C) waits for R2's pickup(C) in the 3-position case).
// ── THE FLIP (IndexRobotSupplies), and why it is safe without a gate change ──
//
// Unflipped, R1 is self-sufficient: it evacuates AND backfills, so today's
// INDEX anti-collision arm in swap_hold.go reads it as a leg that needs no
// partner and lets it through. Flipped, R1 is evac-only and R2 owns the
// refill, and the same arm reads BOTH legs as needing a partner.
//
// v1 DOES NOT TOUCH swap_hold.go. SYNTH-round2 established why: applySwapGates
// runs BEFORE acquireComplexSources, so a hold gates CLAIMING, not
// fleet-create — and a stamp that holds both legs is a permanent mutual
// deadlock, both robots and the press out until someone cancels a leg. 5/5
// reviewers reached that independently.
//
// So flipped pairs FAIL OPEN at dispatch, deliberately: both legs dispatch and
// park at their opening stationWait. The physical collision needs one leg
// RELEASED while the other is not, and that is what Unit 2's release-gate
// precondition refuses (see ReleaseStagedOrders). A refused release is a click
// the operator repeats; a refused dispatch is a mutual wait.
func BuildTwoRobotPressIndexSwapSteps(claim *processes.NodeClaim) (orderR1, orderR2 []protocol.ComplexOrderStep) {
	if claim.PairedCoreNode == "" || claim.OutboundDestination == "" {
		return nil, nil
	}
	// R1's opening is the same either way: drive to the press, hold, lift the
	// full tote off, take it away.
	orderR1 = []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName),
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
	backfill := claim.PairedCoreNode
	if claim.SecondPairedCoreNode != "" {
		backfill = claim.SecondPairedCoreNode
	}
	if claim.IndexRobotSupplies {
		// FLIPPED: R1 stops at the destination; R2 indexes forward and then
		// goes for the replacement itself. One robot leaves the cell the
		// moment the press is clear.
		orderR2 = []protocol.ComplexOrderStep{
			stationWait(claim.PairedCoreNode),
			{Action: "pickup", Node: claim.PairedCoreNode},
			{Action: "dropoff", Node: claim.CoreNodeName},
		}
		if claim.SecondPairedCoreNode != "" {
			orderR2 = append(orderR2,
				protocol.ComplexOrderStep{Action: "pickup", Node: claim.SecondPairedCoreNode},
				protocol.ComplexOrderStep{Action: "dropoff", Node: claim.PairedCoreNode})
		}
		orderR2 = append(orderR2,
			buildStep("pickup", claim.InboundSource),
			protocol.ComplexOrderStep{Action: "dropoff", Node: backfill})
	} else {
		// UNFLIPPED (today): R1 carries on to the supermarket and backfills the
		// rearmost position; R2 only shifts the on-deck carrier forward.
		orderR1 = append(orderR1,
			buildStep("pickup", claim.InboundSource),
			protocol.ComplexOrderStep{Action: "dropoff", Node: backfill})
		orderR2 = []protocol.ComplexOrderStep{
			stationWait(claim.PairedCoreNode),
			{Action: "pickup", Node: claim.PairedCoreNode},
			{Action: "dropoff", Node: claim.CoreNodeName},
		}
		if claim.SecondPairedCoreNode != "" {
			orderR2 = append(orderR2,
				protocol.ComplexOrderStep{Action: "pickup", Node: claim.SecondPairedCoreNode},
				protocol.ComplexOrderStep{Action: "dropoff", Node: claim.PairedCoreNode})
		}
	}
	// hop A3 (2026-07-23): a PRODUCE press-index indexes ON-DECK EMPTIES forward
	// onto the press to be filled, so flag the index leg's (R2) paired-node
	// pickups Empty. That drops Core's payload filter (findAvailableForNeed →
	// emptyBinsOnly, claimPayload="") so the index fetches the on-deck carrier
	// regardless of any part number stamped on it — a wrong-part stamp on an
	// on-deck empty is exactly what hung the Hopkinsville swap (the index pickup
	// matched nothing → waiting_for_material). A CONSUME press-index would index
	// FULL bins forward, so it must NOT be flagged (and doesn't occur in
	// practice); scoping to produce also keeps the consume "full retrieve"
	// invariant. The changeover R2 solves the same asymmetry via carriesFromPayload
	// (§ buildPressIndexChangeoverSwap); steady-state has no from-style, so Empty
	// is the mirror. Produce-scoped INSIDE the builder, mirroring BuildSingleSwapSteps
	// / BuildSequentialBackfillSteps' own markInboundEmpty gate.
	if claim.Role == protocol.ClaimRoleProduce {
		markPressIndexOnDeckEmpty(orderR2, claim)
		// TWO FLAGGERS ON ONE LEG, and only under the flip.
		//
		// markPressIndexOnDeckEmpty catches the on-deck pickups (B, and C on a
		// 3-position press); markInboundEmpty catches the supermarket pickup.
		// Unflipped those sit on different legs and never met. Flipped they are
		// both on R2, so they have to compose — each matches on node name and
		// only ever SETS Empty, so neither can clear the other's. Pinned by
		// TestPressIndexFlipped_BothEmptyFlaggersCompose, because a flagger
		// rewritten to assign rather than set would silently unflag the other's
		// pickup and reopen the Hopkinsville waiting_for_material hang.
		if claim.IndexRobotSupplies {
			markInboundEmpty(orderR2, claim.InboundSource, "")
		}
	} else if claim.IndexRobotSupplies {
		// Consume never flags Empty (it indexes FULL bins forward), so the
		// flipped supermarket pickup stays a full retrieve — same invariant as
		// the unflipped R1's.
		_ = backfill
	}
	return orderR1, orderR2
}

// markPressIndexOnDeckEmpty flags the index leg's pickups at the on-deck
// (paired / second-paired) positions as Empty, so Core sources the on-deck
// carrier regardless of its stamped payload. See BuildTwoRobotPressIndexSwapSteps
// (hop A3) for why this is produce-only.
func markPressIndexOnDeckEmpty(steps []protocol.ComplexOrderStep, claim *processes.NodeClaim) {
	for i := range steps {
		if steps[i].Action != protocol.ActionPickup {
			continue
		}
		if steps[i].Node == claim.PairedCoreNode ||
			(claim.SecondPairedCoreNode != "" && steps[i].Node == claim.SecondPairedCoreNode) {
			steps[i].Empty = true
		}
	}
}

// BuildSequentialRemovalSteps builds Order A for sequential mode (removal robot).
// Robot drives to line and holds, waits for operator release, picks up old bin, delivers to destination.
//  1. wait(CoreNodeName)            — drive to node + hold (RDS BinTask=Wait)
//  2. pickup(CoreNodeName)          — pick up old from line
//  3. dropoff(OutboundDestination)  — deliver old to destination
func BuildSequentialRemovalSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName),                 // 1 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},    // 2
		buildStep("dropoff", claim.OutboundDestination), // 3
	}
}

// BuildSequentialBackfillSteps builds Order B for sequential mode (backfill robot).
// Robot picks up new material from source and delivers to line.
// Order B is auto-created by wiring when Order A goes in_transit.
//  1. pickup(InboundSource)    — pick new from source
//  2. dropoff(CoreNodeName)    — deliver to line
func BuildSequentialBackfillSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	steps := []protocol.ComplexOrderStep{
		buildStep("pickup", claim.InboundSource),      // 1
		{Action: "dropoff", Node: claim.CoreNodeName}, // 2
	}
	// Produce/consume are duals: a consume backfill pulls a payload-matched FULL bin
	// from the market; a produce backfill pulls a fresh EMPTY carrier (the store dual).
	// The pickup defaults to a full retrieve, so without this a PRODUCE node's backfill
	// hunts a full payload bin in the empty pool and the dispatch fails ("no bin of
	// requested payload in <inbound group>"), stranding produce-side A/B (sequential)
	// after its first removal. Flag the inbound pickup Empty for produce — the same
	// thing BuildSwapDispatch does via markInboundEmpty for the other modes' StepsA/B.
	// This leg is auto-created by the wiring (handleSequentialBackfill) and never passes
	// through BuildSwapDispatch, so it's flagged here at the source instead.
	if claim.Role == protocol.ClaimRoleProduce && claim.InboundSource != "" {
		markInboundEmpty(steps, claim.InboundSource, "")
	}
	return steps
}

// ──────────────────────────────────────────────────────────────────────────
// Changeover step builders (Phase 3: orders-up-front with operator gates)
// ──────────────────────────────────────────────────────────────────────────
//
// These builders construct the complex-order step sequences for changeover
// flows. All orders for a node are created at changeover start; the operator
// controls flow by releasing wait steps.

// ChangeoverDispatch is the per-mode shape returned by the SwapMode-aware
// changeover step builders. It packages the two complex-order step lists
// (supply and evac) along with their per-order flags so the planner glue
// can assemble NodeAction.SupplyOrder / EvacOrder directly without
// duplicating the per-mode switch on the planner side.
//
// Single-robot modes: StepsA holds a stage list, StepsB holds the full
// 7-step swap (or 8-step evacuate). Multi-robot modes: StepsA and StepsB
// each hold one robot's coordinated leg.
type ChangeoverDispatch struct {
	StepsA        []protocol.ComplexOrderStep
	DeliveryNodeA string
	AutoConfirmA  bool

	// CarriesFromPayloadA stamps StepsA with the from-style payload code. Set it
	// when StepsA opens by lifting an OLD bin off the line: without the stamp the
	// order's payload is left blank, Core backfills it to the target style, and
	// the pickup filters for a bin that isn't there (ALN_001). Legs declared via
	// Roles carry the same signal per-leg (changeoverLeg.carriesFromPayload);
	// this is the single-order equivalent.
	CarriesFromPayloadA bool

	StepsB       []protocol.ComplexOrderStep
	AutoConfirmB bool

	// Roles, when non-nil, declares the two coordinated swap legs by ROLE
	// instead of by StepsA/StepsB position. Only the two-leg swap builders
	// that know the choreography — two_robot and two_robot_press_index — set
	// it. When set, assignDispatch reads Roles and ignores the positional
	// StepsA/StepsB fields.
	//
	// It exists because the positional mapping (StepsA->supply, StepsB->evac)
	// is a two_robot assumption that is INVERTED for press-index, where R1
	// clears the press (evac) and R2 indexes the fresh bin on (supply).
	// Letting the builder declare the role removes every place a consumer could
	// infer it from field order and get press-index wrong. Single-order modes
	// (sequential swap, press_position) and the legacy single_robot stage+swap
	// shape carry no clean evac/supply split, so they keep using StepsA/StepsB.
	// See swap_leg_role.go legPlacesBinAt for the steps-based definition of each
	// role.
	Roles *changeoverSwapLegs
}

// changeoverSwapLegs carries a coordinated two-robot swap's legs keyed by
// ROLE, not by creation order. evac lifts the line's spent bin off; supply
// places the incoming bin on the line. The builder assigns the roles because
// it alone knows the choreography.
type changeoverSwapLegs struct {
	evac   changeoverLeg
	supply changeoverLeg
}

// changeoverLeg is one leg of a role-declared swap dispatch. deliveryNode is
// the leg's final resting node, derived from its steps (finalDropoff); it is
// left empty on the evac leg, whose destination Core derives from the steps.
type changeoverLeg struct {
	steps        []protocol.ComplexOrderStep
	deliveryNode string
	autoConfirm  bool
	// carriesFromPayload marks a leg whose line/press pickup is an OLD
	// (from-style) bin, so its order must be stamped with the from-style
	// payload to claim it. The evac leg always removes the old line bin
	// (assignDispatch stamps it unconditionally); this flag is for the supply
	// leg, which carries an old bin only for press-index (R2 indexes an
	// existing tote) — two_robot's supply fetches fresh material, so it stays
	// blank (backfilled to the target style). See assignDispatch, ALN_001.
	carriesFromPayload bool
}

// rejected reports whether a builder produced no dispatch (its "I rejected
// this claim" signal). Empty StepsA covers the positional builders; a nil
// Roles covers the role-declared two-leg builders.
func (d ChangeoverDispatch) rejected() bool {
	return d.StepsA == nil && d.Roles == nil
}

// BuildSwapChangeoverSteps builds the changeover swap dispatch (no tool
// clearance) for the given SwapMode. Internal switch on fromClaim.SwapMode.
//
//   - single_robot: 7-step Order B at fromClaim.CoreNodeName plus a stage
//     Order A. Single wait at the line.
//   - two_robot: Order A pre-stages new → waits at staging → delivers;
//     Order B drives to line → waits → evacuates to OutboundDestination.
//     Both fire on operator release.
//   - two_robot_press_index: mirrors BuildTwoRobotPressIndexSwapSteps —
//     R1 evacuates from CoreNodeName then refills from InboundSource into
//     the back position; R2 indexes the press.
//   - sequential: single robot, single complex order, mid-sequence wait
//     at the active position. Cutover click flips ActivePull and
//     releases the wait.
//
// For unrecognised / "simple" modes, falls back to the single-robot
// pattern.
//
// inactiveNode / activeNode are populated only for sequential mode (the
// planner reads ActivePull at plan time and computes them); other modes
// ignore the values.
func BuildSwapChangeoverSteps(fromClaim, toClaim *processes.NodeClaim, inactiveNode, activeNode string) ChangeoverDispatch {
	switch fromClaim.SwapMode {
	case protocol.SwapModeTwoRobot:
		return buildTwoRobotChangeoverSwap(fromClaim, toClaim)
	case protocol.SwapModeTwoRobotPressIndex:
		return buildPressIndexChangeoverSwap(fromClaim, toClaim, false /* tooling */)
	case protocol.SwapModeSequential:
		// Per-node (2026-08-28): this diff's own position only. activeNode is
		// unused here — which side THIS claim is on is decided by comparing its
		// own CoreNodeName against the parked one, and the parked one is a
		// property of the pair rather than of the caller.
		_ = activeNode
		return buildSequentialPerPositionSwap(fromClaim, toClaim, inactiveNode)
	case pressPositionSwapMode:
		// Synthesized per-position diff from the press-index different-
		// bin-type fan-out: each position dispatches its own NodeAction
		// so up to three robots fire in parallel for one press's changeover.
		return buildPressIndexPerPositionSwap(fromClaim, toClaim)
	default:
		return buildSingleRobotChangeoverSwap(fromClaim, toClaim, false)
	}
}

// BuildEvacuateChangeoverSteps builds the changeover evacuate dispatch
// (tool clearance needed). Same SwapMode switch as BuildSwapChangeoverSteps:
//
//   - single_robot: wait between dropoff-old-at-staging and pickup-new.
//   - two_robot: identical to Swap — two independent robots clear the
//     line during tooling without an extra operator gate, so no second
//     wait is needed.
//   - two_robot_press_index: extra wait on R1 between dropoff old at
//     OutboundDestination and pickup new from InboundSource.
//   - sequential: this position's own evac+backfill, gated on the bare
//     tooling-done wait; the single click still releases both positions
//     through ReleaseChangeoverWait's per-task fan-out.
//
// activeNode is accepted for signature symmetry with BuildSwapChangeoverSteps
// and is unread: evacuate has no cutover gate, because both positions come off
// the line. inactiveNode IS read now — not for choreography, but because the
// parked position of a produce press holds an on-deck EMPTY whether the press
// is changing tools or styles, and its pickup has to say so.
func BuildEvacuateChangeoverSteps(fromClaim, toClaim *processes.NodeClaim, inactiveNode, activeNode string) ChangeoverDispatch {
	_ = activeNode
	switch fromClaim.SwapMode {
	case protocol.SwapModeTwoRobot:
		return buildTwoRobotChangeoverSwap(fromClaim, toClaim)
	case protocol.SwapModeTwoRobotPressIndex:
		return buildPressIndexChangeoverSwap(fromClaim, toClaim, true)
	case protocol.SwapModeSequential:
		return buildSequentialPerPositionEvacuate(fromClaim, toClaim, inactiveNode)
	case pressPositionSwapMode:
		// Per-position dispatch: the parent evacuate situation drives the
		// "evacuate" semantics, but at the per-position level the robot
		// work is identical to Swap (evac old, fetch new, deliver new).
		//
		// A MARKED position's tooling treatment is NOT decided here. The tooling
		// decorator edits this leg afterwards — the staging hold always, and a
		// redirected destination only if the cell named an override — precisely
		// so that no builder has to know whether the press is marked. That
		// knowledge lived in a predicate here once, and an earlier pass
		// rewriting SwapMode is what made it unreachable.
		return buildPressIndexPerPositionSwap(fromClaim, toClaim)
	default:
		return buildSingleRobotChangeoverSwap(fromClaim, toClaim, true)
	}
}

// buildSingleRobotChangeoverSwap is the legacy single-robot 7- or
// 8-step pattern. Order A pre-stages at InboundStaging (operator
// confirms staging); Order B does the line-side swap on operator
// release.
func buildSingleRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim, tooling bool) ChangeoverDispatch {
	stepsB := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName),              // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName}, // evacuate old
		stagingDropoff(fromClaim.OutboundStaging),        // park old
	}
	if tooling {
		stepsB = append(stepsB, stationWait("")) // "tooling done"
	}
	stepsB = append(stepsB,
		protocol.ComplexOrderStep{Action: "pickup", Node: toClaim.InboundStaging},    // grab new
		protocol.ComplexOrderStep{Action: "dropoff", Node: toClaim.CoreNodeName},     // deliver new
		protocol.ComplexOrderStep{Action: "pickup", Node: fromClaim.OutboundStaging}, // grab old
		buildStep("dropoff", fromClaim.OutboundDestination),                          // clear old to final
	)
	return ChangeoverDispatch{
		StepsA:        BuildStageSteps(toClaim),
		DeliveryNodeA: toClaim.InboundStaging,
		AutoConfirmA:  false,
		StepsB:        stepsB,
		AutoConfirmB:  true,
	}
}

// buildTwoRobotChangeoverSwap mirrors BuildTwoRobotSwapSteps adapted for
// changeover: from-claim drives the outbound side, to-claim drives the
// inbound side. The "ready" wait is the operator gate that releases both
// robots; once released, Robot B clears the line while Robot A delivers
// the new bin. No second wait point: with two robots running independently
// the line is naturally clear during tooling, and Swap and Evacuate
// produce the same step list.
func buildTwoRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if toClaim.InboundStaging == "" {
		return ChangeoverDispatch{}
	}
	stepsA := []protocol.ComplexOrderStep{
		refillPickup(fromClaim, toClaim),       // fetch a fresh EMPTY carrier
		stagingDropoff(toClaim.InboundStaging), // stage new
		stationWait(toClaim.InboundStaging),    // "ready" — shared release gate
		{Action: "pickup", Node: toClaim.InboundStaging},
		{Action: "dropoff", Node: toClaim.CoreNodeName},
	}
	stepsB := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName),                 // drive to node + hold (shared "ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},    // evacuate old
		buildStep("dropoff", fromClaim.OutboundDestination), // straight to final
	}
	return ChangeoverDispatch{
		Roles: &changeoverSwapLegs{
			// stepsA fetches new material and delivers it to the line — supply.
			supply: changeoverLeg{steps: stepsA, deliveryNode: finalDropoff(stepsA), autoConfirm: true},
			// stepsB waits at the line, lifts the old bin, carries it to
			// outbound — evac. Core derives its delivery node from the steps.
			evac: changeoverLeg{steps: stepsB, autoConfirm: true},
		},
	}
}

// buildPressIndexChangeoverSwap mirrors BuildTwoRobotPressIndexSwapSteps —
// R1 evacuates from CoreNodeName and reloads the back position from
// InboundSource; R2 indexes intermediate positions. Honors 2-pos vs 3-pos
// via fromClaim.SecondPairedCoreNode (using fromClaim's geometry is
// consistent with the swap_dispatch pattern).
//
// For evacuate, a "tooling done" wait sits on R1 between the outbound
// dropoff and the inbound pickup so the operator gates the refill leg
// after the line has been cleared.
func buildPressIndexChangeoverSwap(fromClaim, toClaim *processes.NodeClaim, tooling bool) ChangeoverDispatch {
	if fromClaim.PairedCoreNode == "" || fromClaim.OutboundDestination == "" {
		return ChangeoverDispatch{}
	}
	// R1 prefix is identical for 2-pos and 3-pos: wait, evac, dropoff destination.
	r1 := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName),
		{Action: "pickup", Node: fromClaim.CoreNodeName},
		buildStep("dropoff", fromClaim.OutboundDestination),
	}
	if tooling {
		r1 = append(r1, stationWait("")) // "tooling done"
	}
	// R1's other pickup — the old front tote — keeps the from-style payload it
	// needs (§ carriesFromPayload below); only this leg fetches a fresh carrier.
	r1 = append(r1, refillPickup(fromClaim, toClaim))
	if fromClaim.SecondPairedCoreNode != "" {
		// 3-position: refill back position.
		r1 = append(r1, protocol.ComplexOrderStep{Action: "dropoff", Node: fromClaim.SecondPairedCoreNode})
	} else {
		// 2-position: refill paired (back) position.
		r1 = append(r1, protocol.ComplexOrderStep{Action: "dropoff", Node: fromClaim.PairedCoreNode})
	}
	var r2 []protocol.ComplexOrderStep
	if fromClaim.SecondPairedCoreNode != "" {
		r2 = []protocol.ComplexOrderStep{
			stationWait(fromClaim.PairedCoreNode),
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
			{Action: "pickup", Node: fromClaim.SecondPairedCoreNode},
			{Action: "dropoff", Node: fromClaim.PairedCoreNode},
		}
	} else {
		r2 = []protocol.ComplexOrderStep{
			stationWait(fromClaim.PairedCoreNode),
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
		}
	}
	return ChangeoverDispatch{
		Roles: &changeoverSwapLegs{
			// R1 lifts the spent tote off the front and refills the back — evac.
			// Its delivery node is left blank for Core to derive from the steps
			// (the back position); the old hardcoded fromClaim.CoreNodeName was
			// a lie — R1's bin comes to rest at the back, not the front, which
			// is what bound the wrong bin at HK 2026-07-14.
			evac: changeoverLeg{steps: r1, autoConfirm: true},
			// R2 indexes the fresh tote onto the front — supply. It picks up an
			// OLD (from-style) tote at the back position, so it carries the
			// from-style payload; without it the removal filters for the new
			// payload and finds no bin (ALN_001), the same defect the evac slot
			// already guards on two_robot.
			supply: changeoverLeg{steps: r2, deliveryNode: finalDropoff(r2), autoConfirm: true, carriesFromPayload: true},
		},
	}
}

// buildPressIndexPerPositionSwap is the per-position dispatch for a
// synthesized press-index different-bin-type fan-out claim. Single
// complex order, 4 steps, no operator gate inside the order:
//
//	pickup(my position)       evac old bin
//	dropoff(OutboundDestination)
//	pickup(InboundSource)     fetch new bin
//	dropoff(my position)      deliver new bin
//
// The fan-out post-processor synthesized one such per-position claim
// per occupied/needed position; each gets its own NodeAction with this
// dispatch, so up to 3 robots fire in parallel for one press's
// changeover. Half-cases (from-only or to-only positions) reach the
// planner as SituationDrop / SituationAdd respectively and route
// through the simpler builders (BuildReleaseSteps for evac-only;
// planFallbackStagingAction → Retrieve order for refill-only).
//
// Returns ChangeoverDispatch with StepsA only (single-order shape).
// Empty dispatch when required fields are missing — the planner
// already validated them via the per-mode registry, but the empty-
// dispatch backstop catches the impossible case where validation
// passed but the synthesized claim is malformed.
func buildPressIndexPerPositionSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if fromClaim == nil || toClaim == nil {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.CoreNodeName == "" {
		return ChangeoverDispatch{}
	}
	pos := fromClaim.CoreNodeName
	steps := []protocol.ComplexOrderStep{
		{Action: "pickup", Node: pos},                       // evac old bin
		buildStep("dropoff", fromClaim.OutboundDestination), // old bin to destination
		// The opening pickup lifts the OLD bin off the position, so the order
		// carries the from-style payload; this one fetches the carrier the
		// INCOMING style needs. refillPickup keeps the two from being the same
		// question — see its doc for why naming the style is load-bearing
		// (N1-c, sim 2026-08-24).
		refillPickup(fromClaim, toClaim), // fetch new bin
		{Action: "dropoff", Node: pos},   // deliver new bin
	}
	return ChangeoverDispatch{
		StepsA:        steps,
		DeliveryNodeA: pos,
		AutoConfirmA:  true,
		// The opening pickup lifts the OLD bin off the position, so the order
		// carries the from-style payload. Safe alongside the Empty refill above,
		// whose own payload filter is dropped.
		CarriesFromPayloadA: true,
		StepsB:              nil,
	}
}

// buildToolingEvacSteps is the one-robot tooling-evacuation shape: take the
// blocking bin off a line position, put the replacement where it waits, and
// deliver it when the operator says the tool is done.
//
//	pickup(position)      lift the bin that blocks the tool
//	dropoff(evacDest)     get it off the line
//	pickup(inboundSource) fetch the incoming style's bin
//	wait(waitNode)        the tooling-done gate
//	dropoff(position)     deliver on release
//
// ── WHY waitNode IS A PARAMETER AND NOT A CONSTANT ────────────────────────
//
// Sequential passes "" — a BARE wait. Its robot holds the bin wherever it
// happens to be when the gate closes, and one tooling-done click releases both
// positions' waits through ReleaseChangeoverWait's per-task fan-out. That
// works because a sequential cell is two positions beside each other and the
// robot has nowhere better to be.
//
// The staged press-index positions pass InboundStaging — a wait AT A NODE, so the
// robot drives there holding the bin and parks. That difference is DELIBERATE
// and owner-chosen, not an accident of two builders growing apart: a press
// tooling change can take a shift, and robots idling on the press apron for
// that long block the very access the millwrights need. Staging gets them out
// of the cell while still holding the bin, so release is a short move in
// rather than a full fetch.
//
// Same steps, one parameter, so the difference is legible at both call sites
// instead of living in two builders that merely look similar.
// It takes the CLAIMS rather than a bare inbound-source name so its refill leg
// goes through refillPickup like every other one. Passing the name was how
// buildSequentialChangeoverEvacuate — which shares this helper — ended up
// fetching a full payload-matched bin from the empty pool: the string carried
// the node but not the produce/empty question, so each caller had to answer it
// again afterwards, and that caller never did.
func buildToolingEvacSteps(position, evacDest string, fromClaim, toClaim *processes.NodeClaim, waitNode string) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: position},
		buildStep("dropoff", evacDest),
		refillPickup(fromClaim, toClaim),
		stationWait(waitNode),
		{Action: "dropoff", Node: position},
	}
}

// ── SEQUENTIAL CHANGEOVER IS PER-NODE (owner ruling, 2026-08-28) ──────────
//
// "It's not one order for two nodes — sequential just pulls from two nodes."
//
// buildSequentialChangeoverSwap and buildSequentialChangeoverEvacuate used to
// live here, each returning ONE order (or one pair of orders) spanning BOTH
// positions of an A/B press, with the cutover wait in the middle of the step
// list. That was the overbuilt part, and it broke in the way overbuilding
// usually does — by disagreeing with the thing that calls it.
//
// DiffStyleClaims emits one Swap diff PER CLAIMED POSITION, and on an A/B press
// both positions are claimed in both styles. Two diffs, each handed a builder
// that covered the whole press, produced TWO identical whole-press orders that
// then raced for the same two bins: "reserve miss: bins present but none
// available (claimed/reserved/locked elsewhere)". The press was planned twice.
//
// Everything else about sequential was already per-node —
// BuildSequentialRemovalSteps and BuildSequentialBackfillSteps are per-node
// orders on CoreNodeName. Changeover was the only place the press was treated
// as one machine, so per-node is a return to the family's own shape rather than
// a new idea. What the collapsed order encoded and the pair does not is the
// cross-node dependency (cut over only after the parked side is refilled).
//
// That dependency moved to a per-node cutover gate, and then out of the code
// entirely: SequentialChangeoverCutover is deleted (owner ruling 2026-08-28,
// recorded at the head of operator_changeover_cutover.go). It bundled "flip the
// pull, then release the wait" into one changeover-only button that needed a
// precondition of its own to stop cutting over onto an unstocked position. Both
// halves already exist as ordinary, mode-agnostic operator controls — the A/B
// flip and the per-node release — each carrying its own physical guard, so the
// ordering now lives with the operator and in those two guards.
//
// Three consequences, and they are why the ruling is cheaper than the fix it
// replaces: the double-plan dissolves rather than needing a dedupe pass; each
// position keeps its own changeover task linked to its own order, so board
// visibility comes free; and the empty/full asymmetry below becomes a per-node
// decision, which is what it physically is.

// buildSequentialPerPositionSwap builds ONE position's changeover swap for a
// sequential A/B press: an opening wait at this position, then the four-step
// direct trip.
//
//	wait(my position)                   — the operator's release
//	pickup(my position)                 — lift what is standing here
//	dropoff(OutboundDestination)
//	refillPickup(fromClaim, toClaim)    — fetch the incoming style's carrier
//	dropoff(my position)                — deliver it
//
// Direct trips, no InboundStaging hop — sequential's steady-state backfill
// fetches InboundSource and drops straight at CoreNodeName, so changeover
// follows it. The shape is buildPressIndexPerPositionSwap's, which is the same
// physical choreography for the same reason.
//
// ── BOTH POSITIONS WAIT, AND THE OPERATOR SEQUENCES THEM ──────────────────
//
// This used to be two shapes: the parked side ran IMMEDIATELY while the active
// side opened with a wait, which put the ordering in the choreography. It is the
// operator's now (owner ruling 2026-08-28, and the code below says so at the
// site) — one release per node, sequenced however he likes — and the ordering
// safety lives in the two guards instead: a robot may not strip a position the
// line is still pulling from, and the line may not be flipped onto a position
// that is not ready.
//
// So each position opens with stationWait at its OWN node. The robot drives to
// the position it will clear and parks there visibly, holding until that node's
// own release. Wait-with-node rather than a bare wait so RDS reports WAITING and
// the order reliably reaches `staged` on Edge — the same fragility fix the
// two_robot pattern applies to its own mid-sequence wait.
//
// ── AND THEY ARE NOT HOLDING THE SAME THING (the flag split) ──────────────
//
// The ACTIVE position holds the partial FULL of the outgoing style. Its order
// carries the from-payload, or lookupPayloadMeta backfills it to the INCOMING
// style and the opening pickup filters for a part the press does not have —
// the ALN_001 shape (30630c70 / 1a6b6d23).
//
// The PARKED position of a PRODUCE press holds an on-deck EMPTY carrier by
// steady-state design. A payload-matched full retrieve there matches nothing
// and waits forever on finder-node-empty, which is the second blocker the sim
// surfaced ("Waiting for material: PANEL-B in PLN_004"). So its pickup is
// flagged Empty — dropping Core's payload filter but not its bin-type
// compatibility (N1-c) — and the order carries no from-payload, because there
// is no outgoing bin here to match. Direct port of markPressIndexOnDeckEmpty,
// produce-scoped for that rule's own reason: a CONSUME sequential press parks a
// FULL standby bin and keeps the full-retrieve invariant.
//
// inactiveNode is the parked side as resolveSequentialActivePull sees it — a
// property of the PAIR, so both positions' diffs agree on which is which.
//
// Empty dispatch on a malformed claim; the planner turns that into
// NodeAction.Err. requiredChangeoverFields and (since this ruling) the
// claim-validation arm both catch the field cases first, with a message naming
// the field.
func buildSequentialPerPositionSwap(fromClaim, toClaim *processes.NodeClaim, inactiveNode string) ChangeoverDispatch {
	if fromClaim == nil || toClaim == nil {
		return ChangeoverDispatch{}
	}
	if fromClaim.PairedCoreNode == "" || inactiveNode == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	pos := fromClaim.CoreNodeName
	if pos == "" {
		return ChangeoverDispatch{}
	}
	parked := pos == inactiveNode
	onDeckEmpty := parked && fromClaim.Role == protocol.ClaimRoleProduce

	// BOTH POSITIONS OPEN WITH A WAIT AT THEIR OWN NODE (owner ruling
	// 2026-08-28). The parked side used to run immediately, which made the two
	// sides different shapes and put the ordering in the choreography. It is the
	// operator's now: one release per node, sequenced however he likes, and the
	// ordering safety lives in the release and flip guards instead — a robot may
	// not strip a position the line is still pulling from, and the line may not
	// be flipped onto a position that is not ready.
	//
	// The robot therefore ARRIVES EMPTY-HANDED and waits. Fetching the new bin
	// after the old one clears is what makes a per-node release safe to hold
	// indefinitely: nothing is standing in the aisle holding material for a
	// position whose operator has not pressed anything yet.
	steps := []protocol.ComplexOrderStep{
		stationWait(pos),
		{Action: "pickup", Node: pos, Empty: onDeckEmpty},
		buildStep("dropoff", fromClaim.OutboundDestination),
		refillPickup(fromClaim, toClaim),
		{Action: "dropoff", Node: pos},
	}
	return ChangeoverDispatch{
		StepsA:        steps,
		DeliveryNodeA: pos,
		AutoConfirmA:  true,
		// Only when this order actually lifts an old bin. The parked produce
		// position lifts an empty carrier instead, whose payload filter is
		// dropped — naming the outgoing style there would say something untrue
		// about a step that is not asking the question.
		CarriesFromPayloadA: !onDeckEmpty,
		StepsB:              nil, // per-node: one order, this position's
	}
}

// buildSequentialPerPositionEvacuate is the tooling variant of the same split:
// ONE position's evac-and-backfill, gated on the tooling-done click.
//
//	pickup(my position)              — lift what is standing here
//	dropoff(OutboundDestination)
//	refillPickup(fromClaim, toClaim) — fetch new material now, hold it
//	wait("")                         — the tooling-done gate, BARE
//	dropoff(my position)             — deliver on release
//
// No inactive/active distinction in the CHOREOGRAPHY — both positions come off
// the line because production is going down anyway, so neither waits for a
// cutover. The parked/produce EMPTY flag still applies: what is standing on the
// parked position is an on-deck empty whether the press is changing tools or
// styles.
//
// THE BARE WAIT IS WHAT KEEPS THE SINGLE CLICK. One tooling-done click still
// releases both positions, and it does so the same way it always did — through
// ReleaseChangeoverWait's per-task fan-out, which walks every node task of the
// changeover. Under the whole-press builder those two waits were two step-lists
// in one NodeAction; now they are two orders on two tasks. The fan-out iterates
// tasks either way, so the operator-facing semantic is unchanged.
func buildSequentialPerPositionEvacuate(fromClaim, toClaim *processes.NodeClaim, inactiveNode string) ChangeoverDispatch {
	if fromClaim == nil || toClaim == nil {
		return ChangeoverDispatch{}
	}
	if fromClaim.PairedCoreNode == "" || inactiveNode == "" {
		return ChangeoverDispatch{}
	}
	if fromClaim.OutboundDestination == "" || toClaim.InboundSource == "" {
		return ChangeoverDispatch{}
	}
	pos := fromClaim.CoreNodeName
	if pos == "" {
		return ChangeoverDispatch{}
	}
	onDeckEmpty := pos == inactiveNode && fromClaim.Role == protocol.ClaimRoleProduce

	steps := buildToolingEvacSteps(pos, fromClaim.OutboundDestination, fromClaim, toClaim, "")
	if onDeckEmpty {
		// The opening pickup, and only it: the refill pickup later in the list is
		// already Empty-by-role through refillPickup, and the dropoffs are not
		// pickups at all.
		steps[0].Empty = true
	}
	return ChangeoverDispatch{
		StepsA:              steps,
		DeliveryNodeA:       pos,
		AutoConfirmA:        true,
		CarriesFromPayloadA: !onDeckEmpty,
		StepsB:              nil,
	}
}

// BuildKeepStagedEvacSteps builds Robot B's complex order for keep-staged
// changeovers. Simpler than swap/evacuate — no outbound staging hop, goes
// straight to final destination after evacuation.
func BuildKeepStagedEvacSteps(fromClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName),                 // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},    // evacuate old
		buildStep("dropoff", fromClaim.OutboundDestination), // straight to final
	}
}

// BuildKeepStagedDeliverSteps builds Robot A's complex order for keep-staged
// changeovers (split mode — two robots). Stages new material then waits for
// operator release to deliver.
func BuildKeepStagedDeliverSteps(toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		refillPickup(nil, toClaim),                       // grab new
		stagingDropoff(toClaim.InboundStaging),           // stage new
		stationWait(""),                                  // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging}, // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},  // deliver to line
	}
}

// BuildKeepStagedCombinedSteps builds Robot A's complex order for keep-staged
// changeovers (combined mode — single robot). Clears the keep-staged bin, stages
// new material, waits, then delivers.
func BuildKeepStagedCombinedSteps(fromClaim, toClaim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: toClaim.InboundStaging}, // grab keep-staged bin
		buildStep("dropoff", fromClaim.InboundSource),    // return to market/source
		refillPickup(fromClaim, toClaim),                 // grab changeover material
		stagingDropoff(toClaim.InboundStaging),           // stage new
		stationWait(""),                                  // "ready"
		{Action: "pickup", Node: toClaim.InboundStaging}, // grab new
		{Action: "dropoff", Node: toClaim.CoreNodeName},  // deliver to line
	}
}
