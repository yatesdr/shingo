package engine

import (
	"fmt"
	"log"
	"sync"
	"time"

	"shingo/protocol"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
)

// RequestProduceSwap (formerly FinalizeProduceNode — the /finalize route
// keeps its name) dispatches the order(s) to remove the filled bin and bring
// the next empty. Builds a ProducePlan (pure validation + dispatch shape)
// and then applies it. Swap-mode dispatch shape is shared with consume
// via SwapDispatch — the robot doesn't care whether the bin is filling
// or emptying, the choreography is the same.
//
// This call only REQUESTS robots, in every mode. The count splits at the
// operator's RELEASE (owner, 2026-09-30): the manifest, the ingest and the
// slot clear — the actual "finalize" — happen at the release of the leg that
// takes the bin away (finalizeDepartingProduce), because every part pressed
// between this call and RELEASE still belongs to the departing bin.
// RequestProduceSwap is the OPERATOR entry point for the evacuate direction —
// the mirror of RequestNodeMaterial, with the same dual-trigger shape. Tick-
// driven callers use requestProduceSwapFor with the autoreorder trigger.
func (e *Engine) RequestProduceSwap(nodeID int64) (*NodeOrderResult, error) {
	return e.requestProduceSwapFor(nodeID, protocol.EpisodeTriggerOperator)
}

func (e *Engine) requestProduceSwapFor(nodeID int64, trigger string) (*NodeOrderResult, error) {
	node, runtime, claim, err := loadActiveNode(e.db, nodeID)
	if err != nil {
		return nil, err
	}

	// A2 (hop 2026-07-23): refuse outgoing-style relief while a changeover is
	// armed on this process — don't let a produce swap race the cutover.
	if err := e.guardStyleTransition(node, claim); err != nil {
		return nil, err
	}
	// A5 (hop 2026-07-23): refuse outgoing-style relief when the press's live
	// CATID says the wrong part is physically on it — the ground-truth sibling
	// of the changeover guard above.
	if err := e.guardCatidMismatch(node, claim); err != nil {
		return nil, err
	}

	// The partial-empty prime reads two things and then writes: what is
	// physically on the cell's positions, and what empties are already on
	// their way to them. Both reads and the create that follows sit inside one
	// per-cell lock so a double-tap cannot fire two empties at one bare
	// position. Cheap: these are operator clicks and autoreorder ticks, not a
	// hot path.
	mu := e.primeNodeLock(claim)
	mu.Lock()
	defer mu.Unlock()

	occupancy := e.occupancyKnownNodesOnly(e.claimOccupancy(claim), node.Name)
	primedPositions, err := e.pairedPositionsAlreadyPrimed(node, claim)
	if err != nil {
		return nil, err
	}

	// See swap_evac_dest.go: the outgoing carrier goes to ITS home, not the
	// requested style's. Blank override = today's behaviour.
	swapClaim := withResidentEvacDest(claim, e.residentEvacDest(runtime, claim))

	plan, err := BuildProducePlan(node, runtime, swapClaim, occupancy, primedPositions)
	if err != nil {
		return nil, err
	}
	if plan.SuppressSwap {
		if len(plan.PrimePairedPositions) == 0 {
			// HOLD: every bare position already has an empty on the way. Refuse
			// BEFORE the episode opens — an episode with expected_orders 0 is
			// noise, and the operator needs a sentence, not a silent success.
			//
			// TYPED, because this refusal is the system working. Rendered as a
			// red error it reads as a fault the operator has to do something
			// about, and the only correct response is to wait. The type is what
			// lets the station render it as a notice instead.
			return nil, &PrimeInFlightError{NodeName: node.Name}
		}
		dests := make([]string, 0, len(plan.PrimePairedPositions))
		for _, p := range plan.PrimePairedPositions {
			dests = append(dests, p.Dest)
		}
		log.Printf("[produce-swap] node %s: head occupied, paired %v bare — priming from %s, no swap this round",
			node.Name, dests, claim.InboundSource)
	}

	// Bug 3 guard: refuse to start a second swap on top of an in-flight one.
	// Edge-runtime-only — Core anomalies don't shut down the line.
	if plan.Dispatch != nil && plan.Dispatch.RequiresActiveSwapGuard {
		if err := e.guardNoActiveSwap(node, runtime, claim); err != nil {
			return nil, err
		}
	}

	// The evacuate-direction episode, opened after the plan exists and before
	// any order does — same ordering and same reasoning as the consume side.
	//
	// A primes-only round opens the episode HERE TOO, deliberately. A prime is
	// a supply move and this is nominally the evacuate entry point, but the
	// cell episode is direction-agnostic by design (see openEpisodeForProduce
	// below): this path and RequestEmptyBin already join the ONE episode for
	// this cell's circle rather than opening a row each. Splitting a supply
	// episode out for the primes-only round would re-create exactly the
	// two-rows-for-one-cell shape that change removed. expected_orders comes
	// from ProducePlan.OrderCount, which counts the primes.
	//
	// A produce node's level runs the OTHER WAY: it fills toward capacity
	// rather than draining toward a reorder point, so "needs attention" is a
	// HIGH reading. The episode still means one thing — this process needs
	// material moved, in this direction — which is why direction is part of the
	// episode key and not a separate kind.
	origin := e.openEpisodeForProduce(node, runtime, claim, plan, trigger)

	return e.applyProducePlan(node, runtime, claim, plan, origin)
}

// PrimeInFlightError says a press-index swap was refused because the empty it
// needs is already on its way. It is ADVISORY: nothing is wrong, nothing needs
// fixing, and the next press of the button after the bin lands will run the
// swap.
//
// A distinct type rather than a message the UI matches on. The station has to
// decide a colour, and deciding it by substring is how a reworded sentence
// silently turns an all-clear back into a red alarm.
type PrimeInFlightError struct {
	NodeName string
}

func (e *PrimeInFlightError) Error() string {
	return fmt.Sprintf("node %s: an empty bin is already inbound to the index position — "+
		"the swap will run once it lands", e.NodeName)
}

// Advisory reports that this refusal is the system behaving correctly rather
// than a fault. The handler keys on the behaviour, not on the concrete type,
// so a second advisory refusal later needs no handler change.
func (e *PrimeInFlightError) Advisory() bool { return true }

// primeBarePressIndexPositions is the partial-empty prime for callers that do
// NOT go through BuildProducePlan — today that is RequestEmptyBin, which
// reaches BuildSwapDispatch directly.
//
// Returns (primes, suppressed, err). suppressed=true means this round minted
// the primes and the caller must NOT build a swap; suppressed=false with a nil
// error means the cell is whole and the caller carries on as before. A
// suppressed round can still carry an error — the no-inbound-source refusal and
// the advisory PrimeInFlightError both mean "no swap this round" too.
//
// WHY THIS IS A SECOND IMPLEMENTATION AND NOT A CALL INTO BuildProducePlan:
// that function is a pure planner over a (node, runtime, claim) triple and its
// UOP guard refuses a cell with nothing counted, which is every cell at
// Springfield — the counter tag is not wired, so RemainingUOPCached reads 0
// forever. Routing REQUEST EMPTY BIN through it would trade a missing guard for
// a guaranteed refusal. The predicate below is the same one, lifted out of it;
// the two must not drift, which is what TestRequestEmptyBin_PrimesBarePosition
// and produce_swap_test.go's prime cases pin from either side.
//
// The lock, both reads, and the create sit inside one critical section for the
// same reason the produce path does it: a double-tap must not fire two empties
// at one bare position.
func (e *Engine) primeBarePressIndexPositions(
	node *processes.Node, claim *processes.NodeClaim, origin ordermgr.Origin,
) (primes []*orders.Order, suppressed bool, err error) {
	if claim == nil || claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil, false, nil
	}
	mu := e.primeNodeLock(claim)
	mu.Lock()
	defer mu.Unlock()

	occupancy := e.occupancyKnownNodesOnly(e.claimOccupancy(claim), node.Name)
	// A BARE HEAD IS A DIFFERENT SHAPE and not this function's to answer: with
	// nothing on the press there is nothing to index forward, and the consume
	// side's node-empty downgrade owns that case. Matching BuildProducePlan's
	// precondition exactly keeps the two from disagreeing about which shape
	// they each handle.
	if !isOccupied(occupancy, claim.CoreNodeName) {
		return nil, false, nil
	}
	primedPositions, perr := e.pairedPositionsAlreadyPrimed(node, claim)
	if perr != nil {
		return nil, false, perr
	}
	var bare, needsPrime []string
	for _, pos := range claim.ExtensionPositions() {
		if isOccupied(occupancy, pos) {
			continue
		}
		bare = append(bare, pos)
		if !primedPositions[pos] {
			needsPrime = append(needsPrime, pos)
		}
	}
	if len(bare) == 0 {
		return nil, false, nil
	}
	// SUPPRESSED FROM HERE DOWN, on every arm. A position that is physically
	// bare cannot be indexed from whether or not the empty filling it is
	// already on its way, so the swap stays suppressed for as long as the
	// position reads empty and only the duplicate ORDER is skipped. Releasing
	// the swap on the second tap of a double-tap would hand it exactly the
	// un-sourceable leg this exists to prevent.
	if len(needsPrime) == 0 {
		return nil, true, &PrimeInFlightError{NodeName: node.Name}
	}
	if claim.InboundSource == "" {
		return nil, true, fmt.Errorf("node %s has no inbound source configured", node.Name)
	}
	autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
	nodeID := node.ID
	for _, pos := range needsPrime {
		po, cerr := e.orderMgr.CreateRetrieveOrder(&nodeID, true, 1,
			pos, claim.InboundSource, "", "standard", claim.PayloadCode,
			autoConfirm, false, origin)
		if cerr != nil {
			// Partial success is still suppression: whatever was created is on
			// its way, and minting a swap on top of a half-primed cell is the
			// original bug with fewer steps.
			return primes, true, fmt.Errorf("prime %s: %w", pos, cerr)
		}
		primes = append(primes, po)
	}
	log.Printf("[request-empty] node %s: head occupied, paired %v bare — priming from %s, no swap this round",
		node.Name, needsPrime, claim.InboundSource)
	return primes, true, nil
}

// primeNodeLock returns the per-cell prime mutex, creating it on first use.
// Keyed by the claim's CORE node name so every process_node row that shares
// one physical cell serialises against the same lock — the in-flight count it
// protects is itself scoped by delivery node, and a shared core node carries
// many process_node rows for one slot.
func (e *Engine) primeNodeLock(claim *processes.NodeClaim) *sync.Mutex {
	key := ""
	if claim != nil {
		key = claim.CoreNodeName
	}
	m, _ := e.primeResv.LoadOrStore(key, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// pairedPositionsAlreadyPrimed reports which of the claim's paired positions
// already have a non-terminal empty inbound, so a second request while the
// first prime is still travelling adds nothing. Reuses the same in-flight
// count RequestEmptyBin uses for its one-slot anti-spam guard, scoped by
// delivery node for the same reason.
//
// FAILS CLOSED. A read error means we do not know what is inbound, and
// priming on that is how a position collects a carrier it has no room for; a
// refused request is a click the operator can repeat.
func (e *Engine) pairedPositionsAlreadyPrimed(node *processes.Node, claim *processes.NodeClaim) (map[string]bool, error) {
	if claim == nil || claim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil, nil
	}
	primed := map[string]bool{}
	for _, pos := range claim.ExtensionPositions() {
		n, err := e.countActiveOrdersAtNode(pos, func(o orders.Order) bool { return o.RetrieveEmpty })
		if err != nil {
			return nil, fmt.Errorf("node %s: check inbound empties at paired position %s: %w", node.Name, pos, err)
		}
		if n > 0 {
			primed[pos] = true
		}
	}
	return primed, nil
}

// occupancyKnownNodesOnly re-reads an "empty" telemetry answer as occupied
// when the node it names is not one Core knows.
//
// Core reports an unknown node as a PRESENT entry with Occupied=false
// (shingo-core/www/handlers_telemetry.go) — "there is no bin at a place that
// does not exist", which is true and is the wrong sentence to prime on. A
// typo'd or scenesync-reaped PairedCoreNode would read bare forever and take a
// carrier every cycle. The missing-entry default already covers a node Core
// declined to answer about; this covers the node Core answered about and does
// not have.
//
// AND IT MUST KNOW WHETHER IT HAD THE INPUT TO CHECK. An EMPTY node set is not
// evidence that a name is wrong — a fresh Edge, a restart or a Kafka gap all
// present that way, and Core answers node-bins over a different transport than
// the node-list sync. So an empty set SKIPS the check and says so, rather than
// suppressing every prime during the startup window and handing the cell back
// the un-sourceable swap this whole path exists to prevent. Same reading as
// coreNodeNameIsUnknown in www/handlers_process_nodes.go.
func (e *Engine) occupancyKnownNodesOnly(occ map[string]bool, nodeName string) map[string]bool {
	known := e.CoreNodes()
	if len(known) == 0 {
		log.Printf("[produce-swap] node %s: core node list is EMPTY, so paired positions could not be "+
			"checked against Core's plant — reading telemetry as-is. This is not a pass: Core has "+
			"not been heard from.", nodeName)
		return occ
	}
	out := make(map[string]bool, len(occ))
	for name, occupied := range occ {
		out[name] = occupied
		if occupied || coreNodeKnown(known, name) {
			continue
		}
		log.Printf("[produce-swap] node %s: position %q is not a node Core knows (%d known) — reading it "+
			"as occupied, no prime. Check the spelling against the node picker, or sync nodes if Core "+
			"has just been reconfigured.", nodeName, name, len(known))
		out[name] = true
	}
	return out
}

// coreNodeKnown resolves a name against Core's synced node set, falling back
// to the bare child name. Core sends group children as "Group.CHILD";
// SetCoreNodes normally trims that on ingestion, but keeps the qualified form
// when two children collide on one bare name.
func coreNodeKnown(known map[string]protocol.NodeInfo, name string) bool {
	if _, ok := known[name]; ok {
		return true
	}
	for full := range known {
		if bareNodeName(full) == name {
			return true
		}
	}
	return false
}

// openEpisodeForProduce opens or joins the evacuate-direction episode.
//
// Best-effort: observability must never fail an evacuation. A full bin gets
// taken away whether or not we could record why.
func (e *Engine) openEpisodeForProduce(
	node *processes.Node, runtime *processes.RuntimeState,
	claim *processes.NodeClaim, plan *ProducePlan, trigger string,
) ordermgr.Origin {
	remaining := 0
	if runtime != nil {
		remaining = runtime.RemainingUOPCached
	}
	// DISCRETIONARY on this side means an operator called a swap on a bin the
	// system does not consider full. Same rule as the consume side: flag it,
	// conclude nothing. The count may be wrong, the capacity may be wrong, or
	// the operator may be clearing the line for a reason the system cannot see.
	discretionary := trigger == protocol.EpisodeTriggerOperator &&
		claim.UOPCapacity > 0 && remaining < claim.UOPCapacity

	// No direction argument — see openCellEpisode. This path's claim is a produce
	// one, and RequestEmptyBin's claim is the SAME produce claim: both now open
	// or join the one episode for this cell's circle, where before this site
	// opened an evacuate row and that one opened a supply row for the same cell.
	originID, _, err := e.openCellEpisode(
		node.ProcessID, claim, trigger,
		plan.OrderCount(), remaining, discretionary,
	)
	if err != nil {
		e.logFn("demand_episode: open %s episode node=%s: %v", claim.Role, node.Name, err)
	}
	if originID == "" {
		// No episode, so nothing to attach to. Say NOTHING rather than
		// guessing: an unstated class lets Core classify, where a wrong one
		// here would be indistinguishable from a real answer.
		return ordermgr.Origin{}
	}
	return ordermgr.Attached(originID)
}

// applyProducePlan is the impure half of the produce-finalize pipeline:
// it manifests the filled bin, dispatches the planned complex orders,
// resets node UOP, and re-reads the resulting orders. Direction-specific
// glue around the shared SwapDispatch.
func (e *Engine) applyProducePlan(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim, plan *ProducePlan, origin ordermgr.Origin) (*NodeOrderResult, error) {
	nodeID := node.ID

	// Primes-only round: fill the bare paired position(s) and mint nothing
	// else. No manifest (there is no departing bin), no dispatch, and NO
	// runtime order slots — those belong to the head node's serial-order
	// machinery for swap cycles, and a prime is not a swap. Same treatment the
	// consume side's downgrade primes get.
	//
	// A retrieve, not a move: a move is a full-intent local relocation of the
	// bin AT a concrete source node, so it would hunt a FULL bin in what is an
	// empties pool. RetrieveEmpty is the intent that matches.
	if plan.SuppressSwap {
		// The merged signal, not a hard-coded true: one auto-confirm policy for
		// both directions of this cell.
		autoConfirm := claim.AutoConfirm || e.cfg.Web.AutoConfirm
		var primes []*orders.Order
		for _, p := range plan.PrimePairedPositions {
			// No re-read: CreateRetrieveOrder already returns the stored row and
			// nothing below rewrites it.
			po, err := e.orderMgr.CreateRetrieveOrder(&nodeID, true, 1,
				p.Dest, p.Source, "", "standard", claim.PayloadCode,
				autoConfirm, false, origin)
			if err != nil {
				return nil, fmt.Errorf("prime %s: %w", p.Dest, err)
			}
			primes = append(primes, po)
		}
		return &NodeOrderResult{PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	// No paperwork here, in any mode: the bin keeps filling until the
	// operator's RELEASE, which finalizes it (finalizeDepartingProduce). The
	// runtime ORDER pointers stamp below (release resolution and the
	// swap_ready gate depend on them).
	// Produce always has a swap mode now (BuildProducePlan errors otherwise), so
	// Dispatch is always set.
	dispatch := plan.Dispatch
	// BOTH UUIDs BEFORE EITHER CREATE. Each leg goes in already naming its
	// partner, so neither is ever unpaired and creation order stops being a
	// correctness input — see CreateComplexOrderPaired.
	//
	// Creation ORDER is unchanged and still deliberate: the outbox drains
	// strictly ORDER BY id, so A is the leg Core is asked for first.
	uuidA, uuidB := ordermgr.NewOrderUUID(), ""
	if dispatch.StepsB != nil {
		uuidB = ordermgr.NewOrderUUID()
	}
	sibA, sibB := coreSiblings(dispatch.StepsA, dispatch.StepsB, uuidA, uuidB)
	orderA, err := e.dispatchPairedLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, sibA, uuidA, origin)
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		orderB, err = e.dispatchPairedLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, sibB, uuidB, origin)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, &orderA.ID, orderBID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
	if orderB != nil {
		// Return-error on failure: see comment in
		// operator_stations.go:LinkOrderSiblings call site.
		if err := e.db.LinkOrderSiblings(orderA.ID, orderB.ID); err != nil {
			return nil, fmt.Errorf("link order siblings %d↔%d: %w", orderA.ID, orderB.ID, err)
		}
	}

	orderA, err = e.refreshOrder(orderA.ID)
	if err != nil {
		return nil, err
	}
	if orderB != nil {
		orderB, err = e.refreshOrder(orderB.ID)
		if err != nil {
			return nil, err
		}
	}

	if orderB == nil {
		return &NodeOrderResult{Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// finalizeDepartingProduce is the one finalize of a produce bin, run at the
// operator's RELEASE of the leg that takes the bin away, in every mode and at
// every door (the pair click, the per-order trunk, the changeover's clicks,
// the sweep). The count splits at that press (owner, 2026-09-30): parts made
// before it, including after the call for parts, belong to the departing bin;
// parts made after it belong to the next one. It is the operator's
// declaration, so it stands through a Core refusal of the release.
//
// Three steps, in this order:
//  1. flush the accumulator, so the ticks of the last window reach Core
//     before the ingest bumps the bin's epoch (no stale-epoch drops);
//  2. queue the ingest: the Edge's count, the bin and its epoch, and the
//     departing ORDER's payload (for a changeover evac that is the outgoing
//     style's, which the claim no longer names). No manifest lines: Core
//     resolves the payload's template and stamps this count;
//  3. clear the slot (carrierLeft), so later ticks hold and replay onto the
//     next bin.
//
// ONCE PER DEPARTING ORDER. The ingest is recorded against the departing leg
// in release_paperwork, and a second door, click or re-fire for the same
// order ships nothing. A zero count ships nothing either.
func (e *Engine) finalizeDepartingProduce(node *processes.Node, runtime *processes.RuntimeState, departing *orders.Order, placingOrderID *int64) error {
	if runtime == nil || runtime.RemainingUOPCached <= 0 {
		e.logFn("produce release: node %s remaining=%d — no release-time manifest to stamp",
			node.Name, runtimeRemaining(runtime))
		return nil
	}
	shipped, err := e.db.ReleaseIngestShipped(departing.ID)
	if err != nil {
		return fmt.Errorf("node %s: %w", node.Name, err)
	}
	if shipped {
		e.logFn("produce release: node %s order %d — its ingest already shipped; not again",
			node.Name, departing.ID)
		return nil
	}
	payload := departing.PayloadCode
	if payload == "" {
		if resident, rerr := e.residentClaim(node, runtime); rerr == nil && resident != nil {
			payload = resident.PayloadCode
		}
	}
	qty := int64(runtime.RemainingUOPCached)
	var binID int64
	if runtime.ActiveBinID != nil {
		binID = *runtime.ActiveBinID
	}
	if e.inventoryDelta != nil {
		e.inventoryDelta.Flush()
	}
	// Quantity is the CYCLE count, not a part count: Core writes it to
	// uop_remaining, and the part count is uop_remaining x the template's
	// parts_per_cycle.
	if err := e.orderMgr.QueueIngestManifest(
		payload, "", binID, runtime.ActiveBinEpoch, node.CoreNodeName, qty, nil,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("queue release-time ingest for node %s: %w", node.Name, err)
	}
	if err := e.db.MarkReleaseIngestShipped(departing.ID, binID, qty); err != nil {
		e.logFn("produce release: node %s order %d — ingest shipped but not recorded (%v); a second door may ship it again",
			node.Name, departing.ID, err)
	}
	// ── gate: is the bound bin the one LEAVING, or the one that just
	// ARRIVED? ───────────────────────────────────────────────────────────
	//
	// The clear below opens the hold-and-replay window on the premise that
	// the slot's active bin is the DEPARTING bin. Under press-index that
	// premise can already be false at the tap: the index leg
	// auto-dispatches at creation and binds its bin through the delivery
	// handler, so by the time the operator clicks, the active bin can be
	// the one the press is filling INTO. Clearing then erases a bin that
	// is standing on the press mid-production; nothing rebinds it (the
	// uop_adjustment anti-ghost guard and bin_epoch_refresh both decline
	// by design) and SimMachineReady gates the machine off permanently —
	// sim 2026-09-07, PLN_001 dead at counter 66 for the rest of the run.
	//
	// Discriminate by bin identity, not timing. The evac leg Edge cannot
	// name a bin for (its bin_id is set late, on the Core side), but the
	// PLACING leg carries the bin it delivered, and the delivery handler
	// is the only binder of active_bin_id. If the placing order's bin is
	// the one bound, the count belongs to the NEXT cycle — there is no
	// hold window to open, because the next bin is already on the
	// position and ticks keep landing on it. In plain two_robot the
	// supply leg parks at a staging node and never binds here, so the
	// match cannot fire; the classic clear is untouched.
	//
	// Ambiguity breaks toward NOT destroying state: a placing order that
	// is terminal but carries no bin_id cannot be ruled out as the binder,
	// so the clear is skipped and a breadcrumb logged. A clear that
	// should have fired costs a few ticks landing on the departing bin
	// before pickup (bounded, replays at the next cycle); a clear that
	// should not have fired costs the press.
	//
	// The ingest above has already shipped in that state, with the count of
	// the bin that is bound; the gate is latent at the pair door today (both
	// press-index legs wait, and R2 places only after R1 lifts) and S4
	// decides it.
	if placingOrderID != nil {
		if placing, err := e.db.GetOrder(*placingOrderID); err == nil {
			if placing.BinID != nil && runtime.ActiveBinID != nil &&
				*placing.BinID == *runtime.ActiveBinID {
				e.logFn("produce release: node %s active bin %d is the bin order %d PLACED here — not the departing one; keeping the slot bound",
					node.Name, *runtime.ActiveBinID, placing.ID)
				return nil
			}
			if placing.BinID == nil && ordermgr.IsTerminalSuccess(placing.Status) {
				e.logFn("produce release: node %s placing order %d is terminal with no bin on the row — skipping the clear rather than risk erasing a bin standing on the position",
					node.Name, placing.ID)
				return nil
			}
		}
	}
	// The count now belongs to the departing bin: clear the slot, so the
	// hold-and-replay window starts HERE. Log-only on failure: the ingest
	// already shipped.
	if err := e.carrierLeft(node.ID, node.CoreNodeName); err != nil {
		log.Printf("produce release: clear active bin for node %d: %v", node.ID, err)
	}
	return nil
}

// departingProduceLeg reports whether releasing this order takes a produce
// bin off the node: the segment after the leg's first station wait lifts a
// bin at the node, and the bin standing there was filled for a produce claim
// (residentClaim). Read from the leg's steps, never from SwapMode or
// SiblingOrderID, so every mode and every door answers it the same way. An
// unreadable answer is an error: the caller refuses rather than ship or skip
// a bin's count on a guess.
//
// Once per leg per act: the pair door asks it of the evac, and the trunk the
// door releases the evac through asks again from the memo. known is the claim
// the caller already resolved for the node; it answers for the resident when
// it is the runtime's active claim, which saves the read.
func (e *Engine) departingProduceLeg(act *releaseAct, orderID int64, node *processes.Node, runtime *processes.RuntimeState, known *processes.NodeClaim) (bool, error) {
	if node == nil || runtime == nil {
		return false, nil
	}
	if d, ok := act.departs[orderID]; ok {
		return d, nil
	}
	// The bin's claim first: a consume bin never has a count to finalize, and
	// when the caller's claim is the resident one this costs no read.
	resident := known
	if resident == nil || runtime.ActiveClaimID == nil || resident.ID != *runtime.ActiveClaimID {
		var err error
		if resident, err = e.residentClaim(node, runtime); err != nil {
			return false, fmt.Errorf("departing-bin check: node %s: %w", node.Name, err)
		}
	}
	departs := false
	if resident != nil && resident.Role == protocol.ClaimRoleProduce {
		steps, err := e.legSteps(act, orderID)
		if err != nil {
			return false, fmt.Errorf("departing-bin check: %w", err)
		}
		departs = segmentAfterFirstStationWaitLifts(steps, node.CoreNodeName)
	}
	if act.departs == nil {
		act.departs = map[int64]bool{}
	}
	act.departs[orderID] = departs
	return departs, nil
}

// residentClaim is the claim the bin standing on the node was filled for: the
// runtime's active claim, which a changeover does not move until the incoming
// material lands, or — on a node's first cycle, before one was stamped — the
// node's current claim. nil when neither resolves.
func (e *Engine) residentClaim(node *processes.Node, runtime *processes.RuntimeState) (*processes.NodeClaim, error) {
	if runtime != nil && runtime.ActiveClaimID != nil {
		c, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
		if err != nil {
			return nil, fmt.Errorf("read claim %d: %w", *runtime.ActiveClaimID, err)
		}
		return c, nil
	}
	return e.claimAtNode(node), nil
}

// segmentAfterFirstStationWaitLifts reports whether the steps between a leg's
// first station wait and its next wait of any kind pick a bin up at node. A
// leg with no station wait has no RELEASE, and answers false.
func segmentAfterFirstStationWaitLifts(steps []protocol.ComplexOrderStep, node string) bool {
	start := -1
	for i, s := range steps {
		if s.Action == protocol.ActionWait && protocol.IsStationWaitKind(s.WaitKind) {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return false
	}
	for _, s := range steps[start:] {
		if s.Action == protocol.ActionWait {
			return false
		}
		if s.Action == protocol.ActionPickup && s.Node == node {
			return true
		}
	}
	return false
}

// runtimeRemaining is a nil-safe read for log lines.
func runtimeRemaining(runtime *processes.RuntimeState) int {
	if runtime == nil {
		return 0
	}
	return runtime.RemainingUOPCached
}

// dispatchComplexLeg issues a single complex order with the right auto-
// confirm wiring. Direction-agnostic — produce passes quantity=1 (the
// bin), consume passes the operator-requested quantity. processNodeName
// is the line node both legs of a swap belong to (= claim.CoreNodeName);
// threaded into ComplexOrderRequest.ProcessNode so Core picks the line
// bin for order.BinID.
// dispatchComplexLeg creates one leg of a swap.
//
// THE ORIGIN IS ON THE SIGNATURE AND PASSED BY THE CALLER, never resolved
// inside. This function is SHARED with origin-less callers, and a lookup here
// would have to guess which episode a leg belongs to from the node — which is
// exactly the read-time attribution the stamp-forward rule exists to avoid.
// The caller knows, because the caller opened the episode.
//
// Both legs of a pair are given the SAME origin: one fire of the plan is one
// demand served by two rows.
// dispatchPairedLeg is dispatchComplexLeg for a leg whose uuid was minted with
// its partner's. Same wiring; the only difference is that the uuid arrives
// rather than being invented inside the create.
func (e *Engine) dispatchPairedLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool, siblingUUID, orderUUID string, origin ordermgr.Origin) (*orders.Order, error) {
	dn := deliveryNode
	if autoConfirm {
		dn = ""
	}
	return e.orderMgr.CreateComplexOrderPaired(&nodeID, quantity, dn, processNodeName, steps, autoConfirm, "", siblingUUID, orderUUID, origin)
}

// refreshOrder re-reads an order after the runtime-orders write so the
// caller sees the updated process_node_id linkage in the response.
func (e *Engine) refreshOrder(orderID int64) (*orders.Order, error) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		log.Printf("produce: re-read order %d after runtime update: %v", orderID, err)
		return nil, fmt.Errorf("re-read order %d: %w", orderID, err)
	}
	return o, nil
}
