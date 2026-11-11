package engine

import (
	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/release"
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
func stationWait(node string, purpose release.Purpose) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{
		Action:   "wait",
		Node:     node,
		WaitKind: waitKindStation,
		Purpose:  string(purpose),
	}
}

// waitKindStation is orders.WaitKindStation, which is protocol.WaitKindStation:
// the one definition, which Core's dispatch.WaitKindStation aliases too. Its
// wire value is pinned by TestWaitKindStation_TheWireValue (protocol).
const waitKindStation = orders.WaitKindStation

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
//
// THE FENCE IS RIGHT; THAT SENTENCE IS NOT WHAT MAKES IT RIGHT. "Its sibling is
// on the way to clear it" describes a race, and a race is something a wait can
// win. What cannot be won is the PHASE: Core's exclusive-dropoff check runs
// before the sources are acquired, so an exclusive line node holds the supply at
// CLAIMING time while its evac — held on the supply securing a bin — is at the
// same phase waiting for it. Neither is on the way anywhere yet. Two legs each
// waiting for the other to acquire is a mutual wait no event ends, which is why
// this is a fence rather than a tuning knob. See reserveComplexDestination in
// shingo-core/dispatch/complex_dispatch.go, which states the same rule from the
// other side.
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
// needs no second confirmation: the operator entered the count at the EMPTY
// click, and it travels with the order — on the move's remaining count, or on
// the curtain wait's release where the position is curtained.
func BuildStagedReleaseSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName, release.PurposeReady),
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
}

// refillPickup is the ONE way to express "fetch a fresh carrier from the
// inbound source". Every builder that opens a leg at InboundSource must use it,
// and TestArch_InboundPickupUsesRefillPickup fails the build if one does not.
//
// THE SENTENCE ABOVE WAS FALSE WHEN IT WAS WRITTEN, which is its own defect:
// five builders in this file opened their inbound leg with a raw
// buildStep("pickup", claim.InboundSource) and got the Empty flag from a
// post-hoc markInboundEmpty pass in BuildSwapDispatch instead. Three of those
// five carried their own Role == Produce guard as well; two carried nothing and
// were correct only because a caller fixed them up afterwards — the exact
// arrangement this constructor was written to abolish, still standing under a
// comment claiming it had been.
//
// The five are converted and the arch test is what keeps the claim true. Go
// cannot make it unwriteable: buildStep is already unexported and every bypass
// was inside this same file, so there is no visibility boundary left to move.
// A grep-based structural test is the strongest rung actually available here,
// and saying so is better than a comment that asserts a coverage it does not
// have.
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
// against (a bare pre-stage, a steady-state swap). Those say nothing about the
// payload rather than guessing, which is exactly what they said before.
func refillPickup(fromClaim, toClaim *processes.NodeClaim) protocol.ComplexOrderStep {
	// A KEEP-STAGED CLAIM'S CARRIER COMES FROM ITS SPOT. The spot is one more
	// node the full or the empty comes from instead of the market, in every
	// swap mode, so this is the only place that has to know about it: every
	// builder that fetches a fresh carrier asks here.
	if spotNode(toClaim) != "" {
		return spotPickup(toClaim)
	}
	step := buildStep("pickup", toClaim.InboundSource)
	if toClaim.InboundSource == "" {
		return step
	}
	// A produce node refills with an EMPTY carrier; a consume node refills with a
	// payload-matched FULL retrieve. That is the only difference between the two,
	// and it is this flag.
	step.Empty = toClaim.Role == protocol.ClaimRoleProduce
	// ── WHICH PAYLOAD, AND WHY BOTH ROLES HAVE TO SAY IT ──────────────────
	//
	// refillCarrierPayload already answers this: blank when the two styles want
	// the same thing (any carrier will do), the INCOMING style's code when they
	// differ. Produce asked it; consume returned before reaching it and inherited
	// the ORDER's payload instead.
	//
	// That inheritance is wrong on a changeover, and silently. This order opens by
	// lifting the old bin off the line, so it is stamped with the FROM style's
	// payload on purpose — an old-tote pickup filtered for the new payload finds
	// no bin, which is ALN_001. The refill pickup two steps later then inherited
	// that same FROM payload and went looking for the outgoing part to feed a cell
	// that had just changed over. One order, one payload field, two pickups that
	// want opposite answers.
	//
	// It never showed up because it cannot fire on produce: there the parked
	// position holds an EMPTY, so the order is not stamped with the from-payload
	// at all (CarriesFromPayloadA: !onDeckEmpty in changeover_planner.go). A
	// consume A/B pair holds MATERIAL on the parked side, so the stamp lands and
	// the refill inherits it. No fixture had ever configured one — every
	// sequential claim in demo.yaml and lane-stress.yaml is produce — so the
	// changeover simply waited forever for a part the market was never going to
	// be asked for, with the incoming material sitting unclaimed beside it.
	//
	// Naming it per-step is what stops the two pickups sharing one answer.
	if fromClaim != nil {
		step.PayloadCode = refillCarrierPayload(fromClaim, toClaim)
	}
	return step
}

// BuildStageSteps builds steps to pre-stage material at the inbound staging
// node in preparation for a swap. Material is fetched and placed at the
// inbound staging node but NOT yet delivered to the production node.
//
// Nil when there is nothing to stage: no inbound staging, or a carrier that
// already stands on it (a keep-staged claim whose spot is its inbound staging).
func BuildStageSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	prefix, _ := stageSteps(nil, claim, relayPickup(claim))
	return prefix
}

// stageSteps is how a swap that stages its incoming carrier gets it: the fetch
// to the to-claim's inbound staging that opens the swap, and the lift that
// collects it from there later. relay is the builder's own collect for a bin
// this order parked there. fromClaim is the outgoing claim on a changeover (so
// the fetch can say which style its carrier is for) and nil otherwise.
//
// A SET-DOWN ON THE NODE THE BIN WAS JUST FETCHED FROM IS NOT BUILT. When the
// fetch is at the inbound staging itself (a keep-staged claim naming the same
// node as both), the carrier already stands where the swap collects it: there
// is no prefix, and the fetch is the collect. That is the whole of what a
// keep-staged swap is, in a mode that stages: the ordinary swap, with its
// carrier coming from the spot.
func stageSteps(fromClaim, toClaim *processes.NodeClaim, relay protocol.ComplexOrderStep) (prefix []protocol.ComplexOrderStep, collect protocol.ComplexOrderStep) {
	if toClaim.InboundStaging == "" {
		return nil, relay
	}
	fetch := refillPickup(fromClaim, toClaim)
	if fetch.Node == toClaim.InboundStaging {
		return nil, fetch
	}
	return []protocol.ComplexOrderStep{fetch, stagingDropoff(toClaim.InboundStaging)}, relay
}

// relayPickup re-collects the bin this same order parked on inbound staging a
// few steps earlier. It names nothing: Core reads a pickup at a node its own
// plan dropped on as a relay, not as a source.
func relayPickup(claim *processes.NodeClaim) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: "pickup", Node: claim.InboundStaging}
}

// spotPickup lifts the spare standing on a keep-staged claim's spot. It is a
// real source, not a relay, so it NAMES THE PART and Core judges the bin for
// it as what the spare carries (carriesEmpty). On a consume line the spare is
// a full, and Core takes stock of the part: a full of another part, one counted
// to nothing and an empty hold the leg. On a produce line it is an empty, so
// the pickup is Empty, and naming the part it asks for an empty the part may
// ride (the carrier rule: an empty of another type holds the leg, which is how
// a changeover's supply waits while the outgoing style's spare stands there),
// or a partial of the part when the part has a partial home. Core binds what
// the step says it lifts (ground rule 5), so a produce spot pickup naming the
// part without Empty would take no empty at all. Both roles use this one step
// (Core dispatch TestSpotPickup_HowTheStepFlagJudgesTheBin). refillPickup is
// its only caller.
func spotPickup(claim *processes.NodeClaim) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: spotNode(claim), PayloadCode: claim.PayloadCode,
		Empty: carriesEmpty(claim)}
}

// twoRobotSupplyTail carries the bin standing on inbound staging to the line:
// hold at the staging node until the line is clear, lift, deliver. The caller
// builds the wait, on the staging node and with its own release purpose
// (PurposeSwap steady, PurposeReady at a changeover), so every stationWait
// names its purpose where it is written. The wait comes BEFORE the lift, so a
// cancel during it leaves the bin where it stands.
func twoRobotSupplyTail(wait, collect protocol.ComplexOrderStep, line string) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		wait,                            // hold at staging until line clears
		collect,                         // pick new from staging
		{Action: "dropoff", Node: line}, // deliver to production
	}
}

// twoRobotEvac is the removal robot's order: drive to the line and hold, then
// carry the old bin to the outbound destination.
func twoRobotEvac(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName, release.PurposeSwap), // drive to node + hold (RDS BinTask=Wait)
		{Action: "pickup", Node: claim.CoreNodeName},         // remove old from production
		buildStep("dropoff", claim.OutboundDestination),      // deliver to destination
	}
}

// singleRobotTail is the single-robot swap from the bin standing on inbound
// staging: steps 3-9 of BuildSingleSwapSteps, with collect at step 6.
func singleRobotTail(claim *processes.NodeClaim, collect protocol.ComplexOrderStep) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName, release.PurposeSwap), // 3 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},         // 4
		stagingDropoff(claim.OutboundStaging),                // 5
		collect,                                              // 6
		{Action: "dropoff", Node: claim.CoreNodeName},        // 7
		{Action: "pickup", Node: claim.OutboundStaging},      // 8
		buildStep("dropoff", claim.OutboundDestination),      // 9
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
	prefix, collect := stageSteps(nil, claim, relayPickup(claim))
	// A produce backfill's fetch is an EMPTY carrier: refillPickup, inside
	// stageSteps, flags it.
	return append(prefix, singleRobotTail(claim, collect)...) // 1-2, then 3-9
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
	prefix, collect := stageSteps(nil, claim, relayPickup(claim))
	orderA = append(prefix,
		twoRobotSupplyTail(stationWait(claim.InboundStaging, release.PurposeSwap), collect,
			claim.CoreNodeName)...)
	// Robot B: drive to node and hold, wait for release, remove old to destination
	return orderA, twoRobotEvac(claim)
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
// ── THE FLIP (RefillBy: Robot 2), and why it needs no gate ──────────────────
//
// This used to explain why v1 did not touch Core's index anti-collision arm:
// unflipped, R1 is self-sufficient and the arm let it through; flipped, the same
// arm would read BOTH legs as needing a partner, and a hold on both legs is a
// permanent mutual deadlock — both robots and the press out until someone
// cancels a leg (SYNTH-round2, 5/5 reviewers independently).
//
// THAT ARM NO LONGER EXISTS, and the deadlock was not theoretical: once Core
// admitted both legs in one pass, the arm wedged a plain unflipped pair the same
// way, and it was measured before it was deleted.
//
// The conclusion is unchanged and now unconditional. BOTH legs open with a
// stationWait either way, so dispatch parks two robots under two nodes and no
// bin moves until the operator releases. The physical collision needs one leg
// RELEASED while the other is not, and that is what the release layer's lift
// rule holds (release.PlanAct, G7, over Core's releasePoints). A held release
// goes by itself when the lift happens; a refused dispatch was a mutual wait.
func BuildTwoRobotPressIndexSwapSteps(claim *processes.NodeClaim) (orderR1, orderR2 []protocol.ComplexOrderStep) {
	if claim.PairedCoreNode == "" || claim.OutboundDestination == "" {
		return nil, nil
	}
	// R1's opening is the same either way: drive to the press, hold, lift the
	// full tote off, take it away.
	orderR1 = []protocol.ComplexOrderStep{
		stationWait(claim.CoreNodeName, release.PurposeSwap),
		{Action: "pickup", Node: claim.CoreNodeName},
		buildStep("dropoff", claim.OutboundDestination),
	}
	backfill := claim.PairedCoreNode
	if claim.SecondPairedCoreNode != "" {
		backfill = claim.SecondPairedCoreNode
	}
	if claim.IndexRobotFetches() {
		// FLIPPED: R1 stops at the destination; R2 indexes forward and then
		// goes for the replacement itself. One robot leaves the cell the
		// moment the press is clear.
		orderR2 = []protocol.ComplexOrderStep{
			stationWait(claim.PairedCoreNode, release.PurposeSwap),
			{Action: "pickup", Node: claim.PairedCoreNode},
			{Action: "dropoff", Node: claim.CoreNodeName},
		}
		if claim.SecondPairedCoreNode != "" {
			orderR2 = append(orderR2,
				protocol.ComplexOrderStep{Action: "pickup", Node: claim.SecondPairedCoreNode},
				protocol.ComplexOrderStep{Action: "dropoff", Node: claim.PairedCoreNode})
		}
		orderR2 = append(orderR2,
			refillPickup(nil, claim),
			protocol.ComplexOrderStep{Action: "dropoff", Node: backfill})
	} else {
		// UNFLIPPED (today): R1 carries on to the supermarket and backfills the
		// rearmost position; R2 only shifts the on-deck carrier forward.
		orderR1 = append(orderR1,
			refillPickup(nil, claim),
			protocol.ComplexOrderStep{Action: "dropoff", Node: backfill})
		orderR2 = []protocol.ComplexOrderStep{
			stationWait(claim.PairedCoreNode, release.PurposeSwap),
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
	// pickups Empty. That drops Core's payload filter (findAvailableForNeed:
	// claimPayload="") so the index fetches an on-deck EMPTY whichever part the
	// order names. It does NOT take a carrier stamped with a part: Core's
	// emptyBinsOnly keeps only a blank payload code, so a stamped on-deck carrier
	// holds the index leg under Empty as well (this comment said the opposite;
	// dispatch TestSpotPickup_HowTheStepFlagJudgesTheBin measures it). What hung
	// the Hopkinsville swap was the unflagged pickup filtering an on-deck empty
	// by the order's part. A CONSUME press-index would index
	// FULL bins forward, so it must NOT be flagged (and doesn't occur in
	// practice); scoping to produce also keeps the consume "full retrieve"
	// invariant. The changeover R2 flags its on-deck pickups the same way
	// (buildPressIndexChangeoverSwap). Produce-scoped INSIDE the builder.
	//
	// Under the flip R2 also carries the fetch, which refillPickup has already
	// flagged Empty for produce (and left a full retrieve for consume). This
	// flagger matches the on-deck nodes only and only ever SETS Empty, so the
	// two compose (TestPressIndexFlipped_BothEmptyFlaggersCompose).
	if claim.Role == protocol.ClaimRoleProduce {
		markPressIndexOnDeckEmpty(orderR2, claim)
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
		stationWait(claim.CoreNodeName, release.PurposeSwap), // 1 drive to node + hold
		{Action: "pickup", Node: claim.CoreNodeName},         // 2
		buildStep("dropoff", claim.OutboundDestination),      // 3
	}
}

// BuildSequentialBackfillSteps builds Order B for sequential mode (backfill robot).
// Robot picks up new material from source and delivers to line.
// Order B is auto-created by wiring when Order A goes in_transit.
//  1. pickup(InboundSource)    — pick new from source
//  2. dropoff(CoreNodeName)    — deliver to line
func BuildSequentialBackfillSteps(claim *processes.NodeClaim) []protocol.ComplexOrderStep {
	// Produce/consume are duals: a consume backfill pulls a payload-matched FULL
	// bin from the market, a produce backfill a fresh EMPTY carrier, and
	// refillPickup sets which. This leg is auto-created by the wiring
	// (handleSequentialBackfill) and never passes through BuildSwapDispatch.
	return []protocol.ComplexOrderStep{
		refillPickup(nil, claim),                      // 1
		{Action: "dropoff", Node: claim.CoreNodeName}, // 2
	}
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
	// CurtainWaitsA: StepsA lifts the old bin off its position behind its own
	// ready wait; at a curtained position the creation's standing intent rides
	// that wait (S7, createComplexFromSpec), and at any other position the
	// operator's RELEASE does. Its return drop gets none.
	CurtainWaitsA bool

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
	// See protocol.PlacesBinAt for the steps-based definition of each
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
// this claim" signal). Empty StepsA and StepsB cover the positional builders
// (a single-robot changeover onto a spare standing on inbound staging has
// StepsB alone: the spare needs no stage order); a nil Roles covers the
// role-declared two-leg builders.
func (d ChangeoverDispatch) rejected() bool {
	return d.StepsA == nil && d.StepsB == nil && d.Roles == nil
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
		return buildPressIndexChangeoverSwap(fromClaim, toClaim)
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
		return buildSingleRobotChangeoverSwap(fromClaim, toClaim)
	}
}

// buildSingleRobotChangeoverSwap is the single-robot changeover: ONE order.
// It fetches the incoming carrier and parks it on inbound staging, waits for
// the line, evacuates the old bin to outbound staging, collects the new one
// from inbound staging and delivers it, then clears the old one. A carrier that
// already stands on inbound staging (a keep-staged to-claim's right spare) is
// collected by the fetch itself, and there is nothing to park (stageSteps).
//
// THE COLLECT TAKES WHAT THIS ORDER PARKED (relayPickup), the two-robot
// supply's rule. It was a second order until 2026-10-16: a stage order parked
// the carrier and the swap order collected it with a pickup naming the
// incoming part, which Core sources like any pickup, so it took whatever bin
// of a carrier the part may ride stood on the node. With a stray empty on
// inbound staging, Core reserved it for the collect at the swap's dispatch,
// before the stage order could park there: the 08:31 shape (SPR ALN_011,
// 2026-10-15) in single-robot form, pinned by
// TestScenario_KeepStagedSingleRobot_TheCollectNeverTakesAStray. A relay names
// nothing and Core reads it as the carrier this plan set down. The fetch names
// the incoming part (stageSteps with fromClaim, as the two-robot supply does),
// because the order carries the outgoing payload for its lift off the line.
func buildSingleRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	stage, collect := stageSteps(fromClaim, toClaim, relayPickup(toClaim))
	return ChangeoverDispatch{
		StepsB:       append(stage, singleRobotChangeoverTail(fromClaim, toClaim, collect)...),
		AutoConfirmB: true,
	}
}

// singleRobotChangeoverTail is order B of a single-robot changeover from the
// bin standing on the to-claim's inbound staging: evacuate the line, park the
// old bin, collect the new one, deliver it, clear the old one.
func singleRobotChangeoverTail(fromClaim, toClaim *processes.NodeClaim, collect protocol.ComplexOrderStep) []protocol.ComplexOrderStep {
	stepsB := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName, release.PurposeReady), // drive to node + hold ("ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},          // evacuate old
		stagingDropoff(fromClaim.OutboundStaging),                 // park old
	}
	return append(stepsB,
		collect, // grab new
		protocol.ComplexOrderStep{Action: "dropoff", Node: toClaim.CoreNodeName},     // deliver new
		protocol.ComplexOrderStep{Action: "pickup", Node: fromClaim.OutboundStaging}, // grab old
		buildStep("dropoff", fromClaim.OutboundDestination),                          // clear old to final
	)
}

// buildTwoRobotChangeoverSwap mirrors BuildTwoRobotSwapSteps adapted for
// changeover: from-claim drives the outbound side, to-claim drives the
// inbound side. The "ready" wait is the operator gate that releases both
// robots; once released, Robot B clears the line while Robot A delivers
// the new bin. It has no Tooling done wait: a tool change at this node is the
// decorator's (applyToolingChangeover), which replaces this pair with the
// tool change's one leg.
func buildTwoRobotChangeoverSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if toClaim.InboundStaging == "" {
		return ChangeoverDispatch{}
	}
	// The incoming carrier is fetched (EMPTY on produce, a payload-matched full
	// retrieve on consume) and staged, then the shared "ready" release carries it
	// to the line.
	prefix, collect := stageSteps(fromClaim, toClaim, relayPickup(toClaim))
	stepsA := append(prefix,
		twoRobotSupplyTail(stationWait(toClaim.InboundStaging, release.PurposeReady), collect,
			toClaim.CoreNodeName)...)
	return twoRobotChangeoverLegs(fromClaim, stepsA)
}

// twoRobotChangeoverLegs pairs a two-robot changeover supply with the evac that
// clears the from-claim's line.
func twoRobotChangeoverLegs(fromClaim *processes.NodeClaim, stepsA []protocol.ComplexOrderStep) ChangeoverDispatch {
	stepsB := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName, release.PurposeReady), // drive to node + hold (shared "ready")
		{Action: "pickup", Node: fromClaim.CoreNodeName},          // evacuate old
		buildStep("dropoff", fromClaim.OutboundDestination),       // straight to final
	}
	return ChangeoverDispatch{
		Roles: &changeoverSwapLegs{
			// stepsA fetches new material and delivers it to the line — supply.
			//
			//
			// AUTO-CONFIRM HERE IS CORRECT AND IS NOT confirmPolicy's QUESTION.
			// stepsA ends with a dropoff at the process node, so confirmPolicy
			// would call it the leg that needs a receipt. The changeover path
			// does not use confirmPolicy at all.
			//
			// The reason used to be given as "the shared 'ready' wait above IS
			// the operator's gate", and that is wrong about what the wait does.
			// The wait is a RELEASE: it holds both robots at their staging points
			// until the operator starts them, and it fires before either carrier
			// reaches the press. It signs for nothing.
			//
			// The receipt on a changeover is the CUTOVER, plus openPostCutoverVerify
			// after it — which watches the press's live CATID against the incoming
			// style and puts a mismatch in front of the operator. That is a
			// supervised event with a person present, and a per-leg CONFIRM tap
			// would be a second signature on the same act. Do not route these legs
			// through confirmPolicy.
			supply: changeoverLeg{steps: stepsA, deliveryNode: finalDropoff(stepsA), autoConfirm: true},
			// stepsB waits at the line, lifts the old bin, carries it to
			// outbound — evac. Core derives its delivery node from the steps.
			// It leaves nothing on the press, so this one agrees with
			// confirmPolicy anyway.
			evac: changeoverLeg{steps: stepsB, autoConfirm: true},
		},
	}
}

// buildPressIndexChangeoverSwap mirrors BuildTwoRobotPressIndexSwapSteps —
// R1 evacuates from CoreNodeName and reloads the back position from
// InboundSource; R2 indexes intermediate positions. Honors 2-pos vs 3-pos
// via fromClaim.SecondPairedCoreNode (using fromClaim's geometry is
// consistent with the swap_dispatch pattern).
func buildPressIndexChangeoverSwap(fromClaim, toClaim *processes.NodeClaim) ChangeoverDispatch {
	if fromClaim.PairedCoreNode == "" || fromClaim.OutboundDestination == "" {
		return ChangeoverDispatch{}
	}
	// R1 prefix is identical for 2-pos and 3-pos: wait, evac, dropoff destination.
	r1 := []protocol.ComplexOrderStep{
		stationWait(fromClaim.CoreNodeName, release.PurposeReady),
		{Action: "pickup", Node: fromClaim.CoreNodeName},
		buildStep("dropoff", fromClaim.OutboundDestination),
	}
	// ── THE FLIP APPLIES HERE TOO ────────────────────────────────────────
	//
	// RefillBy moves the supermarket trip between the two robots.
	// The steady-state builder reads it; this one did not, so a press
	// configured with the flip ran R2-supplies all shift and then inverted the
	// two robots' roles the moment a changeover started — same cell, same
	// hardware, opposite choreography, decided by which builder happened to be
	// running.
	//
	// The flip does NOT move the press pickup or the press dropoff, only the
	// fetch, which is why R1's prefix above and R2's index moves below are
	// identical in both states.
	backPosition := fromClaim.PairedCoreNode
	if fromClaim.SecondPairedCoreNode != "" {
		backPosition = fromClaim.SecondPairedCoreNode
	}
	// R1's other pickup — the old front tote — keeps the from-style payload it
	// needs (§ carriesFromPayload below); only the fetching leg takes a fresh
	// carrier.
	if !fromClaim.IndexRobotFetches() {
		r1 = append(r1, refillPickup(fromClaim, toClaim))
		r1 = append(r1, protocol.ComplexOrderStep{Action: "dropoff", Node: backPosition})
	}
	// R2's on-deck pickups are the steady-state R2's: Empty on a produce press,
	// whose on-deck holds the empty the index carries to the press, and naming
	// nothing on a consume one, whose index takes stock of the part the leg
	// carries on the order, the outgoing style's (carriesFromPayload below).
	// Core binds what the step says it lifts (ground rule 5), and it keeps a
	// changeover's leg out of its vacant-pickup fetch by the leg's episode
	// (dispatch reanchorVacantPickup), so the step says nothing else.
	var r2 []protocol.ComplexOrderStep
	if fromClaim.SecondPairedCoreNode != "" {
		r2 = []protocol.ComplexOrderStep{
			stationWait(fromClaim.PairedCoreNode, release.PurposeReady),
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
			{Action: "pickup", Node: fromClaim.SecondPairedCoreNode},
			{Action: "dropoff", Node: fromClaim.PairedCoreNode},
		}
	} else {
		r2 = []protocol.ComplexOrderStep{
			stationWait(fromClaim.PairedCoreNode, release.PurposeReady),
			{Action: "pickup", Node: fromClaim.PairedCoreNode},
			{Action: "dropoff", Node: fromClaim.CoreNodeName},
		}
	}
	if fromClaim.Role == protocol.ClaimRoleProduce {
		markPressIndexOnDeckEmpty(r2, fromClaim)
	}
	if fromClaim.IndexRobotFetches() {
		// Flipped: R2 owns the supermarket trip, exactly as it does in steady
		// state. It has just placed on the press, so the fresh carrier it
		// fetches goes to the back position behind it.
		r2 = append(r2, refillPickup(fromClaim, toClaim))
		r2 = append(r2, protocol.ComplexOrderStep{Action: "dropoff", Node: backPosition})
	}
	return ChangeoverDispatch{
		Roles: &changeoverSwapLegs{
			// R1 lifts the spent tote off the front — evac — and refills the
			// back when it owns the fetch. Its delivery node is left blank for
			// Core to derive from the steps; the old hardcoded
			// fromClaim.CoreNodeName was a lie — R1's bin comes to rest at the
			// back (or at outbound, flipped), not the front, which is what bound
			// the wrong bin at HK 2026-07-14.
			evac: changeoverLeg{steps: r1, autoConfirm: true},
			// R2 indexes the fresh tote onto the front — supply. It picks up an
			// OLD (from-style) tote at the back position, so it carries the
			// from-style payload: a consume index takes stock of it there, and
			// without it would ask for the new payload and find no bin
			// (ALN_001), the same defect the evac slot already guards on
			// two_robot. A produce index is Empty and takes the empty whatever
			// part the order names.
			//
			// R2 drops on the press in both geometries, so confirmPolicy would
			// want a receipt here. It auto-confirms for the reason given at the
			// two_robot builder above: the changeover's receipt is the cutover
			// and its post-cutover verify, not a per-leg tap.
			supply: changeoverLeg{steps: r2, deliveryNode: finalDropoff(r2), autoConfirm: true, carriesFromPayload: true},
		},
	}
}

// buildPressIndexPerPositionSwap is the per-position dispatch for a
// synthesized press-index different-bin-type fan-out claim. Single
// complex order, 5 steps, released at the position like every lift off a
// line position:
//
//	wait(my position)         ready: the release (the ingest ships; Core chooses the drop)
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
		// Every lift off a line position goes through its release: the one
		// place the count is final, the produce ingest ships and Core chooses
		// the drop. The same ready wait the press's R1 lift opens with.
		stationWait(pos, release.PurposeReady),
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
		CurtainWaitsA: true,
		// The opening pickup lifts the OLD bin off the position, so the order
		// carries the from-style payload. Safe alongside the Empty refill above,
		// whose own payload filter is dropped.
		CarriesFromPayloadA: true,
		StepsB:              nil,
	}
}

// buildToolingEvacSteps is a tool change's one leg at a marked position: take
// the carrier that blocks the tool off the line at START, put the replacement
// where it waits, and deliver it when the operator says the tool is done.
//
//	pickup(position)      lift the carrier that blocks the tool
//	dropoff(evacDest)     get it off the line
//	pickup(inboundSource) fetch the incoming style's carrier
//	wait(staging)         the Tooling done gate, AT inbound staging
//	dropoff(position)     deliver on release
//
// The wait is at a node, so the robot drives there holding the carrier and
// parks out of the cell: a tool change can take a shift, and robots idling on
// the line apron for that long block the very access the millwrights need, and
// release is then a short move in rather than a full fetch. Every line mode
// takes this one shape (toolingPositionAction); the save requires inbound
// staging on a claim with marks.
//
// It takes the CLAIMS rather than a bare inbound-source name so its refill leg
// goes through refillPickup like every other one: a name carries the node but
// not the produce/empty question.
func buildToolingEvacSteps(position, evacDest string, fromClaim, toClaim *processes.NodeClaim, staging string) []protocol.ComplexOrderStep {
	return []protocol.ComplexOrderStep{
		{Action: "pickup", Node: position},
		buildStep("dropoff", evacDest),
		refillPickup(fromClaim, toClaim),
		stationWait(staging, release.PurposeToolingDone),
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
		stationWait(pos, release.PurposeReady),
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
