// admin_style_switch.go — the admin's style switch ("Mark as running").
//
// The admin switch is a correction to shingo's record of what a cell runs, and
// every branch of it leaves a completed process_changeovers row:
//
//	changeover open, to its target       the door, forced: the row completes
//	                                     (admin-switch, or admin-switch-forced
//	                                     with the overridden blockers in notes)
//	changeover open, to a third style    the door, forced, to the open target;
//	                                     then a flip to the chosen style and a
//	                                     second completed row (admin-switch)
//	changeover open, to the active style refused: Cancel is the way to stay
//	no changeover, to another style      the flip and a completed row
//	                                     (admin-switch) with no tasks, orders
//	                                     or episode
//
// Nothing here starts, cancels or plans a changeover, or touches an order or a
// node task: a forced completion leaves every leg running, and a leg still
// moving lands as an ordinary delivery once no changeover is active.

package engine

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"shingoedge/store/processes"
)

// SwitchRefused is an admin switch refused because of the cell's state; the
// station's page shows Sentence as it stands.
type SwitchRefused struct{ Sentence string }

func (r *SwitchRefused) Error() string { return r.Sentence }

// SetProcessActiveStyle is the admin's style switch, dispatched by the
// process's changeover state (the table at the top of this file). It holds the
// door's mutex across the whole switch, so the operator's CUTOVER, the CATID
// monitor and the restore cannot interleave with it.
func (e *Engine) SetProcessActiveStyle(processID int64, styleID *int64) error {
	e.cutoverMu.Lock()
	defer e.cutoverMu.Unlock()
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	changeover, err := e.db.GetActiveProcessChangeover(processID)
	if errors.Is(err, sql.ErrNoRows) {
		return e.adminSwitchNoChangeover(proc, styleID)
	}
	if err != nil {
		return err
	}
	switch {
	case styleID == nil:
		return &SwitchRefused{Sentence: fmt.Sprintf("A changeover to %s is open — cancel it before clearing the running style.",
			changeover.ToStyleName)}
	case *styleID == changeover.ToStyleID:
		_, err := e.completeCutoverLocked(processID, changeover.ID, triggeredAdminSwitch, CutoverForced)
		return err
	case proc.ActiveStyleID != nil && *styleID == *proc.ActiveStyleID:
		return &SwitchRefused{Sentence: fmt.Sprintf("A changeover to %s is open — use Cancel to stay on %s.",
			changeover.ToStyleName, changeover.FromStyleName)}
	default:
		if _, err := e.completeCutoverLocked(processID, changeover.ID, triggeredAdminSwitch, CutoverForced); err != nil {
			return err
		}
		target := changeover.ToStyleID
		return e.recordSwitch(processID, &target, *styleID, "admin", triggeredAdminSwitch, "")
	}
}

// adminSwitchNoChangeover is the switch with no changeover open: today's flip,
// and a completed record row when the style actually changes to a style. A
// clear (nil) or a re-set of the running style records nothing.
func (e *Engine) adminSwitchNoChangeover(proc *processes.Process, styleID *int64) error {
	if styleID == nil || sameStyle(proc.ActiveStyleID, styleID) {
		return e.flipActiveStyle(proc.ID, styleID, time.Time{})
	}
	var from *int64
	if proc.ActiveStyleID != nil {
		a := *proc.ActiveStyleID
		from = &a
	}
	return e.recordSwitch(proc.ID, from, *styleID, "admin", triggeredAdminSwitch, "")
}

// recordSwitch flips the process to toStyleID and records the switch as a
// completed changeover row from fromStyleID (no tasks, orders or episode)
// with the PLC's part as it lands (start_catid: a part standing through the
// switch is never suggested for it), then opens the post-cutover watch on it
// — replacing a watch the door opened for the changeover's own target — and
// releases the holds the landing style runs, as the door's landing does. A
// standing post-cutover flag on a changeover to the style it lands on is
// answered and cleared. The landing is noted on the CATID monitor first, as
// the door notes its own.
func (e *Engine) recordSwitch(processID int64, fromStyleID *int64, toStyleID int64, calledBy, triggeredBy, notes string) error {
	e.noteLanding(processID, fromStyleID)
	if err := e.flipActiveStyle(processID, &toStyleID, time.Time{}); err != nil {
		return err
	}
	id, err := e.db.InsertCompletedChangeover(processID, fromStyleID, toStyleID, calledBy, triggeredBy, notes)
	if err != nil {
		return fmt.Errorf("record the switch on process %d: %w", processID, err)
	}
	if v, ok := e.liveIdentity(processID); ok {
		if err := e.db.SetChangeoverStartCATID(id, v); err != nil {
			e.logFn("switch: record start CATID on changeover %d: %v", id, err)
		}
	}
	if co, err := e.db.LatestFlaggedChangeover(processID); err == nil && co != nil && co.ToStyleID == toStyleID {
		if err := e.ClearPostCutoverFlag(processID); err != nil {
			e.logFn("switch: clear the post-cutover flag on changeover %d: %v", co.ID, err)
		}
	}
	e.openPostCutoverVerify(processID, id)
	e.releaseHoldsOnLanding(processID)
	return nil
}
