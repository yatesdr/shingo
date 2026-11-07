// repoint_catch_up.go — the one-click re-point counts the strokes since the
// PLC moved.
//
// When the PLC moves its count from the process's slot to a sibling, no point
// reads the sibling until the counter is re-pointed. The slot watch keeps
// each sibling's count when the PLC moved (slotState.base); a re-point to the
// slot it has leading seeds the point there at that count, so the point's
// next read counts every stroke since the move, once, as one delta (a jump
// past the poll's threshold when it is a large one, counted like any other).
// That read waits for the monitor's first read of the new slot's identity:
// the poll asks catchUpHeld each pass and writes nothing for the point while
// it is held, so the strokes are attributed by the part the PLC reports there
// — for at most catchUpWait, then they count with no part identity, under the
// point's style. With no base (the watch has not seen the counted slot rise
// since its start) the point seeds at its first read, as any re-point does,
// and the strip's item says which strokes the record lacks.

package engine

import (
	"log"
	"time"

	"shingo/protocol/clock"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// catchUpWait is how long a seeded point waits for the monitor's first read
// of the new slot's identity before it counts without one.
const catchUpWait = 30 * time.Second

// catchUp is a re-pointed point waiting for the monitor's read of its
// process's new identity tag.
type catchUp struct {
	processID   int64
	identityTag string
	until       time.Time
}

func catchUpKey(plcName, tagName string) string { return plcName + "\x00" + tagName }

// noCatchUp is beginCatchUp's step when there is nothing to catch up.
func noCatchUp(bool) {}

// beginCatchUp starts a re-point's catch-up when the edit moves the counter
// to a sibling slot the watch has leading with a base: it holds the point on
// the new tag from the poll before the point moves (so no pass seeds it in
// between) and returns the step that, once the point has moved, seeds it at
// the base, or, when the move failed or left no unread point there, lets it
// go. Under cutoverMu, after the edit's refusals.
func (e *Engine) beginCatchUp(old *processes.Process, edit domain.ProcessCounterEdit) func(moved bool) {
	if e.catidMon == nil || edit.PLCName != old.CounterPLCName || edit.TagName == old.CounterTagName {
		return noCatchUp
	}
	s, ok := e.catidMon.leadingSlot(old, edit.TagName)
	if !ok || !s.baseOK {
		return noCatchUp
	}
	identity := identityTagOf(&processes.Process{CounterPLCName: edit.PLCName, CounterTagName: edit.TagName,
		IdentityTagName: edit.IdentityTagName, IdentityNone: edit.IdentityNone})
	key := catchUpKey(edit.PLCName, edit.TagName)
	e.catchUps.Store(key, &catchUp{processID: old.ID, identityTag: identity, until: clock.Now().Add(catchUpWait)})
	return func(moved bool) {
		if !moved || !e.seedCatchUp(edit.PLCName, edit.TagName, s) || identity == "" {
			e.catchUps.Delete(key)
		}
	}
}

// seedCatchUp seeds the point on (plcName, tagName) at the slot's base when
// the re-point left it unread; a point already reading the tag keeps its
// read. Reports whether it seeded.
func (e *Engine) seedCatchUp(plcName, tagName string, s slotState) bool {
	rp, err := e.db.GetReportingPointByTag(plcName, tagName)
	if err != nil || rp == nil || rp.LastPollAt != nil {
		return false
	}
	if wrote, err := e.db.UpdateReportingPointCounter(rp.ID, plcName, tagName, s.base); err != nil || !wrote {
		log.Printf("reporting point %d (%s/%s): seed at %d: written=%t %v — it seeds at its first read", rp.ID, plcName,
			tagName, s.base, wrote, err)
		return false
	}
	log.Printf("reporting point %d (%s/%s): seeded at %d, its count when the PLC moved the count there (read %s); its next read counts the strokes since",
		rp.ID, plcName, tagName, s.base, s.baseAt.Format(time.RFC3339))
	return true
}

// catchUpHeld is the poll's question each pass (plc.Manager.SetPointHold):
// whether the point on (plcName, tagName) waits, its catch-up not yet
// attributable. It lets go once the monitor has read the new identity tag
// (or holds the process no more), or at the wait's end. No lock: the
// snapshot and a map entry.
func (e *Engine) catchUpHeld(plcName, tagName string) bool {
	key := catchUpKey(plcName, tagName)
	v, ok := e.catchUps.Load(key)
	if !ok {
		return false
	}
	c := v.(*catchUp)
	if e.identityReadOn(c.processID, c.identityTag) {
		e.catchUps.CompareAndDelete(key, v)
		return false
	}
	if clock.Now().Before(c.until) {
		return true
	}
	if e.catchUps.CompareAndDelete(key, v) {
		log.Printf("reporting point %s/%s: no read of %s in %v: the strokes since the PLC moved count with no part identity",
			plcName, tagName, c.identityTag, catchUpWait)
	}
	return false
}

// identityReadOn reports whether the monitor has read tag for the process
// since it was primed on it, or will not read it (no monitor, or none for
// the process).
func (e *Engine) identityReadOn(processID int64, tag string) bool {
	cm := e.tickMonitor.Load()
	if cm == nil {
		return true
	}
	snap, ok := cm.snapshot(processID)
	return !ok || (snap.tag == tag && !snap.lastOK.IsZero())
}

// lastCountedAt is when the process's latest counted row was read, zero when
// it has none or the read failed.
func (e *Engine) lastCountedAt(processID int64) time.Time {
	t, err := e.db.LastCountedAt(processID)
	if err != nil {
		log.Printf("process %d: the record's last count: %v", processID, err)
	}
	return t
}
