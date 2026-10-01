package engine

// release_load.go — the release layer's one loader (SHAPE §3.1).
//
// Plan (package release) decides every release from data. When it reaches a
// gate whose facts are not loaded it returns the group it needs (release.Need),
// and the door's loop calls the loader here for that group and plans again. So
// the loader reads in gate order and never past the gate that decides — a
// refused leg costs exactly the reads it cost before the extraction — and it
// reads each fact the way the gate it serves always has, errors included.
//
// It is the one place the release path reads: the order and its release facts,
// the node, its runtime and claims, the pull state, the curtain's tag values,
// the changeover's tasks. Its one write is the runtime row the trunk has always
// created for a node that had none (EnsureProcessNodeRuntime), a default row
// and no paperwork.
//
// ── THE LIGHT CURTAIN ─────────────────────────────────────────────────────
//
// A light curtain stands at a finished-goods pickup. A physical bypass button
// at the cell writes a BOOL tag that mutes it. With the interlock enabled on a
// NODE, a release is only allowed when that node's tag reads the node's
// configured release value - checked by a DIRECT WarLink read at the moment of
// the release (plc.Manager.ReadTagDirect), never the poll cache, because a
// safety decision must not be older than the request that made it.
//
// THE BIN TRIPS THE CURTAIN, NOT THE ROBOT. The gate covers every curtained
// node where the leg's next move PICKS UP OR DROPS a bin (release.Facts.Touches).
// An empty robot driving in to its wait is not gated; the lift or the set-down
// that follows the release is. Until the Edge knows which wait a leg is parked
// at, "the next move" is read as every pickup and dropoff after the leg's first
// station wait - a superset for a leg already at a later wait, never a subset.
//
// PER NODE, NOT PER ROLE, and NO EXEMPTIONS: a changeover robot crosses the
// same curtain an ordinary one does.
//
// ONE READ PER NODE PER ACT. An act - one click, or one automatic re-fire -
// reads each curtained node once (releaseAct), so a pair click cannot pass the
// check for one leg and fail it for the other. A leg's nodes are read in the
// order its release lets a bin cross them, and the reads stop at the first that
// is not safe: the gate's answer is that node, and the ones after it cannot
// change it.
//
// FAIL-CLOSED, on everything: an unreadable curtain list, missing PLC/tag
// pointers, an unchosen polarity, an unreadable tag, a value that is not a
// BOOL, or the wrong value - each holds the release with a sentence the
// operator can act on.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/release"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// curtainReadTimeout bounds the direct WarLink read. A release click waits
// at most this long for the curtain's answer; past it the release refuses.
// Short on purpose - WarLink answers in milliseconds on a healthy plant, and
// a gate that hangs the operator's button is its own failure.
const curtainReadTimeout = 3 * time.Second

// CurtainHeldError is the light curtain's typed refusal, defined with the plan
// that returns it.
type CurtainHeldError = release.CurtainHeldError

// releaseAct is one act: what the door decided it is, and what it has already
// read, so each curtained node is read once and each leg's facts decoded once
// however many legs and doors the act goes through.
type releaseAct struct {
	release.Act
	actFacts release.ActFacts
	// pressPaperwork is the door's own declaration at the press (the pair
	// click's departing-bin finalize), run when the act covers a leg.
	pressPaperwork  func() error
	failed          map[int64]bool
	failures        []error
	curtainedLoaded bool
	curtainedErr    error
	curtained       map[string]*processes.Node // by core node name
	verdict         map[int64]error            // by node id; nil = clear
	facts           map[int64]legFacts         // by order id
	departs         map[int64]bool             // by order id: releasing it takes a produce bin away
}

func newReleaseAct() *releaseAct { return &releaseAct{} }

func (e *Engine) newAct(origin release.Origin, node int64, calledBy string) *releaseAct {
	return &releaseAct{Act: release.Act{Origin: origin, Node: node, CalledBy: calledBy}}
}

// ── A leg's release facts ─────────────────────────────────────────────────

// legFacts is a leg's release.Facts and how they were come by. An order this
// Edge created with steps carries them on its row; any other (a row older than
// the column, a plain retrieve or move, steps written by a test) has them
// computed from steps_json, with the three ways that can fail kept apart
// because each question treats them differently.
type legFacts struct {
	facts release.Facts
	// fromSteps: computed here rather than read from the row.
	fromSteps   bool
	readErr     error  // steps_json could not be read
	undecodable error  // steps_json did not decode
	none        bool   // no steps ("", "null", "[]")
	raw         string // steps_json as read, when fromSteps
}

// legFactsOf returns a leg's facts, once per act.
func (e *Engine) legFactsOf(act *releaseAct, order *storeorders.Order) legFacts {
	if act != nil {
		if f, ok := act.facts[order.ID]; ok {
			return f
		}
	}
	f := e.readLegFacts(order)
	if act != nil {
		if act.facts == nil {
			act.facts = map[int64]legFacts{}
		}
		act.facts[order.ID] = f
	}
	return f
}

func (e *Engine) readLegFacts(order *storeorders.Order) legFacts {
	if order.ReleaseFacts != "" {
		if f, err := release.DecodeFacts(order.ReleaseFacts); err == nil {
			return legFacts{facts: f}
		}
	}
	lf := legFacts{fromSteps: true}
	raw, err := e.db.GetOrderStepsJSON(order.ID)
	if err != nil {
		lf.readErr = err
		return lf
	}
	lf.raw = raw
	if raw == "" || raw == "null" || raw == "[]" {
		lf.none = true
		return lf
	}
	steps, err := decodeSteps(raw)
	if err != nil {
		lf.undecodable = err
		return lf
	}
	// The node-keyed answers are what the release reads; Role is the
	// creation-time label and is not computed for a row that has none.
	lf.facts = release.FactsFromSteps(steps, "")
	return lf
}

// placesBinAt is the supply/evac discriminator over a leg's facts, with the
// errors the classifier has always returned: an unreadable leg is refused,
// never guessed (guessing "evac" wipes a supply bin's manifest, ALN_002).
func (lf legFacts) placesBinAt(orderID int64, node string) (bool, error) {
	switch {
	case lf.readErr != nil:
		return false, fmt.Errorf("supply-leg check: order %d: load steps: %w", orderID, lf.readErr)
	case lf.fromSteps && lf.raw == "":
		return false, fmt.Errorf("supply-leg check: order %d: no steps stored", orderID)
	case lf.undecodable != nil:
		return false, fmt.Errorf("supply-leg check: order %d: %w", orderID, lf.undecodable)
	}
	return lf.facts.PlacesBinAt(node), nil
}

// ── The trunk's loads ─────────────────────────────────────────────────────

// legLoad is one leg's load: the snapshot Plan reads, and the objects commit
// acts on.
type legLoad struct {
	snap     release.Leg
	disp     ReleaseDisposition
	order    *storeorders.Order
	node     *processes.Node
	runtime  *processes.RuntimeState
	toClaim  *processes.NodeClaim
	nodeTask *processes.NodeTask
	partner  *processes.Node // the flip's target
	press    string
	point    release.StaticPoint
	// flipped / finalized: the act's press paperwork already did these.
	flipped, finalized bool
}

// echo is the release's station wait (OrderRelease.StationWait): the wait the
// leg is at or heading to; none past its last.
func (ll *legLoad) echo() *int {
	if !ll.point.HasWait {
		return nil
	}
	n := ll.point.Ordinal
	return &n
}

func newLegLoad(orderID int64, label, taskNode string, disp ReleaseDisposition) *legLoad {
	return &legLoad{
		snap: release.Leg{OrderID: orderID, Label: label, TaskNode: taskNode, Mode: string(disp.Mode)},
		disp: disp,
	}
}

// loadLeg reads one group of a leg's facts.
func (e *Engine) loadLeg(act *releaseAct, ll *legLoad, need release.Need) {
	s := &ll.snap
	switch need {
	case release.NeedOrder:
		order, err := e.db.GetOrder(s.OrderID)
		if err != nil {
			s.ReadErr = err
			break
		}
		ll.order = order
		s.Status, s.QueueReason, s.QueueCode = order.Status, order.QueueReason, order.QueueCode
		if order.ProcessNodeID != nil {
			s.HasProcessNode, s.ProcessNodeID = true, *order.ProcessNodeID
		}
		in, ierr := release.DecodeIntent(order.ReleaseIntent)
		if ierr != nil {
			e.logRelease("order=%d: release intent unreadable (%v) — read as none", order.ID, ierr)
		}
		s.Intent = in
		ll.point = release.PointOf(order.Status, order.StationWait, order.WaitKind, in, e.legFactsOf(act, order).facts.Purposes)
	case release.NeedCurtain:
		if pt, ok := act.actFacts.Points[s.OrderID]; ok && pt.Found {
			s.Curtain = e.curtainForNodes(act, pt.Enters)
		} else {
			s.Curtain = e.curtainForLeg(act, ll.order)
		}
	case release.NeedNode:
		node, err := e.db.GetProcessNode(s.ProcessNodeID)
		if err != nil {
			s.NodeErr = err
			break
		}
		ll.node = node
		s.NodeName, s.CoreNode = node.Name, node.CoreNodeName
	case release.NeedPull:
		// An unreadable pull state declines, as the scoped door does: the
		// answer to "may a robot strip this position" must not depend on which
		// button was pressed. Asked before the runtime row below would be
		// created with the bit off.
		if _, own, _, err := e.linePullsFrom(ll.node.ID); err != nil {
			if own == "" {
				own = ll.node.CoreNodeName
			}
			s.PullErr, s.PullNode = err, own
		}
	case release.NeedRuntime:
		runtime, err := e.db.EnsureProcessNodeRuntime(ll.node.ID)
		if err != nil {
			s.RuntimeErr = err
			break
		}
		ll.runtime = runtime
		s.ActiveClaim = "<nil>"
		if runtime != nil && runtime.ActiveClaimID != nil {
			s.ActiveClaim = fmt.Sprintf("%d", *runtime.ActiveClaimID)
		}
	case release.NeedKind:
		if dropTask, _ := e.db.GetChangeoverNodeTaskByEvacOrderID(s.OrderID); dropTask != nil && dropTask.Situation == "drop" {
			s.Drop = true
			break
		}
		ll.toClaim, ll.nodeTask = e.resolveReleaseClaim(ll.node, ll.runtime)
		s.ClaimResolved = ll.toClaim != nil
		s.Produce = ll.toClaim != nil && ll.toClaim.Role == protocol.ClaimRoleProduce
	case release.NeedSupply:
		s.IsSupply, s.SupplyErr = e.isSupplyLeg(act, ll.order, ll.node, ll.toClaim)
	case release.NeedDeparts:
		known := ll.toClaim
		if s.Drop {
			known = nil
		}
		s.Departs, s.DepartErr = e.departingProduceLeg(act, ll.order, ll.node, ll.runtime, known)
	case release.NeedFlip:
		s.Flip, ll.partner = e.loadFlip(ll.node)
	}
	s.Loaded |= need
}

// loadFlip reads the sequential partner a release on the feeding side moves
// the line to. It re-derives sequential + paired + own-bit rather than trusting
// the caller: a parked-side release (own bit false) must never rewrite the
// pair, and neither must a position that only shares the trunk.
func (e *Engine) loadFlip(node *processes.Node) (release.Flip, *processes.Node) {
	claim := e.claimAtNode(node)
	if claim == nil || claim.PairedCoreNode == "" || claim.SwapMode != protocol.SwapModeSequential {
		return release.Flip{}, nil
	}
	f := release.Flip{Applies: true, Node: node.CoreNodeName}
	rt, err := e.db.GetProcessNodeRuntime(node.ID)
	if err != nil || rt == nil {
		f.RuntimeErr = err
		if err == nil {
			f.RuntimeErr = release.ErrRuntimeMissing
		}
		return f, nil
	}
	f.OwnPull = rt.ActivePull
	if !f.OwnPull {
		return f, nil
	}
	partner, err := e.pairedNodeOf(node)
	if err != nil {
		f.PartnerErr = err
		return f, nil
	}
	f.Partner = partner.CoreNodeName
	// A partner with no bin yet takes the line and its count waits (owner,
	// 2026-09-30). The one shape hold-and-replay cannot carry is a partner not
	// ready with a carrier still BOUND: mid-changeover, its outgoing carrier.
	if f.NotReady = e.flipTargetReady(partner); f.NotReady != "" {
		prt, perr := e.db.GetProcessNodeRuntime(partner.ID)
		f.Blocked = perr != nil || prt == nil || prt.ActiveBinID != nil
	}
	return f, partner
}

// resolveReleaseClaim returns the claim whose capacity the release should
// reset UOP against, plus the changeover node task when one is active. For a
// changeover the target is the to-style claim; otherwise it's the
// currently-active claim on the node.
func (e *Engine) resolveReleaseClaim(node *processes.Node, runtime *processes.RuntimeState) (*processes.NodeClaim, *processes.NodeTask) {
	if changeover, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil {
		if toClaim, err := e.db.GetStyleNodeClaimByNode(changeover.ToStyleID, node.CoreNodeName); err == nil {
			if task, err := e.db.GetChangeoverNodeTaskByNode(changeover.ID, node.ID); err == nil {
				return toClaim, task
			}
			return toClaim, nil
		}
	}
	if runtime.ActiveClaimID == nil {
		return nil, nil
	}
	claim, err := e.db.GetStyleNodeClaim(*runtime.ActiveClaimID)
	if err != nil {
		return nil, nil
	}
	return claim, nil
}

// isSupplyOrderInTwoRobotSwap reports whether the given order is the supply
// leg of a two-robot swap pair: a durable sibling link, a two-robot mode, and
// a leg that LEAVES A BIN at this node (release.Facts.PlacesBinAt) — the bin's
// resting place, not the robot's, and never order.DeliveryNode (press-index R1
// stored the press there; R2, auto-confirmed, stores nothing).
//
// An unreadable leg is an error, not a guess: false means EVAC, the branch
// that wipes the manifest, and that is the ALN_002 incident class.
func (e *Engine) isSupplyOrderInTwoRobotSwap(order *storeorders.Order, node *processes.Node, claim *processes.NodeClaim) (bool, error) {
	return e.isSupplyLeg(nil, order, node, claim)
}

func (e *Engine) isSupplyLeg(act *releaseAct, order *storeorders.Order, node *processes.Node, claim *processes.NodeClaim) (bool, error) {
	if order == nil || node == nil || claim == nil {
		return false, nil
	}
	if !claim.SwapMode.IsTwoRobot() || order.SiblingOrderID == nil {
		return false, nil
	}
	return e.legFactsOf(act, order).placesBinAt(order.ID, node.CoreNodeName)
}

// departingProduceLeg reports whether releasing this order takes a produce
// bin off the node: the segment after the leg's first station wait lifts a
// bin at the node (release.Facts.DepartsFrom), and the bin standing there was
// filled for a produce claim (residentClaim). Never from SwapMode or
// SiblingOrderID, so every mode and every door answers it the same way. An
// unreadable answer is an error: the caller refuses rather than ship or skip a
// bin's count on a guess.
//
// Once per leg per act. known is the claim the caller already resolved; it
// answers for the resident when it is the runtime's active claim.
func (e *Engine) departingProduceLeg(act *releaseAct, order *storeorders.Order, node *processes.Node, runtime *processes.RuntimeState, known *processes.NodeClaim) (bool, error) {
	if node == nil || runtime == nil {
		return false, nil
	}
	if d, ok := act.departs[order.ID]; ok {
		return d, nil
	}
	resident := known
	if resident == nil || runtime.ActiveClaimID == nil || resident.ID != *runtime.ActiveClaimID {
		var err error
		if resident, err = e.residentClaim(node, runtime); err != nil {
			return false, fmt.Errorf("departing-bin check: node %s: %w", node.Name, err)
		}
	}
	departs := false
	if resident != nil && resident.Role == protocol.ClaimRoleProduce {
		lf := e.legFactsOf(act, order)
		switch {
		case lf.readErr != nil:
			return false, fmt.Errorf("departing-bin check: order %d: load steps: %w", order.ID, lf.readErr)
		case lf.undecodable != nil:
			return false, fmt.Errorf("departing-bin check: %w", stepsUndecodable{fmt.Errorf("order %d: %w", order.ID, lf.undecodable)})
		}
		departs = lf.facts.DepartsFrom(node.CoreNodeName)
	}
	if act.departs == nil {
		act.departs = map[int64]bool{}
	}
	act.departs[order.ID] = departs
	return departs, nil
}

// stepsUndecodable is a leg whose stored steps did not decode, as opposed to a
// leg whose steps could not be read or has none.
type stepsUndecodable struct{ error }

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

// linePullsFrom reports whether a node is one half of a SEQUENTIAL A/B pair
// that the line is currently drawing from, and names its partner. That is the
// physical reason a robot must not strip a position. Two_robot and press-index
// release at the active pull point by design (the index motion swaps the
// position the press runs from), so only sequential — whose premise is that
// the other position takes over first — answers yes.
func (e *Engine) linePullsFrom(nodeID int64) (pulling bool, own, partner string, err error) {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil || node == nil {
		return false, "", "", fmt.Errorf("read process node %d: %w", nodeID, err)
	}
	claim := e.claimAtNode(node)
	if claim == nil || claim.PairedCoreNode == "" {
		return false, node.CoreNodeName, "", nil
	}
	if claim.SwapMode != protocol.SwapModeSequential {
		return false, node.CoreNodeName, claim.PairedCoreNode, nil
	}
	rt, err := e.db.GetProcessNodeRuntime(nodeID)
	if err != nil || rt == nil {
		return false, node.CoreNodeName, claim.PairedCoreNode,
			fmt.Errorf("read runtime for node %d: %w", nodeID, err)
	}
	return rt.ActivePull, node.CoreNodeName, claim.PairedCoreNode, nil
}

// ── The curtain's reads ───────────────────────────────────────────────────

// curtainedNodes loads the Edge's curtained nodes once per act, keyed by core
// node name. A core node claimed by two processes is curtained if either row
// says so: the curtain is a fact about the place.
func (e *Engine) curtainedNodes(act *releaseAct) (map[string]*processes.Node, error) {
	if act.curtainedLoaded {
		return act.curtained, act.curtainedErr
	}
	act.curtainedLoaded = true
	nodes, err := e.db.ListCurtainedProcessNodes()
	if err != nil {
		act.curtainedErr = fmt.Errorf("the light-curtain interlock's configuration could not be read (%w) - release held", err)
		return nil, act.curtainedErr
	}
	act.curtained = make(map[string]*processes.Node, len(nodes))
	for i := range nodes {
		n := &nodes[i]
		if _, seen := act.curtained[n.CoreNodeName]; !seen {
			act.curtained[n.CoreNodeName] = n
		}
	}
	return act.curtained, nil
}

// curtainForLeg checks every curtained node the leg's next move picks up or
// drops a bin at. An order with no steps (a plain retrieve or move) is gated on
// its own process node, the only node it can be said to touch.
func (e *Engine) curtainForLeg(act *releaseAct, order *storeorders.Order) error {
	curtained, err := e.curtainedNodes(act)
	if err != nil {
		return &CurtainHeldError{Sentence: err.Error()}
	}
	if len(curtained) == 0 {
		return nil
	}
	touch, err := e.legTouches(act, order)
	if err != nil {
		return err
	}
	for _, name := range touch {
		if n := curtained[name]; n != nil {
			if err := e.curtainClear(act, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// curtainForNodes checks the curtained nodes among the ones Core says the
// leg's next segment enters, in that order.
func (e *Engine) curtainForNodes(act *releaseAct, nodes []string) error {
	curtained, err := e.curtainedNodes(act)
	if err != nil {
		return &CurtainHeldError{Sentence: err.Error()}
	}
	for _, name := range nodes {
		if n := curtained[name]; n != nil {
			if err := e.curtainClear(act, n); err != nil {
				return err
			}
		}
	}
	return nil
}

// legTouches is the nodes a leg's release lets a bin cross.
func (e *Engine) legTouches(act *releaseAct, order *storeorders.Order) ([]string, error) {
	lf := e.legFactsOf(act, order)
	switch {
	case lf.undecodable != nil:
		return nil, &CurtainHeldError{Sentence: fmt.Sprintf(
			"order %d's steps could not be read to check the light curtain (%v) - release held", order.ID,
			fmt.Errorf("order %d: %w", order.ID, lf.undecodable))}
	case lf.readErr != nil, lf.none:
		if order.ProcessNodeID != nil {
			if n, nerr := e.db.GetProcessNode(*order.ProcessNodeID); nerr == nil && n != nil {
				return []string{n.CoreNodeName}, nil
			}
		}
		return nil, nil
	}
	return lf.facts.Touches, nil
}

// anyCurtained reports whether the Edge has any curtained node at all, so a
// door can skip reading its legs when there is nothing to check them against.
func (e *Engine) anyCurtained(act *releaseAct) (bool, error) {
	curtained, err := e.curtainedNodes(act)
	if err != nil {
		return false, &CurtainHeldError{Sentence: err.Error()}
	}
	return len(curtained) > 0, nil
}

// curtainForNode checks one node by core name: the node a door lifts a bin
// from directly (the Material page, the position evac).
func (e *Engine) curtainForNode(act *releaseAct, coreNodeName string) error {
	curtained, err := e.curtainedNodes(act)
	if err != nil {
		return &CurtainHeldError{Sentence: err.Error()}
	}
	if n := curtained[coreNodeName]; n != nil {
		return e.curtainClear(act, n)
	}
	return nil
}

// curtainClear reads one curtained node's tag, once per act, and returns nil
// when the release may proceed or the refusal when it may not.
func (e *Engine) curtainClear(act *releaseAct, node *processes.Node) error {
	if act.verdict == nil {
		act.verdict = map[int64]error{}
	}
	if v, done := act.verdict[node.ID]; done {
		return v
	}
	v := e.readCurtain(node)
	act.verdict[node.ID] = v
	return v
}

func (e *Engine) readCurtain(node *processes.Node) error {
	held := func(format string, args ...any) error {
		return &CurtainHeldError{Node: node.CoreNodeName, Sentence: fmt.Sprintf(format, args...)}
	}
	if node.CurtainPLCName == "" || node.CurtainTagName == "" {
		return held("The light curtain at %s is enabled but its PLC/tag are not set - release held; fix the node's curtain settings",
			node.CoreNodeName)
	}
	if node.CurtainSafeValue == nil {
		return held("The light curtain at %s is enabled but nobody has chosen which tag value allows the release - release held; set it in the node's curtain settings",
			node.CoreNodeName)
	}
	plcName, tagName, safe := node.CurtainPLCName, node.CurtainTagName, *node.CurtainSafeValue
	ctx, cancel := context.WithTimeout(context.Background(), curtainReadTimeout)
	defer cancel()
	raw, err := e.plcMgr.ReadTagDirect(ctx, plcName, tagName)
	if err != nil {
		return held("The light curtain at %s could not be read (%s/%s: %s) - release held until it reads",
			node.CoreNodeName, plcName, tagName, err)
	}
	val, ok := plc.CurtainBool(raw)
	if !ok {
		return held("The light curtain at %s read %v, which is not a BOOL - release held",
			node.CoreNodeName, raw)
	}
	if val != safe {
		return held("Release the light curtain at %s, then press RELEASE again.", node.CoreNodeName)
	}
	return nil
}

// ── Door 1's loads ────────────────────────────────────────────────────────

// pairLoad is the pair click's load.
type pairLoad struct {
	snap release.Pair
	// routeProcess / routeName: the node's process and name, read with its
	// changeover task, for the hand-off to the changeover act.
	routeProcess int64
	routeName    string
	node         *processes.Node
	runtime      *processes.RuntimeState
	claim        *processes.NodeClaim
	task         *processes.NodeTask
	taskRead     bool
	evac         *storeorders.Order
	supply       *storeorders.Order
	evacErr      error
	supplyErr    error
}

func (e *Engine) loadPair(act *releaseAct, pl *pairLoad, need release.Need) {
	s := &pl.snap
	switch need {
	case release.NeedRoute:
		// A changeover node whose work is not a coordinated two-robot swap is
		// the changeover act's (doors 5 and 6).
		if node, err := e.db.GetProcessNode(s.NodeID); err == nil && node != nil {
			pl.routeProcess, pl.routeName = node.ProcessID, node.Name
			if co, err := e.db.GetActiveProcessChangeover(node.ProcessID); err == nil && co != nil {
				if task, err := e.db.GetChangeoverNodeTaskByNode(co.ID, s.NodeID); err == nil && task != nil {
					pl.task, pl.taskRead = task, true
					s.TaskEvac = task.OldMaterialReleaseOrderID != nil
					s.TaskSupply = task.NextMaterialOrderID != nil
				}
			}
		}
	case release.NeedActive:
		node, runtime, claim, err := loadActiveNode(e.db, s.NodeID)
		if err != nil {
			s.LoadErr = err
			break
		}
		pl.node, pl.runtime, pl.claim = node, runtime, claim
		s.NodeName = node.Name
		s.ClaimResolved = claim != nil
		if claim != nil {
			s.Mode = claim.SwapMode
		}
	case release.NeedPair:
		e.loadPairLegs(act, pl)
	case release.NeedDeparts:
		// Asked of the leg's steps by id: an evac row that could not be read
		// is refused only if its bin departs (the read of the departing order
		// the finalize needs), and otherwise by the trunk when it releases it.
		evac := pl.evac
		if pl.evacErr != nil {
			evac = &storeorders.Order{ID: *s.Evac}
			s.DepartingReadErr = pl.evacErr
		}
		s.Departs, s.DepartErr = e.departingProduceLeg(act, evac, pl.node, pl.runtime, pl.claim)
	}
	s.Loaded |= need
}

// loadPairLegs resolves the pair: through the durable sibling pointer rather
// than the volatile runtime slots, with the changeover task as the last rung,
// then re-labels it from the legs' steps — ResolveSwapPair's positional labels
// (staged→evac, active→supply) are a two_robot assumption, inverted for
// press-index, and the label decides which leg carries the operator's
// disposition.
func (e *Engine) loadPairLegs(act *releaseAct, pl *pairLoad) {
	s := &pl.snap
	if !pl.taskRead {
		pl.task = loadReleaseSwapNodeTask(e.db, pl.node)
	}
	s.HasTask = pl.task != nil
	if pl.runtime != nil {
		s.StagedPtr, s.ActivePtr = pl.runtime.StagedOrderID, pl.runtime.ActiveOrderID
	}
	evacID, supplyID, err := store.ResolveSwapPair(e.db, pl.runtime, pl.task)
	if err != nil {
		s.ResolveErr = err
		return
	}
	if evacID != nil {
		pl.evac, pl.evacErr = e.db.GetOrder(*evacID)
	}
	if supplyID != nil {
		pl.supply, pl.supplyErr = e.db.GetOrder(*supplyID)
	}
	if evacID != nil && supplyID != nil {
		if inverted, ok := e.classifyLegs(pl.claim.CoreNodeName, *evacID, *supplyID, pl.evac, pl.supply, pl.evacErr, pl.supplyErr); ok && inverted {
			s.Relabelled, s.SlotEvac, s.SlotSupply = true, *evacID, *supplyID
			evacID, supplyID = supplyID, evacID
			pl.evac, pl.supply = pl.supply, pl.evac
			pl.evacErr, pl.supplyErr = pl.supplyErr, pl.evacErr
		}
	}
	s.Evac, s.Supply = evacID, supplyID
}

// loadReleaseSwapNodeTask fetches the active changeover node task for this
// node, ResolveSwapPair's last rung. Best-effort: nil on any miss.
func loadReleaseSwapNodeTask(db *store.DB, node *processes.Node) *processes.NodeTask {
	co, err := db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return nil
	}
	task, err := db.GetChangeoverNodeTaskByNode(co.ID, node.ID)
	if err != nil {
		return nil
	}
	return task
}

// classifySwapLegsBySteps re-derives which of a resolved pair is the SUPPLY
// (the leg that leaves a bin on the process node) and which the EVAC, from
// the legs' release facts rather than the runtime slot each sits in. Exactly
// one leg of a well-formed pair places a bin at the process node; if both do,
// neither does, or a leg cannot be read, ok is false and the caller keeps the
// positional labels (refusing would take away the operator's only button over
// a classification detail), with what it saw logged.
func (e *Engine) classifySwapLegsBySteps(processNode string, posEvacID, posSupplyID int64) (evacID, supplyID int64, ok bool) {
	if processNode == "" {
		return 0, 0, false
	}
	a, aErr := e.db.GetOrder(posEvacID)
	b, bErr := e.db.GetOrder(posSupplyID)
	inverted, ok := e.classifyLegs(processNode, posEvacID, posSupplyID, a, b, aErr, bErr)
	switch {
	case !ok:
		return 0, 0, false
	case inverted:
		return posSupplyID, posEvacID, true
	}
	return posEvacID, posSupplyID, true
}

// classifyLegs is classifySwapLegsBySteps over legs already read: inverted
// says the evac slot holds the supply.
func (e *Engine) classifyLegs(processNode string, posEvacID, posSupplyID int64, a, b *storeorders.Order, aErr, bErr error) (inverted, ok bool) {
	if processNode == "" {
		return false, false
	}
	if aErr != nil || bErr != nil {
		e.logFn("swap-leg classify node=%s: cannot read steps (evac-slot %d: %v, supply-slot %d: %v) - keeping positional labels",
			processNode, posEvacID, aErr, posSupplyID, bErr)
		return false, false
	}
	af, bf := e.readLegFacts(a), e.readLegFacts(b)
	if af.readErr != nil || bf.readErr != nil {
		e.logFn("swap-leg classify node=%s: cannot read steps (evac-slot %d: %v, supply-slot %d: %v) - keeping positional labels",
			processNode, posEvacID, af.readErr, posSupplyID, bf.readErr)
		return false, false
	}
	aPlaces, aDecodeErr := af.placesBinAtQuiet(processNode)
	bPlaces, bDecodeErr := bf.placesBinAtQuiet(processNode)
	if aDecodeErr != nil || bDecodeErr != nil {
		e.logFn("swap-leg classify node=%s: cannot decode steps (%d: %v, %d: %v) - keeping positional labels",
			processNode, posEvacID, aDecodeErr, posSupplyID, bDecodeErr)
		return false, false
	}
	if aPlaces == bPlaces {
		e.logFn("swap-leg classify node=%s: BOTH legs place=%v (orders %d, %d) - not a supply/evac pair, keeping positional labels",
			processNode, aPlaces, posEvacID, posSupplyID)
		return false, false
	}
	return aPlaces, true // aPlaces: the "evac" slot holds the supply
}

// placesBinAtQuiet is placesBinAt with the decode error bare, as the
// classifier logs it.
func (lf legFacts) placesBinAtQuiet(node string) (bool, error) {
	switch {
	case lf.fromSteps && lf.raw == "":
		return false, errors.New("no steps stored")
	case lf.undecodable != nil:
		return false, lf.undecodable
	}
	return lf.facts.PlacesBinAt(node), nil
}

// ── Doors 5 and 6's loads ─────────────────────────────────────────────────

// changeoverLoad is the changeover release's load.
type changeoverLoad struct {
	snap      release.Changeover
	processID int64
	coID      int64
	tasks     []processes.NodeTask
	evacDisp  []ReleaseDisposition // by task index
}

// loadChangeover reads the changeover, its tasks, and each in-scope task's
// facts: the pull state (a sweep only), the evac's disposition from the line's
// runtime cache, and the paired supply's state.
func (e *Engine) loadChangeover(processID, onlyNodeID int64, disp ReleaseDisposition) *changeoverLoad {
	cl := &changeoverLoad{processID: processID, snap: release.Changeover{Sweep: onlyNodeID == 0}}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if err != nil {
		cl.snap.ReadErr = err
		return cl
	}
	cl.coID = changeover.ID
	tasks, err := e.db.ListChangeoverNodeTasks(changeover.ID)
	if err != nil {
		cl.snap.ReadErr = err
		return cl
	}
	cl.tasks = tasks
	cl.evacDisp = make([]ReleaseDisposition, len(tasks))
	for i, task := range tasks {
		t := release.Task{
			NodeID: task.ProcessNodeID, NodeName: task.NodeName, Situation: task.Situation,
			InScope: task.Situation != "unchanged" && (onlyNodeID == 0 || task.ProcessNodeID == onlyNodeID),
			Evac:    task.OldMaterialReleaseOrderID, Supply: task.NextMaterialOrderID,
		}
		if t.InScope {
			if onlyNodeID == 0 {
				t.Pulling, t.PullNode, _, t.PullErr = e.linePullsFrom(task.ProcessNodeID)
				if t.PullErr != nil || t.Pulling {
					t.CoreName = coreNameOf(e, task)
				}
			}
			if onlyNodeID != 0 || (t.PullErr == nil && !t.Pulling) {
				cl.evacDisp[i] = evacDispositionForTask(e, task, disp)
			}
		}
		cl.snap.Tasks = append(cl.snap.Tasks, t)
	}
	return cl
}

// coreNameOf is the node's CORE name — what the board keys on — falling back
// to the display name if the row cannot be read.
func coreNameOf(e *Engine, task processes.NodeTask) string {
	if n, err := e.db.GetProcessNode(task.ProcessNodeID); err == nil && n != nil && n.CoreNodeName != "" {
		return n.CoreNodeName
	}
	return task.NodeName
}

// evacDispositionForTask picks the evac leg's disposition. The operator's
// override wins; otherwise the line's runtime cache decides: parts left →
// send_partial_back with that count, drained → release_empty (the ALN_001 fix:
// a bin can't land at its outbound destination tagged with a stale payload). A
// failed runtime read defaults to release_empty — better to clear than to
// silently no-op.
func evacDispositionForTask(e *Engine, task processes.NodeTask, override ReleaseDisposition) ReleaseDisposition {
	if override.Mode != "" {
		return override
	}
	runtime, err := e.db.GetProcessNodeRuntime(task.ProcessNodeID)
	if err != nil {
		e.logFn("release changeover wait node %s: runtime lookup failed (%v); defaulting evac to release_empty", task.NodeName, err)
		return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
	}
	if runtime != nil && runtime.RemainingUOPCached > 0 {
		count := runtime.RemainingUOPCached
		return ReleaseDisposition{Mode: DispositionSendPartialBack, PartialCount: &count, CalledBy: override.CalledBy}
	}
	return ReleaseDisposition{Mode: DispositionCaptureLineside, CalledBy: override.CalledBy}
}

// ── Doors 3 and 4's loads ─────────────────────────────────────────────────

// materialLoad is the Material page's load.
type materialLoad struct {
	snap     release.Material
	nodeID   int64
	fallback *processes.NodeClaim
	node     *processes.Node
	runtime  *processes.RuntimeState
	claim    *processes.NodeClaim
}

func (e *Engine) loadMaterial(act *releaseAct, ml *materialLoad, need release.Need) {
	s := &ml.snap
	switch need {
	case release.NeedActive:
		node, runtime, claim, err := loadActiveNode(e.db, ml.nodeID)
		if err != nil {
			s.LoadErr = err
			break
		}
		ml.node, ml.runtime = node, runtime
		s.NodeName = node.Name
		if claim == nil {
			claim = ml.fallback
		}
		ml.claim = claim
		if claim != nil {
			s.ClaimResolved, s.ClaimNode, s.OutboundDest = true, claim.CoreNodeName, claim.OutboundDestination
		}
	case release.NeedCurtain:
		s.Curtain = e.curtainForNode(act, ml.claim.CoreNodeName)
	case release.NeedInFlight:
		if ml.runtime == nil || ml.runtime.ActiveOrderID == nil {
			break
		}
		prior, err := e.db.GetOrder(*ml.runtime.ActiveOrderID)
		if err != nil {
			s.InFlightErr = err
			break
		}
		s.InFlightOrder, s.InFlightStatus = prior.ID, prior.Status
		s.InFlight = prior.OrderType == orders.TypeMove && prior.SourceNode == ml.claim.CoreNodeName && !orders.IsTerminal(prior.Status)
	}
	s.Loaded |= need
}
