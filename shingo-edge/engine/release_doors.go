package engine

// release_doors.go — every release door, as an adapter (SHAPE §3.1).
//
// A door builds its act, loads what its plan asks for, plans, and commits:
// nothing else. The decisions are release.Plan*'s (the gate table), the reads
// are release_load.go's, the effects are release_commit.go's. No door here
// refuses anything itself: TestReleaseDoorsDecideNothing holds the file to
// that, and the census holds Plan's callers to this file.
//
//	1  operator pair click          ReleaseStagedOrders
//	2  operator per-order click     ReleaseOrderWithLineside (also the sim's auto-operator)
//	3  Material page                ReleaseNodeWithRemainingUOP → releaseNodeWithClaim
//	4  changeover position evac     EvacuateNode's fallback → releaseNodeWithClaim
//	5  changeover sweep / per node  ReleaseChangeoverWait / ReleaseChangeoverWaitForNode
//	6  a changeover node's station button: door 1 routes it to door 5
//	7  drop-situation evac          the trunk's drop arm
//	8  deferred pair leg            staged → handleSiblingReleaseRefire
//	9  swap survivor                releaseSurvivorOfFinishedPartner
//	10 changeover supply at pickup  HandleBinPickedUp → releaseDeferredSupplyAtPickup
//
// The acts still load per leg, as the doors always did: the pair click's
// finalize clears the slot before its legs' releases read the node, and
// moving that paperwork below every leg's gates is the held press (S5).

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingoedge/orders"
	"shingoedge/release"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// releaseLeg runs one leg through the trunk inside act: load what the plan
// asks for, plan, commit. released reports whether an envelope was queued.
func (e *Engine) releaseLeg(act *releaseAct, orderID int64, label, taskNode string, disp ReleaseDisposition) (bool, release.LegPlan, error) {
	ll := newLegLoad(orderID, label, taskNode, disp)
	p := release.PlanLeg(act.Act, ll.snap)
	for p.Need != 0 {
		e.loadLeg(act, ll, p.Need)
		p = release.PlanLeg(act.Act, ll.snap)
	}
	released, err := e.commitLeg(act, p, ll)
	return released, p, err
}

// ── Door 2 ────────────────────────────────────────────────────────────────

// ReleaseOrderWithLineside performs the operator's release click on one
// order: the gates (release.PlanLeg), then — for a leg every gate let go — the
// flip of a sequential line, the departing produce bin's finalize, the capture
// of parts pulled to lineside, the old bin's count, the changeover task's
// state, the flush and the OrderRelease envelope, whose remaining_uop and
// disposition Core uses to sync the bin's manifest. An order Core will not
// release is refused with Core's own account of why.
func (e *Engine) ReleaseOrderWithLineside(orderID int64, disp ReleaseDisposition) error {
	act := e.newAct(release.OriginStationOrder, 0, disp.CalledBy)
	_, _, err := e.releaseLeg(act, orderID, "", "", disp)
	return err
}

// releaseIfReleasable is the per-leg release of every door that covers legs
// it did not name one by one (the pair click, the re-fires): a leg Core will
// not take yet is passed over, not refused, and released reports whether the
// release was actually queued. act is the door's act, or a fresh one.
func (e *Engine) releaseIfReleasable(act *releaseAct, orderID int64, label string, disp ReleaseDisposition) (bool, error) {
	released, _, err := e.releaseLeg(act, orderID, label, "", disp)
	return released, err
}

// ── Door 1 ────────────────────────────────────────────────────────────────

// ReleaseStagedOrders releases both legs of a two-robot swap on the
// operator's one click. The departing bin is finalized first, then the evac
// leg goes with the operator's disposition and the supply leg with none (its
// freshly loaded bin's manifest is never cleared), each through the trunk; a
// leg Core cannot take yet is remembered and fires when it stages (door 8).
//
// A changeover node whose work is not a coordinated two-robot swap is the
// changeover act's (§6.4, and the single-leg shape, N1-d).
func (e *Engine) ReleaseStagedOrders(nodeID int64, disp ReleaseDisposition) error {
	act := e.newAct(release.OriginStationPair, nodeID, disp.CalledBy)
	pl := &pairLoad{snap: release.Pair{NodeID: nodeID}}
	p := release.PlanPair(pl.snap)
	for p.Need != 0 {
		e.loadPair(act, pl, p.Need)
		p = release.PlanPair(pl.snap)
	}
	if p.Route {
		return e.releaseChangeoverNode(pl, disp)
	}
	if err := e.commitPairPaperwork(p, pl); err != nil {
		return err
	}
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}
	evacReleased, supplyReleased := false, false
	if p.Evac != nil {
		released, err := e.releaseIfReleasable(act, *p.Evac, "evac", disp)
		if err != nil {
			return err
		}
		evacReleased = released
	}
	if p.Supply != nil {
		released, err := e.releaseIfReleasable(act, *p.Supply, "supply", supplyDisp)
		if err != nil {
			return err
		}
		supplyReleased = released
	}
	// The click expressed "go" for the whole pair: a leg that did not go while
	// its sibling did is remembered, not dropped.
	e.deferIfSiblingWent(p.Supply, p.Evac, supplyReleased, evacReleased, supplyDisp)
	e.deferIfSiblingWent(p.Evac, p.Supply, evacReleased, supplyReleased, disp)
	return nil
}

// deferIfSiblingWent is the pair's deferral for one leg (G7, release.PlanDeferral).
func (e *Engine) deferIfSiblingWent(legID, siblingID *int64, legReleased, siblingReleased bool, disp ReleaseDisposition) {
	d := e.loadDeferral(legID, siblingID, legReleased, siblingReleased)
	remember, logs := release.PlanDeferral(d)
	e.commitDeferral(remember, logs, d.Leg, disp)
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
	return nil
}

// ── Doors 5 and 6 ─────────────────────────────────────────────────────────

// ReleaseChangeoverWaitResult reports a release-wait click: Released is the
// legs whose envelopes were queued; Pending the legs a later click is owed for
// (not yet at a wait, or a position the line still pulls from); Deferred the
// paired supplies their evac's pickup releases with no click. Terminal legs
// count nowhere.
type ReleaseChangeoverWaitResult struct {
	Released int `json:"released"`
	Pending  int `json:"pending"`
	Deferred int `json:"deferred"`
	// NeedsFlip names the A/B positions a SWEEP declined because the line is
	// still pulling from them: a sweep is not aimed at one aisle, so it does
	// not move the line for the operator.
	NeedsFlip []string `json:"needs_flip,omitempty"`
}

// ReleaseChangeoverWait releases the changeover's staged legs on every node
// (the sweep). Each node's evac goes at the click, with a disposition from the
// line's runtime cache unless the caller overrides it; a paired supply is
// deferred to its evac's pickup (HandleBinPickedUp), the evac-first sequencing
// that keeps the supply robot off a slot the evac has not cleared.
func (e *Engine) ReleaseChangeoverWait(processID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, 0, disp)
}

// ReleaseChangeoverWaitForNode releases one node's changeover legs: the
// operator's per-node click, the same act narrowed to one task.
func (e *Engine) ReleaseChangeoverWaitForNode(processID, processNodeID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	return e.releaseChangeoverWaitScoped(processID, processNodeID, disp)
}

func (e *Engine) releaseChangeoverWaitScoped(processID, onlyNodeID int64, disp ReleaseDisposition) (ReleaseChangeoverWaitResult, error) {
	origin, scope := release.OriginChangeoverSweep, "all-nodes"
	if onlyNodeID != 0 {
		origin, scope = release.OriginChangeoverNode, fmt.Sprintf("node=%d", onlyNodeID)
	}
	act := e.newAct(origin, onlyNodeID, disp.CalledBy)
	cl := e.loadChangeover(processID, onlyNodeID, disp)
	plan := release.PlanChangeover(cl.snap)
	if plan.Refusal != nil {
		return ReleaseChangeoverWaitResult{}, plan.Refusal
	}
	// One line per click, before any slot, so a click that released nothing
	// is not indistinguishable in the logs from one that never happened.
	e.logFn("release_changeover_wait: process=%d changeover=%d tasks=%d scope=%s called_by=%q",
		processID, cl.coID, len(cl.tasks), scope, disp.CalledBy)
	// The supply leg rides through with no manifest action, whatever the
	// operator chose: its bin is mid-transit carrying its real count.
	supplyDisp := ReleaseDisposition{CalledBy: disp.CalledBy}
	var res ReleaseChangeoverWaitResult
	var failures []error
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
			released, lp, err := e.releaseLeg(act, s.Order, s.Kind, task.NodeName, d)
			switch {
			case err != nil:
				// Collected, not swallowed: one node's failure must reach the
				// operator by name instead of a 200 OK (ALN_001).
				failures = append(failures, err)
			case released:
				res.Released++
			case lp.Skip == release.SkipNotReleasable:
				res.Pending++
			}
		}
		if tp.Deferred {
			res.Deferred++
		}
	}
	return res, errors.Join(failures...)
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

// ── Doors 8 and 9: the pair's deferred legs ───────────────────────────────

// handleSiblingReleaseRefire fires the release the operator already asked
// for, for a pair leg the click deferred (door 8), once it stages — never a
// reaper, never an auto-cancel. A leg with no deferral, and a partner reaching
// a successful terminal, ask the survivor rule instead (door 9): the two events
// arrive in either order. A terminal leg's map entries are dropped.
func (e *Engine) handleSiblingReleaseRefire(changed OrderStatusChangedEvent) {
	newStatus := protocol.Status(changed.NewStatus)
	if protocol.IsTerminal(newStatus) {
		e.forgetLeg(changed.OrderID)
		if orders.IsTerminalSuccess(newStatus) {
			if sibling, ok := e.siblingOf(changed.OrderID); ok {
				e.releaseSurvivorOfFinishedPartner(sibling)
			}
		}
		return
	}
	if newStatus != protocol.StatusStaged {
		return
	}
	disp, ok := e.takeDeferral(changed.OrderID)
	if !ok {
		// No entry: this Edge restarted since the click, or the leg consumed
		// its entry at an earlier wait. Ask the durable question.
		e.releaseSurvivorOfFinishedPartner(changed.OrderID)
		return
	}
	act := e.newAct(release.OriginDeferral, 0, disp.CalledBy)
	released, err := e.releaseIfReleasable(act, changed.OrderID, "sibling-release-refire", disp)
	switch {
	case err != nil:
		e.logFn("sibling-release-refire: order %d reached staged but release failed: %v", changed.OrderID, err)
		e.noteReleaseHeld(changed.OrderID, err)
	case released:
		e.logFn("sibling-release-refire: order %d reached staged — released (sibling already released on the operator's click)", changed.OrderID)
	default:
		e.logFn("sibling-release-refire: order %d reached staged but was not releasable — dropped", changed.OrderID)
	}
}

// releaseSurvivorOfFinishedPartner is the durable half of the pair deferral
// (door 9): a swap leg whose partner finished successfully is owed the click,
// with the supply's zero disposition (nobody chose a UOP decision for it),
// once per order per Edge lifetime — the bound that stops a refusal flap.
// Not a sweep, not a timer, not a reaper; it never cancels and never re-plans.
func (e *Engine) releaseSurvivorOfFinishedPartner(orderID int64) {
	s := release.Survivor{OrderID: orderID}
	p := release.PlanSurvivor(s)
	for p.Need != 0 {
		e.loadSurvivor(&s, p.Need)
		p = release.PlanSurvivor(s)
	}
	e.emitLogs(p.Logs)
	if !p.Release {
		return
	}
	act := e.newAct(release.OriginSurvivor, 0, "swap-survivor-release")
	released, err := e.releaseIfReleasable(act, orderID, "swap-survivor", ReleaseDisposition{CalledBy: "swap-survivor-release"})
	switch {
	case err != nil:
		e.logFn("swap-survivor release: order %d staged with partner %d already completed, but release failed: %v",
			orderID, s.SiblingID, err)
		e.noteReleaseHeld(orderID, err)
	case released:
		e.markSurvivorFired(orderID)
		e.logFn("swap-survivor release: order %d released — its partner %d completed at %s and nothing was left to wait for",
			orderID, s.SiblingID, s.SiblingStatus)
	default:
		e.logFn("swap-survivor release: order %d staged with partner %d already completed, but it was not releasable at Core",
			orderID, s.SiblingID)
	}
}

// ── Door 10 ───────────────────────────────────────────────────────────────

// releaseDeferredSupplyAtPickup releases the changeover supply the sweep
// deferred to its evac's lift, at the moment the slot is physically clear —
// the pickup block FINISHED, not the evac's completion. A supply Core will not
// take yet is passed over; it stages on its own and the next click fires it.
func (e *Engine) releaseDeferredSupplyAtPickup(supplyID int64, evacUUID string) {
	passed, perr := e.supplyHistory(supplyID)
	fire, logs := release.PlanPickup(supplyID, evacUUID, passed, perr)
	e.emitLogs(logs)
	if !fire {
		return
	}
	act := e.newAct(release.OriginPickup, 0, "auto-evac-pickup")
	if _, err := e.releaseIfReleasable(act, supplyID, "deferred-supply-after-evac-pickup", ReleaseDisposition{CalledBy: "auto-evac-pickup"}); err != nil {
		e.logFn("bin_picked_up: deferred-supply release order %d for evac %s: %v", supplyID, evacUUID, err)
		e.noteReleaseHeld(supplyID, err)
	}
}
