package engine

import (
	"errors"
	"fmt"

	"shingo/protocol/clock"
	"shingoedge/domain"
	"shingoedge/domain/flowspec"
	"shingoedge/store/processes"
)

// auto_reorder_switch.go — a person's switch for a line's auto-reorder: off is
// a person's off at every position of the line (domain.ReorderHoldPersonOff),
// which only this switch clears; on clears it and is a go. Flipped from
// Processes, and from the station board where an engineer has allowed it
// there. Every flip is recorded (auto_reorder_flips).
//
// AUTO-REORDER NEEDS THE COUNTER THE IDLE RULE READS (owner, 2026-11-13: "not
// able to config until that's done and connected"). Turning it on is refused
// on a line with no counter configured, and on one whose counter has not read
// since this Edge started or has failed since. Turning it off is never
// refused: stopping the automatic asks needs no counter.

// ErrAutoReorderNoCounter refuses turning auto-reorder on, at the switch or
// on a claim in Processes, for a line with no counter configured.
var ErrAutoReorderNoCounter = errors.New("auto-reorder needs this line's production counter: set its PLC and tag, and enable it, in Settings first")

// ErrAutoReorderSwitchHidden refuses a flip from a station whose line does not
// show the switch.
var ErrAutoReorderSwitchHidden = errors.New("this line's auto-reorder switch is not shown on the station; it is flipped in Processes")

// FlipAutoReorder flips a line's auto-reorder: on or off at every position,
// by who, from source (domain.FlipFromProcesses or FlipFromStation).
func (e *Engine) FlipAutoReorder(processID int64, on bool, by, source string) error {
	p, err := e.db.GetProcess(processID)
	if err != nil || p == nil {
		return fmt.Errorf("process %d: not found", processID)
	}
	if source == domain.FlipFromStation && !p.AutoReorderSwitchHMI {
		return ErrAutoReorderSwitchHidden
	}
	if on {
		if !counterWired(p) {
			return ErrAutoReorderNoCounter
		}
		if !e.stationHealth.readSinceBoot(p.CounterPLCName, p.CounterTagName) {
			return fmt.Errorf("production counter %s cannot be read now; auto-reorder stays off until it reads", p.CounterTagName)
		}
	}
	n, err := processes.FlipAutoReorder(e.db.DB, processID, on, by, source, clock.Now())
	if err != nil {
		return err
	}
	word := "off"
	if on {
		word = "on"
	}
	e.logFn("auto-reorder: process %s switched %s by %s from %s (%d position(s))", p.Name, word, by, source, n)
	e.nudgeBoards()
	return nil
}

// armingFindings refuses a save that turns a claim's auto-reorder on in a line
// with no counter configured (ErrAutoReorderNoCounter), one finding per cell.
// Only the turn: a claim already armed saves as it is. A loader's claim is not
// asked for by the sweep, so its flag arms nothing and is not refused.
func armingFindings(process *processes.Process, draft flowDraft) []domain.NodeFinding {
	if counterWired(process) {
		return nil
	}
	var out []domain.NodeFinding
	for _, c := range draft.cells {
		if !armsAutoReorder(c) || c.claim.IsLoaderNode() {
			continue
		}
		out = append(out, domain.NodeFinding{
			CoreNodeName: c.cell.CoreNodeName, Side: flowspec.SideTo, Field: flowspec.AutoReorder,
			Severity: domain.SeverityError, Message: ErrAutoReorderNoCounter.Error(),
		})
	}
	return out
}

// armsAutoReorder reports whether a saved cell turns its claim's auto-reorder
// on: on in the draft, off or absent before.
func armsAutoReorder(c flowDraftCell) bool {
	return c.claim.AutoReorder && (c.prior == nil || !c.prior.AutoReorder)
}
