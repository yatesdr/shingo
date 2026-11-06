// catid_attribute.go — a counted tick's style, from the part the PLC reported.
//
// The poll stamps every counted tick before its snapshot row is written
// (plc.EventEmitter.AttributeTick): the row, the engine's walk, hourly and Core
// all count it under the same ATTRIBUTED style. The value is the monitor's
// confirmed CATID read from its lock-free snapshot, used only while the last
// good read is fresh — never a raw same-pass read (the cache fills one tag per
// WarLink event, so a raw read is not one PLC scan) and never under cm.mu: the
// poll does not wait on the monitor.

package engine

import (
	"time"

	"shingo/protocol/clock"
	"shingoedge/plc"
)

// tickIdentityFresh is how old the monitor's last good identity read may be
// for a tick to be attributed by it.
const tickIdentityFresh = 5 * time.Second

// attributeTick is the poll's AttributeTick. With no monitor, no snapshot, a
// stale read or a value that names no part, the tick is the point's style.
// Otherwise attributeCATID decides over the process-wide sets — one statement,
// on identity ticks only. An off-plan tick walks and ships under the style
// that runs its part; a held tick ships under the point's style and its draw
// goes to the holds ledger (walkTick).
func (e *Engine) attributeTick(processID, pointStyleID int64) plc.TickIdentity {
	today := plc.TickIdentity{StyleID: pointStyleID, Kind: plc.TickToday}
	cm := e.tickMonitor.Load()
	if cm == nil {
		return today
	}
	snap, ok := cm.snapshot(processID)
	if !ok || snap.lastOK.IsZero() || clock.Now().Sub(snap.lastOK) > tickIdentityFresh || !isIdentity(snap.value) {
		return today
	}
	return attributeCATID(snap.value, pointStyleID, e.processStyleCATIDSets(processID), snap.observed, snap.runningOut())
}

// attributeCATID is the attribution rule (pure). A is the active style, T the
// open changeover's target, proven(A) = A's set holds a value this process has
// emitted, R the style running out (runningOut: the latest landing left R
// while v was already confirmed, 0 when none):
//
//	A's set empty                                → today, the point's style
//	v in A's set (a CATID A shares included)     → today, A
//	v in T's set (alone or among others)         → target, T
//	A's set not proven                           → today, the point's style
//	v in R's set: the run-out after a landing    → today, the point's style
//	v in exactly one other style's set, S        → off-plan, S
//	v in no style's set, or several              → held, the point's style
//
// The run-out is today's, so the incoming cart prepays it after the cutover as
// it did before (TestTick_RunOutAfterAnEarlyCutoverPrepaysTheIncomingCart).
func attributeCATID(v string, pointStyleID int64, sets []styleCATIDEntry, observed map[string]struct{}, runningOut int64) plc.TickIdentity {
	id := plc.TickIdentity{StyleID: pointStyleID, CATID: v, Kind: plc.TickToday}
	var active, target *styleCATIDEntry
	for i := range sets {
		if sets[i].Active {
			active = &sets[i]
		}
		if sets[i].Target {
			target = &sets[i]
		}
	}
	if active == nil || len(active.Set) == 0 {
		return id
	}
	if catidSetHas(active.Set, v) {
		id.StyleID = active.ID
		return id
	}
	if target != nil && catidSetHas(target.Set, v) {
		id.StyleID, id.Kind = target.ID, plc.TickTarget
		return id
	}
	if !proven(active.Set, observed) {
		return id
	}
	var only *styleCATIDEntry
	n := 0
	for i := range sets {
		if sets[i].ID == runningOut && catidSetHas(sets[i].Set, v) {
			return id
		}
		if catidSetHas(sets[i].Set, v) {
			n++
			only = &sets[i]
		}
	}
	if n == 1 {
		id.StyleID, id.Kind = only.ID, plc.TickOffPlan
		return id
	}
	id.Kind = plc.TickHeld
	return id
}

// noteCountedTick records a counted tick the poll attributed off today — to
// the open changeover's target, off-plan to another style, or held — and
// pokes the monitor, which decides on its own goroutine (the counted-part
// force, the automatic switch, the ambiguity prompt): the poll never enters
// the door. No lock.
func (e *Engine) noteCountedTick(processID int64, id plc.TickIdentity) {
	if id.Kind == plc.TickToday {
		return
	}
	cm := e.tickMonitor.Load()
	if cm == nil {
		return
	}
	cm.countedTicks.Store(processID, countedTick{catid: id.CATID, kind: id.Kind, at: clock.Now()})
	e.pokeCutover(processID)
}

// noteLanding publishes a landing (the door's completion or a switch) on the
// monitor's snapshot just before its flip commits the new active style: the
// poll attributes from that flag, so a run-out stroke read between the flip and
// a later publish would be off-plan. A flip that then fails leaves the landing
// noted on an unchanged style, which moves no attribution (a value of the
// active style's is today's first) and only makes the switch wait for a later
// change. Dated on the monitor's clock, as a confirm is.
func (e *Engine) noteLanding(processID int64, fromStyleID *int64) {
	if e.catidMon == nil {
		return
	}
	var from int64
	if fromStyleID != nil {
		from = *fromStyleID
	}
	e.catidMon.noteLanding(processID, from, time.Now())
}

// proven reports whether a set holds a value the process has emitted. Values
// that name no part are evidence of nothing.
func proven(set, observed map[string]struct{}) bool {
	for v := range observed {
		if isIdentity(v) && catidSetHas(set, v) {
			return true
		}
	}
	return false
}
