//go:build sim

package engine

import (
	"regexp"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/release"
	"shingoedge/store/processes"
)

// sim_changeover.go — the sim operator runs a scheduled changeover the way a
// person at the station does (sim.operators.changeovers). It presses START,
// each RELEASE the screen offers, and CUTOVER once the changeover's own gate
// says it can complete with no blockers, through the doors those buttons use:
// StartProcessChangeover (the station's START), the tile's RELEASE (runRelease,
// with the changeover's purposed release where the tile shows one), and
// CompleteProcessProductionCutover (CUTOVER). It reads what the screen reads
// and writes nothing itself. No production code knows any of this.
//
// A REFUSAL IS READ AS A PERSON READS IT. "A carrier is on its way" is waited
// out and START pressed again on the next sweep. "Order N is staged ... Release
// the wait" is done: the operator presses that order's RELEASE, then START
// again. Anything else names nothing to wait for or do from the screen (a
// faulted order needs maintenance); it is logged once and the changeover left.

type coState int

const (
	coWaiting coState = iota // not yet started
	coStarted                // START accepted; waiting for the gate
	coDone                   // cut over
	coLeft                   // refused with nothing to wait for, or ended by someone else
)

// changeoverRun is one scheduled changeover and where the operator is with it.
// Read and written by the reconcile goroutine only.
type changeoverRun struct {
	cfg     config.SimChangeover
	state   coState
	readyAt time.Time // the sweep that first read can_complete with no blockers
	said    string    // the last refusal logged, so each is logged once
}

func newChangeoverRuns(cfg []config.SimChangeover) []changeoverRun {
	runs := make([]changeoverRun, len(cfg))
	for i, c := range cfg {
		runs[i] = changeoverRun{cfg: c}
	}
	return runs
}

// driveChangeovers is one sweep of the scheduled changeovers, in order: a
// changeover for a process starts only once every earlier one for that process
// is done or left.
func (op *simOperator) driveChangeovers() {
	now := op.clk.Now()
	for i := range op.changeovers {
		r := &op.changeovers[i]
		switch r.state {
		case coWaiting:
			if now.Sub(op.startedAt) < r.cfg.At || op.earlierOpen(i) {
				continue
			}
			op.pressStart(r)
		case coStarted:
			op.pressCutoverWhenReady(r, now)
		}
	}
}

func (op *simOperator) earlierOpen(i int) bool {
	for j := 0; j < i; j++ {
		p := op.changeovers[j]
		if p.cfg.Process == op.changeovers[i].cfg.Process && (p.state == coWaiting || p.state == coStarted) {
			return true
		}
	}
	return false
}

// resolve finds the process and the style by name, as the station's picker
// lists them.
func (op *simOperator) resolve(c config.SimChangeover) (processID, styleID int64, ok bool) {
	procs, err := op.e.db.ListProcesses()
	if err != nil {
		return 0, 0, false
	}
	for _, p := range procs {
		if p.Name != c.Process {
			continue
		}
		styles, err := op.e.db.ListStylesByProcess(p.ID)
		if err != nil {
			return 0, 0, false
		}
		for _, s := range styles {
			if s.Name == c.ToStyle {
				return p.ID, s.ID, true
			}
		}
	}
	return 0, 0, false
}

var stagedBlocker = regexp.MustCompile(`order (\d+) is staged`)

func (op *simOperator) pressStart(r *changeoverRun) {
	pid, sid, ok := op.resolve(r.cfg)
	if !ok {
		op.leave(r, "no process "+r.cfg.Process+" with a style "+r.cfg.ToStyle)
		return
	}
	if co, err := op.e.db.GetActiveProcessChangeover(pid); err == nil && co != nil {
		if co.ToStyleID == sid {
			r.state = coStarted // already under way (a restart mid-changeover)
			return
		}
		op.say(r, "another changeover is active on "+r.cfg.Process+"; waiting")
		return
	}
	co, err := op.e.StartProcessChangeover(pid, sid, "sim-operator", "")
	if err == nil {
		r.state, r.said = coStarted, ""
		op.e.logFn("[sim] operator started changeover %d: %s to %s", co.ID, r.cfg.Process, r.cfg.ToStyle)
		return
	}
	msg := err.Error()
	staged := stagedBlocker.FindAllStringSubmatch(msg, -1)
	switch {
	case strings.Contains(msg, "a carrier is on its way") || len(staged) > 0:
		op.say(r, "START refused, waiting it out: "+msg)
		for _, m := range staged {
			if id, err := strconv.ParseInt(m[1], 10, 64); err == nil {
				op.pressReleaseNow(id)
			}
		}
	default:
		op.leave(r, "START refused with nothing to wait for: "+msg)
	}
}

// pressCutoverWhenReady presses CUTOVER once the gate has read can_complete with
// no blockers for the operator's reaction time.
func (op *simOperator) pressCutoverWhenReady(r *changeoverRun, now time.Time) {
	pid, sid, ok := op.resolve(r.cfg)
	if !ok {
		op.leave(r, "the process or style is gone")
		return
	}
	co, err := op.e.db.GetActiveProcessChangeover(pid)
	if err != nil || co == nil {
		if p, perr := op.e.db.GetProcess(pid); perr == nil && p.ActiveStyleID != nil && *p.ActiveStyleID == sid {
			r.state = coDone
			op.e.logFn("[sim] operator: changeover of %s to %s finished without it", r.cfg.Process, r.cfg.ToStyle)
			return
		}
		op.leave(r, "the changeover ended without a cutover (cancelled)")
		return
	}
	can, blockers, err := op.e.ChangeoverGateStatus(pid)
	if err != nil || !can || len(blockers) > 0 {
		r.readyAt = time.Time{}
		return
	}
	if r.readyAt.IsZero() {
		r.readyAt = now
		return
	}
	if now.Sub(r.readyAt) < op.swapReleaseDelay() {
		return
	}
	if err := op.e.CompleteProcessProductionCutover(pid); err != nil {
		r.readyAt = time.Time{}
		op.say(r, "CUTOVER refused, looking again: "+err.Error())
		return
	}
	r.state, r.said = coDone, ""
	op.e.logFn("[sim] operator cut changeover %d over: %s is on %s", co.ID, r.cfg.Process, r.cfg.ToStyle)
}

func (op *simOperator) say(r *changeoverRun, msg string) {
	if r.said != msg {
		r.said = msg
		op.e.logFn("[sim] operator, changeover of %s to %s: %s", r.cfg.Process, r.cfg.ToStyle, msg)
	}
}

func (op *simOperator) leave(r *changeoverRun, why string) {
	r.state = coLeft
	op.e.logFn("[sim] operator leaves the changeover of %s to %s: %s", r.cfg.Process, r.cfg.ToStyle, why)
}

// changeoverReleasePurpose is the changeover decision the node's tile RELEASE
// makes, as the station screen decides it (operator-modal.js
// changeoverReleasePurpose): with a changeover task at the node that is not
// "unchanged", the first purpose in the order a cell owes them, other than
// swap, at which a live leg at the node is parked at a station wait. "" when the
// tile shows the ordinary RELEASE.
func (op *simOperator) changeoverReleasePurpose(node *processes.Node) release.Purpose {
	co, err := op.e.db.GetActiveProcessChangeover(node.ProcessID)
	if err != nil || co == nil {
		return ""
	}
	task, err := op.e.db.GetChangeoverNodeTaskByNode(co.ID, node.ID)
	if err != nil || task == nil || task.Situation == "unchanged" {
		return ""
	}
	legs, err := op.e.db.ListActiveOrdersByProcessNode(node.ID)
	if err != nil {
		return ""
	}
	ready := map[release.Purpose]bool{}
	for i := range legs {
		o := &legs[i]
		if protocol.IsTerminal(o.Status) || o.ReleaseFacts == "" {
			continue
		}
		facts, err := release.DecodeFacts(o.ReleaseFacts)
		if err != nil || len(facts.Purposes) == 0 {
			continue
		}
		intent, _ := release.DecodeIntent(o.ReleaseIntent)
		pt := release.PointOf(o.Status, o.StationWait, o.WaitKind, intent, facts.Purposes)
		if pt.HasWait && pt.AtWait && !pt.CoreHeld {
			ready[pt.Purpose] = true
		}
	}
	for _, p := range release.PurposeOrder() {
		if p != release.PurposeSwap && ready[p] {
			return p
		}
	}
	return ""
}
