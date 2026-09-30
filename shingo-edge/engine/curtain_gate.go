package engine

// curtain_gate.go - the FG light-curtain release interlock.
//
// A light curtain stands at a finished-goods pickup. A physical bypass button
// at the cell writes a BOOL tag that mutes it. With the interlock enabled on a
// NODE, a release is only allowed when that node's tag reads the node's
// configured release value - checked by a DIRECT WarLink read at the moment of
// the release (plc.Manager.ReadTagDirect), never the poll cache, because a
// safety decision must not be older than the request that made it.
//
// ── THE BIN TRIPS THE CURTAIN, NOT THE ROBOT ──────────────────────────────
//
// The gate covers every curtained node where the leg's next move PICKS UP OR
// DROPS a bin. An empty robot driving in to its wait is not gated; the lift or
// the set-down that follows the release is. Until the Edge knows which wait a
// leg is parked at, "the next move" is read as every pickup and dropoff after
// the leg's first station wait - a superset for a leg already at a later wait,
// never a subset.
//
// PER NODE, NOT PER ROLE. A node whose curtain is on is curtained. The role
// check this gate used to make was the per-process configuration's proxy for
// "which of this process's nodes has a curtain"; with the curtain configured on
// the node, the node says so itself, and a release whose claim cannot be
// resolved is gated like any other (it used to pass: A4).
//
// NO EXEMPTIONS. A changeover robot crosses the same curtain an ordinary one
// does, so a changeover release is gated like any other.
//
// ONE READ PER NODE PER ACT. An act - one click, or one automatic re-fire -
// reads each curtained node once (releaseAct), so a pair click cannot pass the
// check for one leg and fail it for the other, and a door that releases its
// legs through the per-leg trunk does not read again below its paperwork.
//
// FAIL-CLOSED, on everything. Every refusal here is a click the operator
// repeats once the curtain is released, while every wrong pass is a release the
// curtain was there to prevent:
//
//   - the node's curtain list cannot be read - refuse;
//   - the interlock is on but its PLC/tag pointers are missing - refuse;
//   - the interlock is on but its polarity was never chosen - refuse (a guessed
//     polarity is an inverted gate wherever the guess is wrong);
//   - the tag cannot be read (WarLink down, PLC unknown, tag unpublished) -
//     refuse, with the reason named;
//   - the tag reads something that is not recognisably a BOOL - refuse;
//   - the value disagrees with the configured release value - refuse.
//
// The render half is elsewhere: the station view folds the tags' cache values
// into the node entry so the RELEASE button greys out before the click. That is
// UX. This gate is the interlock.

import (
	"context"
	"errors"
	"fmt"
	"time"

	"shingo/protocol"
	"shingoedge/plc"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// curtainReadTimeout bounds the direct WarLink read. A release click waits
// at most this long for the curtain's answer; past it the release refuses.
// Short on purpose - WarLink answers in milliseconds on a healthy plant, and
// a gate that hangs the operator's button is its own failure.
const curtainReadTimeout = 3 * time.Second

// CurtainHeldError is the interlock's refusal. Its text is the sentence the
// operator sees - on the toast for a click, on the order's chip for an
// automatic re-fire.
type CurtainHeldError struct {
	Node     string
	Sentence string
}

func (c *CurtainHeldError) Error() string { return c.Sentence }

// releaseAct carries what one act has already read, so each curtained node is
// read once per act however many legs and doors the act goes through.
type releaseAct struct {
	curtainedLoaded bool
	curtainedErr    error
	curtained       map[string]*processes.Node            // by core node name
	verdict         map[int64]error                       // by node id; nil = clear
	touches         map[int64][]string                    // by order id: the nodes its release lets a bin cross
	steps           map[int64][]protocol.ComplexOrderStep // by order id; nil entry = no steps
	departs         map[int64]bool                        // by order id: releasing it takes a produce bin away
}

func newReleaseAct() *releaseAct { return &releaseAct{} }

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

// legSteps is a leg's decoded steps, read once per act and shared by every
// question the act asks of the leg (the curtain's touched nodes, the departing
// produce bin). nil with no error: the order has no steps (a plain retrieve or
// move).
func (e *Engine) legSteps(act *releaseAct, orderID int64) ([]protocol.ComplexOrderStep, error) {
	if st, ok := act.steps[orderID]; ok {
		return st, nil
	}
	stepsJSON, err := e.db.GetOrderStepsJSON(orderID)
	if err != nil {
		return nil, fmt.Errorf("order %d: load steps: %w", orderID, err)
	}
	var steps []protocol.ComplexOrderStep
	if stepsJSON != "" && stepsJSON != "null" && stepsJSON != "[]" {
		if steps, err = decodeSteps(stepsJSON); err != nil {
			return nil, stepsUndecodable{fmt.Errorf("order %d: %w", orderID, err)}
		}
	}
	if act.steps == nil {
		act.steps = map[int64][]protocol.ComplexOrderStep{}
	}
	act.steps[orderID] = steps
	return steps, nil
}

// stepsUndecodable is a leg whose stored steps did not decode, as opposed to a
// leg whose steps could not be read or has none.
type stepsUndecodable struct{ error }

// legTouches is the nodes a leg's release lets a bin cross, read once per leg
// per act: a door that checked the leg does not read its steps again when it
// releases it through the trunk.
func (e *Engine) legTouches(act *releaseAct, order *storeorders.Order) ([]string, error) {
	if t, ok := act.touches[order.ID]; ok {
		return t, nil
	}
	var touch []string
	steps, err := e.legSteps(act, order.ID)
	var undecodable stepsUndecodable
	switch {
	case err != nil && !errors.As(err, &undecodable), err == nil && steps == nil:
		if order.ProcessNodeID != nil {
			if n, nerr := e.db.GetProcessNode(*order.ProcessNodeID); nerr == nil && n != nil {
				touch = []string{n.CoreNodeName}
			}
		}
	case err != nil:
		return nil, &CurtainHeldError{Sentence: fmt.Sprintf(
			"order %d's steps could not be read to check the light curtain (%v) - release held", order.ID, err)}
	default:
		touch = binTouchesAfterFirstStationWait(steps)
	}
	if act.touches == nil {
		act.touches = map[int64][]string{}
	}
	act.touches[order.ID] = touch
	return touch, nil
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

// binTouchesAfterFirstStationWait is every node a leg picks up at or drops at
// after its first station wait - the moves a release lets go. A leg with no
// station wait has nothing a release lets go (its lifts happen at dispatch).
func binTouchesAfterFirstStationWait(steps []protocol.ComplexOrderStep) []string {
	var out []string
	released := false
	for _, s := range steps {
		if !released {
			if s.Action == protocol.ActionWait && (s.WaitKind == waitKindStation || s.WaitKind == "") {
				released = true
			}
			continue
		}
		if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && s.Node != "" {
			out = append(out, s.Node)
		}
	}
	return out
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

// noteReleaseHeld puts an automatic release's curtain refusal on the order, for
// the board's chip. Only the curtain's refusals: every other reason an automatic
// release declines is the deferral working, and has its own account.
func (e *Engine) noteReleaseHeld(orderID int64, err error) {
	var held *CurtainHeldError
	if !errors.As(err, &held) {
		return
	}
	if nerr := e.orderMgr.NoteReleaseHeld(orderID, held.Sentence); nerr != nil {
		e.logFn("release held for order %d (%s) but the note could not be written: %v", orderID, held.Sentence, nerr)
	}
}
