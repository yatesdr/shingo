package engine

// release_act.go — the act's commit, its loads, and the intent worker's
// plumbing (S5, SHAPE §3.5-3.6).

import (
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"shingo/protocol"
	"shingoedge/release"
	storeorders "shingoedge/store/orders"
)

// ── The act's commit ──────────────────────────────────────────────────────

// commitAct carries out an act's plan, in SHAPE §3.6's order:
//
//  1. a refused act changes nothing;
//  2. the press: an operator's act declares what it declares whether its legs
//     go now or hold — the departing produce bin's count splits at the press,
//     and a sequential line moves to its partner (the held press);
//  3. the intents, for every leg the act covers, before any envelope;
//  4. each leg that goes, lifters first: its paperwork, the flush, the
//     envelope with its echo, and the intent's sent_at in the outbox's
//     transaction; each leg that holds: the sentence for the chip.
//
// A failure is I/O only, after every gate: the unsent intents remain and the
// worker retries them.
func (e *Engine) commitAct(act *releaseAct, p release.ActPlan, lls []*legLoad) error {
	e.emitLogs(p.Logs)
	if p.Refusal != nil {
		return p.Refusal
	}
	operator := !act.Reevaluation
	if operator && act.pressPaperwork != nil && anyCovered(p) {
		if err := act.pressPaperwork(); err != nil {
			return err
		}
	}
	for _, d := range p.Decisions {
		ll := lls[d.Index]
		if !operator || (d.Verdict != release.Go && d.Verdict != release.Hold) {
			continue
		}
		if d.Trunk.Flip {
			if err := e.commitFlip(ll); err != nil {
				return err
			}
			ll.flipped = true
		}
		if d.Trunk.Finalize && ll.order != nil {
			if err := e.finalizeDepartingProduce(ll.node, ll.runtime, ll.order); err != nil {
				return err
			}
			ll.finalized = true
		}
	}
	for _, d := range p.Decisions {
		if d.Verdict == release.Go || d.Verdict == release.Hold {
			e.writeIntent(act, d, lls[d.Index])
		}
	}
	for _, d := range p.Decisions {
		ll := lls[d.Index]
		switch d.Verdict {
		case release.Go:
			e.emitLogs(d.Logs)
			trunk := d.Trunk
			trunk.Logs = nil
			if _, err := e.commitLeg(act, trunk, ll); err != nil {
				act.fail(d.OrderID, err)
				continue
			}
			if ll.order != nil && ll.order.ReleaseHeld != "" {
				e.setReleaseHeld(d.OrderID, "")
			}
		case release.Hold:
			e.emitLogs(d.Logs)
			if d.DropIntent {
				if err := e.db.SetOrderReleaseIntent(d.OrderID, ""); err != nil {
					e.logRelease("order=%d: drop release intent: %v", d.OrderID, err)
				}
			}
			// Once per sentence: a held leg is re-planned on every press and
			// every 15 s floor, and a bypass can last a shift.
			if ll.order == nil || ll.order.ReleaseHeld != d.Sentence {
				e.setReleaseHeld(d.OrderID, d.Sentence)
				if d.Gate == release.G6 {
					e.noteReleaseHeld(d.OrderID, d.Sentence)
				}
			}
			e.logRelease("order=%d held at %s: %s", d.OrderID, d.Gate, d.Sentence)
		default:
			e.emitLogs(d.Logs)
		}
	}
	if err := errors.Join(act.failures...); err != nil {
		return err
	}
	if operator {
		return p.Held()
	}
	return nil
}

func anyCovered(p release.ActPlan) bool {
	for _, d := range p.Decisions {
		if d.Verdict == release.Go || d.Verdict == release.Hold {
			return true
		}
	}
	return false
}

// writeIntent records the act's promise to a leg for its station wait. An
// intent already held for the same wait keeps its sent_at and re-send mark;
// a re-evaluation never rewrites one.
func (e *Engine) writeIntent(act *releaseAct, d release.Decision, ll *legLoad) {
	if act.Reevaluation || ll.order == nil || (!ll.point.HasWait && !ll.point.NoWaits) {
		return
	}
	in := release.Intent{
		StationWait: ll.point.Ordinal,
		Purpose:     ll.point.Purpose,
		Origin:      act.Origin,
		CalledBy:    act.CalledBy,
		Choices: release.Choices{
			Mode: string(ll.disp.Mode), Captures: ll.disp.LinesideCapture, PartialCount: ll.disp.PartialCount,
		},
	}
	if old := ll.snap.Intent; old != nil && old.StationWait == in.StationWait {
		in.SentAt, in.Resent = old.SentAt, old.Resent
	}
	raw, err := in.Encode()
	if err == nil {
		err = e.db.SetOrderReleaseIntent(d.OrderID, raw)
	}
	if err != nil {
		e.logRelease("order=%d: release intent not written (%v)", d.OrderID, err)
		return
	}
	ll.snap.Intent = &in
}

func (e *Engine) setReleaseHeld(orderID int64, sentence string) {
	if err := e.db.SetOrderReleaseHeld(orderID, sentence); err != nil {
		e.logRelease("order=%d: release held sentence not written (%v)", orderID, err)
	}
}

// fail records a leg's commit failure on the act.
func (a *releaseAct) fail(orderID int64, err error) {
	if a.failed == nil {
		a.failed = map[int64]bool{}
	}
	a.failed[orderID] = true
	a.failures = append(a.failures, err)
}

// commitPairDoor is the pair door's own verdict: a refusal changes nothing.
func (e *Engine) commitPairDoor(p release.PairPlan) error {
	e.emitLogs(p.Logs)
	if p.Verdict == release.Refuse {
		return p.Refusal
	}
	return nil
}

// ── The act's loads ───────────────────────────────────────────────────────

// loadAct reads an act-level group: Core's point for the act's live legs, in
// one round trip (G3 when it fails).
func (e *Engine) loadAct(act *releaseAct, lls []*legLoad, need release.Need) {
	if need == release.NeedPoint {
		e.loadPoints(act, lls)
	}
	act.actFacts.Loaded |= need
}

func (e *Engine) loadPoints(act *releaseAct, lls []*legLoad) {
	var uuids []string
	byUUID := map[string]int64{}
	for _, ll := range lls {
		if ll.order == nil || protocol.IsTerminal(ll.order.Status) {
			continue
		}
		uuids = append(uuids, ll.order.UUID)
		byUUID[ll.order.UUID] = ll.order.ID
	}
	if len(uuids) == 0 {
		return
	}
	src := e.points
	if src == nil {
		if e.coreClient == nil || !e.coreClient.Available() {
			act.actFacts.PointErr = errors.New("core API not configured")
			return
		}
		src = e.coreClient
	}
	pts, err := src.ReleasePoints(e.cfg.StationID(), uuids)
	if err != nil {
		act.actFacts.PointErr = err
		e.logRelease("release points: %v — holding what would go until Core answers", err)
		return
	}
	act.actFacts.Points = map[int64]release.Point{}
	for _, pt := range pts {
		id, ok := byUUID[pt.OrderUUID]
		if !ok {
			continue
		}
		out := release.Point{Found: pt.Found, Enters: pt.Enters}
		for _, dep := range pt.AwaitsLift {
			lifter, ok := byUUID[dep.LifterUUID]
			if !ok {
				if o, err := e.db.GetOrderByUUID(dep.LifterUUID); err == nil && o != nil {
					lifter = o.ID
				}
			}
			out.AwaitsLift = append(out.AwaitsLift, release.LiftDep{Lifter: lifter, Node: dep.Node, CoRelease: dep.CoRelease})
		}
		act.actFacts.Points[id] = out
	}
}

// releasePointSource is Core's releasePoints read (the Core client).
type releasePointSource interface {
	ReleasePoints(station string, orderUUIDs []string) ([]protocol.ReleasePoint, error)
}

// ── The intent worker ─────────────────────────────────────────────────────

// heldIntent is one leg a node's intents hold, with its choices.
type heldIntent struct {
	orderID int64
	disp    ReleaseDisposition
}

func (e *Engine) heldIntentLegs(nodeID int64) []heldIntent {
	ids, err := e.db.ListReleaseIntentOrders(nodeID)
	if err != nil {
		e.logRelease("release intents at node %d: %v", nodeID, err)
		return nil
	}
	out := make([]heldIntent, 0, len(ids))
	for _, id := range ids {
		o, err := e.db.GetOrder(id)
		if err != nil {
			continue
		}
		in, err := release.DecodeIntent(o.ReleaseIntent)
		// A sent intent is not held: its release went out, and only Core
		// staging it again at the same wait (OnStaged's one re-send) makes it
		// held again.
		if err != nil || in == nil || in.Sent() {
			continue
		}
		out = append(out, heldIntent{orderID: id, disp: ReleaseDisposition{
			Mode: ReleaseDispositionMode(in.Choices.Mode), LinesideCapture: in.Choices.Captures,
			PartialCount: in.Choices.PartialCount, CalledBy: in.CalledBy,
		}})
	}
	return out
}

// intentQueue is the worker's deduplicated node queue.
type intentQueue struct {
	mu      sync.Mutex
	order   []int64
	why     map[int64]string
	running bool          // a worker goroutine drains it
	signal  chan struct{} // the worker's wake
}

// nodeLocks is one lock per node, shared by the click doors and the worker,
// and the count of acts in flight on this goroutine's behalf: a wake raised
// inside an act is drained when the act ends, never under its lock.
type nodeLocks struct {
	mu       sync.Mutex
	locks    map[int64]*sync.Mutex
	inFlight int
}

func (e *Engine) nodeLock(nodeID int64) *sync.Mutex {
	e.relLocks.mu.Lock()
	defer e.relLocks.mu.Unlock()
	if e.relLocks.locks == nil {
		e.relLocks.locks = map[int64]*sync.Mutex{}
	}
	l := e.relLocks.locks[nodeID]
	if l == nil {
		l = &sync.Mutex{}
		e.relLocks.locks[nodeID] = l
	}
	return l
}

// lockNodes takes the nodes' locks in id order (no two acts can deadlock on
// them) and returns their release.
func (e *Engine) lockNodes(nodeIDs []int64) func() {
	ids := append([]int64(nil), nodeIDs...)
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	var held []*sync.Mutex
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		l := e.nodeLock(id)
		l.Lock()
		held = append(held, l)
	}
	e.relLocks.mu.Lock()
	e.relLocks.inFlight++
	e.relLocks.mu.Unlock()
	return func() {
		for i := len(held) - 1; i >= 0; i-- {
			held[i].Unlock()
		}
		e.relLocks.mu.Lock()
		e.relLocks.inFlight--
		idle := e.relLocks.inFlight == 0
		e.relLocks.mu.Unlock()
		if idle {
			e.drainIntentsInline()
		}
	}
}

func (e *Engine) lockNode(nodeID int64) func() { return e.lockNodes([]int64{nodeID}) }

// lockOrderNode locks the order's process node (none: no lock to take).
func (e *Engine) lockOrderNode(orderID int64) func() {
	if o, err := e.db.GetOrder(orderID); err == nil && o.ProcessNodeID != nil {
		return e.lockNode(*o.ProcessNodeID)
	}
	return e.lockNodes(nil)
}

// wakeIntents asks the worker to re-plan a node's held intents.
func (e *Engine) wakeIntents(nodeID int64, why string) {
	q := &e.intents
	q.mu.Lock()
	if q.why == nil {
		q.why = map[int64]string{}
	}
	if _, queued := q.why[nodeID]; !queued {
		q.order = append(q.order, nodeID)
	}
	q.why[nodeID] = why
	running, signal := q.running, q.signal
	q.mu.Unlock()
	if running {
		select {
		case signal <- struct{}{}:
		default:
		}
		return
	}
	e.relLocks.mu.Lock()
	idle := e.relLocks.inFlight == 0
	e.relLocks.mu.Unlock()
	if idle {
		e.drainIntentsInline()
	}
}

// drainIntentsInline runs the queued wakes on this goroutine, when no worker
// goroutine runs them (tests, and an engine not started).
func (e *Engine) drainIntentsInline() {
	q := &e.intents
	q.mu.Lock()
	if q.running {
		q.mu.Unlock()
		return
	}
	q.mu.Unlock()
	e.drainIntents()
}

func (e *Engine) drainIntents() {
	q := &e.intents
	for {
		q.mu.Lock()
		if len(q.order) == 0 {
			q.mu.Unlock()
			return
		}
		node := q.order[0]
		q.order = q.order[1:]
		why := q.why[node]
		delete(q.why, node)
		q.mu.Unlock()
		e.fireIntents(node, why)
	}
}

// intentFloor is the floor's period: machine-owned holds (Core's point, the
// curtain, a lift) are re-planned this often while they stand.
const intentFloor = 15 * time.Second

// startIntentWorker runs the worker goroutine, the 15 s floor, and the boot
// sweep of every node holding an intent.
func (e *Engine) startIntentWorker() {
	q := &e.intents
	q.mu.Lock()
	q.running, q.signal = true, make(chan struct{}, 1)
	q.mu.Unlock()
	go func() {
		for range q.signal {
			e.drainIntents()
		}
	}()
	go func() {
		t := time.NewTicker(intentFloor)
		defer t.Stop()
		for range t.C {
			e.wakeIntentNodes("floor")
		}
	}()
	e.wakeIntentNodes("boot")
}

// wakeIntentNodes wakes every node holding an intent.
func (e *Engine) wakeIntentNodes(why string) {
	nodes, err := e.db.ListReleaseIntentNodes()
	if err != nil {
		e.logRelease("release intents: list nodes: %v", err)
		return
	}
	for _, n := range nodes {
		e.wakeIntents(n, why)
	}
}

// ── The wakes ─────────────────────────────────────────────────────────────

// onStagedMessage is Core's OrderStaged — the message, never the status
// event, so a rollback cannot wake a re-fire. A leg staged at a later station
// wait has consumed its intent; staged again at the same wait after its
// release went out (the faulted no-op), the release is re-sent once. Either
// way the leg's node, and its sibling's, are re-planned.
func (e *Engine) onStagedMessage(orderID int64, stationWait *int) {
	o, err := e.db.GetOrder(orderID)
	if err != nil {
		return
	}
	in, _ := release.DecodeIntent(o.ReleaseIntent)
	switch in.OnStaged(stationWait) {
	case release.StageConsume:
		if err := e.db.SetOrderReleaseIntent(orderID, ""); err != nil {
			e.logRelease("order=%d: consume release intent: %v", orderID, err)
		}
	case release.StageAdvance:
		// A creation's standing intent (S7): the leg reached the next wait put
		// in front of a curtained node, and the intent now waits there.
		in.StationWait, in.SentAt, in.Resent = *stationWait, "", false
		if raw, err := in.Encode(); err == nil {
			if err := e.db.SetOrderReleaseIntent(orderID, raw); err != nil {
				e.logRelease("order=%d: advance release intent: %v", orderID, err)
			}
		}
	case release.StageResend:
		in.SentAt, in.Resent = "", true
		if raw, err := in.Encode(); err == nil {
			if err := e.db.SetOrderReleaseIntent(orderID, raw); err != nil {
				e.logRelease("order=%d: re-send release intent: %v", orderID, err)
			}
		}
		e.logRelease("order=%d staged again at station wait %d after its release went out — re-sending once", orderID, in.StationWait)
	}
	e.wakeOrderNodes(o, "staged")
}

// wakeOrderNodes wakes the order's node and its sibling's.
func (e *Engine) wakeOrderNodes(o *storeorders.Order, why string) {
	if o.ProcessNodeID != nil {
		e.wakeIntents(*o.ProcessNodeID, why)
	}
	if o.SiblingOrderID != nil {
		if s, err := e.db.GetOrder(*o.SiblingOrderID); err == nil && s.ProcessNodeID != nil {
			e.wakeIntents(*s.ProcessNodeID, fmt.Sprintf("%s (sibling %d)", why, o.ID))
		}
	}
}

// onPickupForIntents is BinPickedUp for any leg, at any location — before the
// pickup handler's station-node filter: a lifter's lift is a held placer's
// wake wherever it happened.
func (e *Engine) onPickupForIntents(orderUUID string) {
	if o, err := e.db.GetOrderByUUID(orderUUID); err == nil && o != nil {
		e.wakeOrderNodes(o, "lift")
	}
}

// onReleaseStatusChanged is the status event's one release duty: a leg that
// ended wakes its sibling's node. (Staged is the message's, not the event's.)
func (e *Engine) onReleaseStatusChanged(changed OrderStatusChangedEvent) {
	if protocol.IsTerminal(protocol.Status(changed.NewStatus)) {
		e.onTerminalForIntents(changed.OrderID)
	}
}

// onTerminalForIntents wakes a terminal leg's sibling: its placer may wait on
// it no longer, or die with it.
func (e *Engine) onTerminalForIntents(orderID int64) {
	if o, err := e.db.GetOrder(orderID); err == nil {
		e.wakeOrderNodes(o, "sibling ended")
	}
}
