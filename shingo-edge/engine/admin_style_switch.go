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
		return e.flipActiveStyle(proc.ID, styleID)
	}
	var from *int64
	if proc.ActiveStyleID != nil {
		a := *proc.ActiveStyleID
		from = &a
	}
	return e.recordSwitch(proc.ID, from, *styleID, "admin", triggeredAdminSwitch, "")
}

// recordSwitch flips the process to toStyleID and records the switch as a
// completed changeover row from fromStyleID (no tasks, orders or episode),
// then opens the post-cutover watch on it — replacing a watch the door opened
// for the changeover's own target — and releases the holds the landing style
// runs, as the door's landing does. The landing is noted on the CATID
// monitor first, as the door notes its own. The admin switch and the
// automatic switch.
func (e *Engine) recordSwitch(processID int64, fromStyleID *int64, toStyleID int64, calledBy, triggeredBy, notes string) error {
	e.noteLanding(processID, fromStyleID)
	if err := e.flipActiveStyle(processID, &toStyleID); err != nil {
		return err
	}
	id, err := e.db.InsertCompletedChangeover(processID, fromStyleID, toStyleID, calledBy, triggeredBy, notes)
	if err != nil {
		return fmt.Errorf("record the switch on process %d: %w", processID, err)
	}
	e.openPostCutoverVerify(processID, id)
	e.releaseHoldsOnLanding(processID, id)
	return nil
}

// autoSwitchStyle is the automatic switch (N6): the process counted a part
// only toStyleID runs, by a person's pin, with no changeover open. It switches
// as the admin switch records one — the flip (piles stranded, plant claims
// published, the reporting point synced, every node pointed at the new
// style's claims), a completed auto-catid-flip row, the post-cutover watch,
// the holds the new style runs released — and clears the post-cutover flag the
// part resolves. It never plans a changeover: within a minute the sweep
// reorders and the keeper stages for the new style, and the request guard
// admits its requests. Under the door's mutex, re-read: a changeover opened or
// a style moved since the decision is a no-op.
func (e *Engine) autoSwitchStyle(processID, fromStyleID, toStyleID int64, catid string) error {
	e.cutoverMu.Lock()
	defer e.cutoverMu.Unlock()
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return err
	}
	if proc.ActiveStyleID == nil || *proc.ActiveStyleID != fromStyleID {
		e.logFn("plc-catid: process %d moved off style %d since the decision — not switching", processID, fromStyleID)
		return nil
	}
	if co, err := e.db.GetActiveProcessChangeover(processID); err == nil && co != nil {
		e.logFn("plc-catid: process %d: changeover %d opened since the decision — not switching", processID, co.ID)
		return nil
	} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	from, _ := e.db.GetStyle(fromStyleID)
	to, _ := e.db.GetStyle(toStyleID)
	notes := fmt.Sprintf("CATID %s counted, which only %s runs: switched from %s with no changeover. Piles stranded and "+
		"plant claims published; within a minute the sweep reorders and the keeper stages for %s, and the request "+
		"guard admits its requests.", catid, styleNameOr(to, toStyleID), styleNameOr(from, fromStyleID), styleNameOr(to, toStyleID))
	if err := e.ClearPostCutoverFlag(processID); err != nil {
		e.logFn("plc-catid: process %d: clear the post-cutover flag: %v", processID, err)
	}
	src := fromStyleID
	if err := e.recordSwitch(processID, &src, toStyleID, "auto", triggeredAutoCATIDFlip, notes); err != nil {
		return err
	}
	e.logFn("plc-catid: process %s: %s", proc.Name, notes)
	return nil
}

func styleNameOr(s *processes.Style, id int64) string {
	if s != nil && s.Name != "" {
		return s.Name
	}
	return fmt.Sprintf("style %d", id)
}
