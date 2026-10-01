package engine

// release_doors.go — every release door, as an adapter (SHAPE §3.1).
//
// A door builds its act — its origin, its purpose and its leg set — and runs
// it: the act's plan asks the loader for what it needs, decides every leg
// (release.PlanAct: scope, the trunk's gates, G1, G3, G6, G7), and the commit
// carries it out. No door here reads, writes or refuses anything itself
// (TestReleaseDoorsDecideNothing); the census holds Plan's callers to this
// file.
//
//	1  operator pair click          ReleaseStagedOrders
//	2  operator per-order click     ReleaseOrderWithLineside (also the sim's auto-operator)
//	3  Material page                ReleaseNodeWithRemainingUOP → releaseNodeWithClaim
//	4  changeover position evac     EvacuateNode's fallback → releaseNodeWithClaim
//	5  changeover sweep / per node  ReleaseChangeoverWait / ReleaseChangeoverWaitForNode
//	6  a changeover node's station button: door 1 routes it to door 5
//	7  drop-situation evac          the trunk's drop arm
//	—  the intent worker            fireIntents: a node's held intents, re-planned on a wake
//
// What a held leg waits on is its intent, not a door: the intent worker re-plans
// it on the leg's OrderStaged, a lifter's BinPickedUp, a sibling's end, Core
// coming back, boot, and a 15 s floor while a machine-owned hold stands.

import (
	"errors"
	"fmt"

	"shingoedge/release"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// actLeg is one leg a door hands an act: the order, the disposition it
// carries, and its press (the legs one decision covers together).
type actLeg struct {
	orderID  int64
	label    string
	taskNode string
	press    string
	disp     ReleaseDisposition
}

// runAct plans an act over its legs, loading what the plan asks for, and
// commits it. The plan is returned for the door's account.
func (e *Engine) runAct(act *releaseAct, in []actLeg) (release.ActPlan, []*legLoad, error) {
	lls := make([]*legLoad, len(in))
	for i, l := range in {
		lls[i] = newLegLoad(l.orderID, l.label, l.taskNode, l.disp)
		lls[i].press = l.press
	}
	p := release.PlanAct(act.Act, act.actFacts, actLegsOf(lls))
	for p.Need != 0 {
		if p.NeedLeg < 0 {
			e.loadAct(act, lls, p.Need)
		} else {
			e.loadLeg(act, lls[p.NeedLeg], p.Need)
		}
		p = release.PlanAct(act.Act, act.actFacts, actLegsOf(lls))
	}
	return p, lls, e.commitAct(act, p, lls)
}

func actLegsOf(lls []*legLoad) []release.ActLeg {
	out := make([]release.ActLeg, len(lls))
	for i, ll := range lls {
		out[i] = release.ActLeg{Leg: ll.snap, Point: ll.point, Press: ll.press}
	}
	return out
}

// ── Door 2 ────────────────────────────────────────────────────────────────

// ReleaseOrderWithLineside performs the operator's release click on one
// order, its purpose that of the wait the leg is at or heading to. The leg
// goes (the flip of a sequential line, the departing produce bin's finalize,
// the capture, the count, the task, the flush, the envelope with its echo) or
// holds with an intent (the curtain, a lift, Core unreachable), and an order
// Core will not release is refused with Core's own account of why.
func (e *Engine) ReleaseOrderWithLineside(orderID int64, disp ReleaseDisposition) error {
	act := e.newAct(release.OriginStationOrder, 0, disp.CalledBy)
	unlock := e.lockOrderNode(orderID)
	defer unlock()
	_, _, err := e.runAct(act, []actLeg{{orderID: orderID, disp: disp}})
	return err
}

// ── Door 1 ────────────────────────────────────────────────────────────────

// ReleaseStagedOrders is the operator's one click on a paired node: both legs
// of the swap, in one act. The evac carries the operator's disposition and
// the supply none (its freshly loaded bin's manifest is never cleared). A leg
// that cannot go yet holds with an intent and goes by itself when it can.
//
// A changeover node whose work is not a coordinated two-robot swap is the
// changeover act's (§6.4, and the single-leg shape, N1-d).
func (e *Engine) ReleaseStagedOrders(nodeID int64, disp ReleaseDisposition) error {
	act := e.newAct(release.OriginStationPair, nodeID, disp.CalledBy)
	unlock := e.lockNode(nodeID)
	pl := &pairLoad{snap: release.Pair{NodeID: nodeID}}
	p := release.PlanPair(pl.snap)
	for p.Need != 0 {
		e.loadPair(act, pl, p.Need)
		p = release.PlanPair(pl.snap)
	}
	if p.Route {
		unlock()
		return e.releaseChangeoverNode(pl, disp)
	}
	defer unlock()
	if err := e.commitPairDoor(p); err != nil {
		return err
	}
	press := fmt.Sprintf("pair:%d", nodeID)
	var legs []actLeg
	if p.Evac != nil {
		legs = append(legs, actLeg{orderID: *p.Evac, label: "evac", press: press, disp: disp})
	}
	if p.Supply != nil {
		legs = append(legs, actLeg{orderID: *p.Supply, label: "supply", press: press, disp: ReleaseDisposition{CalledBy: disp.CalledBy}})
	}
	act.pressPaperwork = func() error { return e.commitPairPaperwork(p, pl) }
	_, _, err := e.runAct(act, legs)
	return err
}

// releaseChangeoverNode hands a changeover node's station click to the
// changeover act for that node (door 6).
func (e *Engine) releaseChangeoverNode(pl *pairLoad, disp ReleaseDisposition) error {
	res, err := e.ReleaseChangeoverWaitForNode(pl.routeProcess, pl.snap.NodeID, disp)
	if err != nil {
		return err
	}
	e.logFn("release-staged node=%s: changeover node release — released=%d pending=%d deferred=%d",
		pl.routeName, res.Released, res.Pending, res.Deferred)
	if res.Released == 0 && len(res.Held) > 0 {
		return release.NewHeldError(res.HeldGate, res.Held)
	}
	return nil
}

// ── Doors 5 and 6 ─────────────────────────────────────────────────────────

// ReleaseChangeoverWaitResult reports a release-wait click: Released is the
// legs whose envelopes were queued; Pending the legs a later click is owed for
// (not yet at a wait, or a position the line still pulls from); Deferred the
// legs held with an intent, which go by themselves when they can. Terminal
// legs count nowhere.
type ReleaseChangeoverWaitResult struct {
	Released int `json:"released"`
	Pending  int `json:"pending"`
	Deferred int `json:"deferred"`
	// NeedsFlip names the A/B positions a SWEEP declined because the line is
	// still pulling from them: a sweep is not aimed at one aisle, so it does
	// not move the line for the operator.
	NeedsFlip []string `json:"needs_flip,omitempty"`
	// Held is each held leg's sentence (the chip's words), and HeldGate the
	// first hold's gate.
	Held     []string `json:"held,omitempty"`
	HeldGate string   `json:"-"`
}

// ReleaseChangeoverWait releases the changeover's decision on every node (the
// sweep): one act over every in-scope task's legs, its purpose the earliest
// decision they owe.
func (e *Engine) ReleaseChangeoverWait(processID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, 0, disp, "")
}

// ReleaseChangeoverWaitForNode releases one node's changeover decision: the
// operator's per-node click, the same act narrowed to one task.
func (e *Engine) ReleaseChangeoverWaitForNode(processID, processNodeID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, processNodeID, disp, "")
}

// ReleaseChangeoverWaitFor releases the changeover decision a button names
// (release.Purpose: ready, tooling_done) on one node, or every node with 0:
// a "ready" act covers only legs at, or heading to, a ready wait, so it can
// never carry a leg through "tooling done" (N-a(ii)). A button with no purpose
// takes it from the legs at a wait (ReleaseChangeoverWait).
func (e *Engine) ReleaseChangeoverWaitFor(processID, processNodeID int64, purpose release.Purpose, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, processNodeID, disp, purpose)
}

func (e *Engine) releaseChangeoverWaitScoped(processID, onlyNodeID int64, disp ReleaseDisposition, purpose release.Purpose) (ReleaseChangeoverWaitResult, error) {
	origin, scope := release.OriginChangeoverSweep, "all-nodes"
	if onlyNodeID != 0 {
		origin, scope = release.OriginChangeoverNode, fmt.Sprintf("node=%d", onlyNodeID)
	}
	act := e.newAct(origin, onlyNodeID, disp.CalledBy)
	act.Purpose = purpose
	cl := e.loadChangeover(processID, onlyNodeID, disp)
	plan := release.PlanChangeover(cl.snap)
	if plan.Refusal != nil {
		return ReleaseChangeoverWaitResult{}, plan.Refusal
	}
	e.logFn("release_changeover_wait: process=%d changeover=%d tasks=%d scope=%s called_by=%q",
		processID, cl.coID, len(cl.tasks), scope, disp.CalledBy)
	// The supply leg rides through with no manifest action, whatever the
	// operator chose: its bin is mid-transit carrying its real count.
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}
	var res ReleaseChangeoverWaitResult
	var legs []actLeg
	for _, tp := range plan.Tasks {
		e.emitLogs(tp.Logs)
		if !tp.InScope {
			continue
		}
		task := cl.snap.Tasks[tp.Task]
		if tp.NeedsFlip {
			res.NeedsFlip = append(res.NeedsFlip, task.CoreName)
			res.Pending++
			continue
		}
		for _, s := range tp.Slots {
			d := supplyDisp
			if s.Kind == "evac" {
				d = cl.evacDisp[tp.Task]
			}
			legs = append(legs, actLeg{orderID: s.Order, label: s.Kind, taskNode: task.NodeName,
				press: fmt.Sprintf("task:%d", task.NodeID), disp: d})
		}
	}
	if len(legs) == 0 {
		return res, nil
	}
	unlock := e.lockNodes(nodesOfTasks(cl))
	defer unlock()
	p, _, err := e.runAct(act, legs)
	var failures []error
	var held *release.HeldError
	if err != nil && !errors.As(err, &held) {
		failures = append(failures, err)
	}
	for _, d := range p.Decisions {
		switch {
		case d.Verdict == release.Go && !act.failed[d.OrderID]:
			res.Released++
		case d.Verdict == release.Hold:
			res.Deferred++
			res.Held = append(res.Held, d.Sentence)
			if res.HeldGate == "" {
				res.HeldGate = d.Gate
			}
		case d.Verdict == release.Skip && d.Trunk.Skip == release.SkipNotReleasable:
			res.Pending++
		}
	}
	return res, errors.Join(failures...)
}

func nodesOfTasks(cl *changeoverLoad) []int64 {
	out := make([]int64, 0, len(cl.snap.Tasks))
	for _, t := range cl.snap.Tasks {
		if t.InScope {
			out = append(out, t.NodeID)
		}
	}
	return out
}

// ── Doors 3 and 4 ─────────────────────────────────────────────────────────

// ReleaseNodeEmpty releases the active claim's bin as consumed.
func (e *Engine) ReleaseNodeEmpty(nodeID int64) (*storeorders.Order, error) {
	return e.ReleaseNodePartial(nodeID, 1)
}

// ReleaseNodePartial releases the active claim's bin with the given quantity
// consumed. The manifest sync uses the runtime's cached count; when the
// operator has just declared the bin's count, ReleaseNodeWithRemainingUOP.
func (e *Engine) ReleaseNodePartial(nodeID int64, qty int64) (*storeorders.Order, error) {
	return e.releaseNodeInternal(nodeID, qty, nil)
}

// ReleaseNodeWithRemainingUOP is ReleaseNodePartial with the operator's count
// of what is left in the bin (0 = empty, the manifest cleared), superseding a
// cache that may be stale or zeroed.
func (e *Engine) ReleaseNodeWithRemainingUOP(nodeID int64, qty int64, remainingUOP int) (*storeorders.Order, error) {
	v := remainingUOP
	return e.releaseNodeInternal(nodeID, qty, &v)
}

func (e *Engine) releaseNodeInternal(nodeID int64, qty int64, overrideRemainingUOP *int) (*storeorders.Order, error) {
	return e.releaseNodeWithClaim(nodeID, qty, overrideRemainingUOP, nil)
}

// releaseNodeWithClaim is the Material page's release (door 3) and, with a
// fallback claim, the changeover evacuation of a fanned-out press position
// (door 4): a position owns changeover work but has no claim row under its own
// name. The fallback is used only when the node resolves no claim of its own.
//
// ── THIS DOOR IS NOT LINE-PULL GUARDED, AND THAT IS DELIBERATE ────────────
//
// It takes a bin off a position and sends it to the outbound destination
// without asking whether the line is pulling from it. OWNER RULING 2026-08-28:
// it stays unguarded — "it's basically an admin release, not guarded like the
// line". Guarding it on a SEQUENTIAL press is a change to what the owner
// decided, not a bug fix, and the guard belongs on the wait, not on a fourth
// door. The curtain is a different matter: it is gated (the node the bin
// leaves), and so is a second tap while the first tap's robot is on its way
// (L8).
func (e *Engine) releaseNodeWithClaim(nodeID int64, qty int64, overrideRemainingUOP *int, fallback *processes.NodeClaim) (*storeorders.Order, error) {
	origin := release.OriginMaterialPage
	if fallback != nil {
		origin = release.OriginPositionEvac
	}
	act := e.newAct(origin, nodeID, "")
	ml := &materialLoad{nodeID: nodeID, fallback: fallback, snap: release.Material{Qty: qty}}
	p := release.PlanMaterial(ml.snap)
	for p.Need != 0 {
		e.loadMaterial(act, ml, p.Need)
		p = release.PlanMaterial(ml.snap)
	}
	return e.commitMaterial(p, ml, qty, overrideRemainingUOP)
}

// ── The intent worker ─────────────────────────────────────────────────────

// fireIntents re-plans a node's held intents as one act (Origin intent): each
// leg at its intent's wait, with the intent's choices. It can only hold or
// release, never refuse. One lock per node, shared with the click doors.
func (e *Engine) fireIntents(nodeID int64, why string) {
	unlock := e.lockNode(nodeID)
	defer unlock()
	held := e.heldIntentLegs(nodeID)
	if len(held) == 0 {
		return
	}
	act := e.newAct(release.OriginIntent, nodeID, "")
	act.Reevaluation = true
	legs := make([]actLeg, 0, len(held))
	for _, h := range held {
		legs = append(legs, actLeg{orderID: h.orderID, label: "intent", press: fmt.Sprintf("intent:%d", nodeID), disp: h.disp})
	}
	p, _, err := e.runAct(act, legs)
	if err != nil {
		e.logFn("release intents at node %d (%s): %v", nodeID, why, err)
	}
	for _, d := range p.Decisions {
		if d.Verdict == release.Go && !act.failed[d.OrderID] {
			e.logFn("release intent: order %d released at node %d (%s)", d.OrderID, nodeID, why)
		}
	}
}
