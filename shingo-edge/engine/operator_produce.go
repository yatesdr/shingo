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
// Fix D renamed the tap: on two-robot modes this call only REQUESTS robots.
// The manifest snapshot and the count reset — the actual "finalize" — happen
// at the RELEASE tap (produceIngestAtRelease), because every part pressed
// between this call and the physical swap still lands in the departing bin.
// Snapshotting here understated the shipped tote and pre-credited the next
// one, and calling robots early (desirable) widened that window on purpose.
// Non-two-robot modes have no staged release step, so request time IS
// release time for them and the paperwork stays here.
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

	plan, err := BuildProducePlan(node, runtime, claim, time.Now(), occupancy, primedPositions)
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
	// Runs BEFORE setProduceManifest so we don't burn an ingest order on a
	// node that's about to be rejected. Edge-runtime-only — Core anomalies
	// don't shut down the line.
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
	for _, pos := range []string{claim.PairedCoreNode, claim.SecondPairedCoreNode} {
		if pos == "" {
			continue
		}
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
		return &NodeOrderResult{CycleMode: protocol.SwapModeSimple, PrimeOrders: primes, ProcessNodeID: nodeID}, nil
	}

	// Fix D: two-robot modes DEFER the paperwork (manifest ingest + count
	// reset) to the release tap — the bin keeps filling until the robots are
	// actually sent in, so the release-time count is the true shipped count.
	// The runtime ORDER pointers still stamp below either way (release
	// resolution and the swap_ready gate depend on them).
	deferPaperwork := claim.SwapMode.IsTwoRobot()
	if !deferPaperwork {
		if err := e.dispatchProduceIngest(node, claim, plan); err != nil {
			return nil, err
		}
	}

	// Produce always has a swap mode now (BuildProducePlan errors otherwise), so
	// Dispatch is always set.
	dispatch := plan.Dispatch
	orderA, err := e.dispatchComplexLeg(nodeID, 1, dispatch.StepsA, dispatch.DeliveryNodeA, dispatch.ProcessNode, dispatch.AutoConfirmA, "", origin)
	if err != nil {
		return nil, err
	}

	var orderB *orders.Order
	if dispatch.StepsB != nil {
		// Removal/evac leg carries the supply leg's UUID so Core can pair
		// them at intake (before this leg's dispatch claims the line bin).
		orderB, err = e.dispatchComplexLeg(nodeID, 1, dispatch.StepsB, "", dispatch.ProcessNode, dispatch.AutoConfirmB, orderA.UUID, origin)
		if err != nil {
			return nil, err
		}
	}

	var orderBID *int64
	if orderB != nil {
		orderBID = &orderB.ID
	}
	e.resetProduceRuntime(nodeID, runtime, &orderA.ID, orderBID, !deferPaperwork)
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
		return &NodeOrderResult{CycleMode: dispatch.CycleMode, Order: orderA, ProcessNodeID: nodeID}, nil
	}
	return &NodeOrderResult{CycleMode: dispatch.CycleMode, OrderA: orderA, OrderB: orderB, ProcessNodeID: nodeID}, nil
}

// dispatchProduceIngest stamps Core's bin manifest with the produced count.
// Produce is always manifest-only: the swap's complex order carries the bin, so
// a local ingest order would only be a phantom for the abort fan-out to cancel
// (the "not_found" bug). Fire-and-forget via QueueIngestManifest — no local
// order, no reply on success. Non-two-robot request-time path only; two-robot
// modes stamp at release via produceIngestAtRelease.
func (e *Engine) dispatchProduceIngest(node *processes.Node, claim *processes.NodeClaim, plan *ProducePlan) error {
	return e.orderMgr.QueueIngestManifest(
		claim.PayloadCode,
		"", // bin label resolved by core from node contents
		0,  // bin id likewise
		node.CoreNodeName,
		plan.Manifest[0].Quantity,
		plan.Manifest,
		plan.ProducedAtRFC3339,
	)
}

// produceIngestAtRelease is Fix D's deferred paperwork: at the RELEASE tap of
// a two-robot produce swap, snapshot the manifest from the LIVE count — every
// part pressed since the request landed in the departing bin, and the count
// kept ticking because the request-time reset was skipped — then clear the
// runtime so post-release ticks hold and replay onto the NEXT bin only.
//
// Enqueue ordering is the contract with Core: the ingest is queued BEFORE the
// two OrderRelease envelopes (same goroutine, sequential outbox inserts, and
// the outbox drains strictly ORDER BY id), so Core applies the manifest
// before any release-side manifest action. The ingest pins the departing bin
// by runtime.ActiveBinID — node-based resolution could land on the freshly
// indexed tote by the time Core processes a press-index release.
//
// Zero/negative count skips the stamp (nothing pressed, or a retry after a
// prior successful stamp+clear — the guard is what makes the release click
// idempotent). Ingest enqueue failure fails the release CLOSED: a full bin
// must not leave un-manifested when the operator can just click again.
func (e *Engine) produceIngestAtRelease(node *processes.Node, runtime *processes.RuntimeState, claim *processes.NodeClaim) error {
	if claim.Role != protocol.ClaimRoleProduce {
		return nil
	}
	if runtime == nil || runtime.RemainingUOPCached <= 0 {
		e.logFn("produce release: node %s remaining=%d — no release-time manifest to stamp",
			node.Name, runtimeRemaining(runtime))
		return nil
	}
	qty := int64(runtime.RemainingUOPCached)
	var binID int64
	if runtime.ActiveBinID != nil {
		binID = *runtime.ActiveBinID
	}
	manifest := []protocol.IngestManifestItem{{
		PartNumber:  claim.PayloadCode,
		Quantity:    qty,
		Description: claim.PayloadCode,
	}}
	if err := e.orderMgr.QueueIngestManifest(
		claim.PayloadCode, "", binID, node.CoreNodeName, qty, manifest,
		time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return fmt.Errorf("queue release-time ingest for node %s: %w", node.Name, err)
	}
	// Snapshot taken — the count now belongs to the departing bin. Clear
	// active + zero so the hold-and-replay window starts HERE, not at the
	// request. Log-only on failure: the manifest already shipped, and the
	// stale count would only re-stamp on a retry (Core's SetForProduction
	// is idempotent).
	if e.inventoryDelta != nil {
		var claimID *int64
		if runtime.ActiveClaimID != nil {
			claimID = runtime.ActiveClaimID
		}
		if err := e.inventoryDelta.ClearActiveAndReset(node.ID, claimID); err != nil {
			log.Printf("produce release: clear active bin for node %d: %v", node.ID, err)
		}
	}
	return nil
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
func (e *Engine) dispatchComplexLeg(nodeID int64, quantity int64, steps []protocol.ComplexOrderStep, deliveryNode, processNodeName string, autoConfirm bool, siblingUUID string, origin ordermgr.Origin) (*orders.Order, error) {
	dn := deliveryNode
	if autoConfirm {
		dn = ""
	}
	return e.orderMgr.CreateComplexOrderSibling(&nodeID, quantity, dn, processNodeName, steps, autoConfirm, "", siblingUUID, origin)
}

// resetProduceRuntime stamps the dispatched legs on the runtime and, when
// clearCounts is set, resets the counting state. clearCounts=true is the
// non-two-robot path (request time IS release time there): clear
// active_bin_id, which puts the tick path into hold mode — parts produced
// before the next empty bin lands accumulate in pending_uop_delta and replay
// onto the new bin when its OrderDelivered seeds active_bin_id + epoch.
// clearCounts=false is the two-robot request (Fix D): the bin is still under
// the press until the RELEASE tap, so the count keeps ticking on it and
// produceIngestAtRelease owns the clear.
//
// Errors are logged only — the order(s) already shipped, so failing
// here would leave the caller with no actionable recovery.
func (e *Engine) resetProduceRuntime(nodeID int64, runtime *processes.RuntimeState, activeID, stagedID *int64, clearCounts bool) {
	if clearCounts && e.inventoryDelta != nil {
		// Clear the active bin (slot is empty after finalize → ticks hold)
		// and zero the count (the next empty bin starts at 0). Preserves
		// the claim so the next delivery binds against it.
		var claimID *int64
		if runtime != nil {
			claimID = runtime.ActiveClaimID
		}
		if err := e.inventoryDelta.ClearActiveAndReset(nodeID, claimID); err != nil {
			log.Printf("produce: clear active bin for node %d: %v", nodeID, err)
		}
	}
	if err := e.db.UpdateProcessNodeRuntimeOrders(nodeID, activeID, stagedID); err != nil {
		log.Printf("produce: update runtime orders for node %d: %v", nodeID, err)
	}
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
