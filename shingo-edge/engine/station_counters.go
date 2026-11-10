// station_counters.go — the station counters' verbs: create, re-point, delete,
// and binding a position to one ("counted at"). The poll reads them
// (plc/line_counters.go) and the walk charges what they count
// (handleLineCounterDelta).
//
// A station counter is a tag on the cell's own PLC that clicks once per
// assembly where the part leaves the cart, under every style that uses the
// position. The rules below keep a binding to that: a consume position some
// live style claims, never a loader, a produce position or an A/B pair, and
// never a tag another counter reads.
//
// A station counter may read a second tag on the same PLC and counts the sum:
// a station with one fixture per hand, each with its own count, of which one
// runs at a time, so the sum is one load per assembly on whichever hand runs,
// a changeover's overlap included (TestLinePoll_TwoTagsCountTheirSum).

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/store/counters"
	"shingoedge/store/processes"
)

// StationCounterRefused is a station-counter save or binding refused, with
// the sentence the page shows.
type StationCounterRefused struct{ Sentence string }

func (r *StationCounterRefused) Error() string { return r.Sentence }

func refused(format string, args ...any) error {
	return &StationCounterRefused{Sentence: fmt.Sprintf(format, args...)}
}

// CreateStationCounter adds a station counter reading tagName, and tagName2
// when it is not blank, on the process's counter PLC; a counter with two tags
// counts their sum. Refused for a process with no wired counter, no first
// tag, the same tag twice, a tag a reporting point or another station counter
// reads, and a tag that is not an integer (declared, or read whole when
// WarLink declares no type). Each tag's publishing is enabled when WarLink is
// not already publishing it, and recorded as the Edge's; a refusal after that
// unpublishes it again. The row is unread and each tag seeds from its first
// poll.
func (e *Engine) CreateStationCounter(processID int64, label, tagName, tagName2 string) (*counters.LineCounter, error) {
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	tagName, tagName2 = strings.TrimSpace(tagName), strings.TrimSpace(tagName2)
	if proc.CounterPLCName == "" || proc.CounterTagName == "" {
		return nil, refused("%s has no production counter wired; a station counter reads beside it", proc.Name)
	}
	if tagName == "" {
		return nil, refused("a station counter needs its tag")
	}
	if err := e.stationTagsFree(proc, tagName, tagName2, 0); err != nil {
		return nil, err
	}
	plcName := proc.CounterPLCName
	managed, err := e.publishStationTag(plcName, tagName)
	if err != nil {
		return nil, err
	}
	managed2 := false
	if tagName2 != "" {
		if managed2, err = e.publishStationTag(plcName, tagName2); err != nil {
			e.unpublishTag(managed, plcName, tagName)
			return nil, err
		}
	}
	id, err := e.db.CreateLineCounter(processID, strings.TrimSpace(label), plcName, tagName, tagName2)
	if err != nil {
		e.unpublishTag(managed, plcName, tagName)
		e.unpublishTag(managed2, plcName, tagName2)
		return nil, err
	}
	for _, m := range []struct{ second, managed bool }{{false, managed}, {true, managed2}} {
		if !m.managed {
			continue
		}
		if err := e.db.SetLineCounterManaged(id, m.second, true); err != nil {
			e.logFn("station counter %d: record managed publishing: %v", id, err)
		}
	}
	e.stationHealth.startTrack(id, processID)
	e.logFn("station counter %d on %s: %s/%s created (%q), unread — each tag seeds at its first poll", id, proc.Name,
		plcName, tagsWords(tagName, tagName2), label)
	return e.db.GetLineCounter(id)
}

// UpdateStationCounter renames a station counter or moves it to other tags.
// A blank tagName keeps the first tag; tagName2 nil keeps the second, and a
// blank one removes it. Each new tag is checked and published as on create, an
// old tag the counter no longer reads is unpublished when the Edge published
// it (a tag that moves between the two keeps its publishing), and each tag
// that changed re-seeds from its first read; a tag that stays keeps counting.
func (e *Engine) UpdateStationCounter(id int64, label, tagName string, tagName2 *string) error {
	lc, err := e.db.GetLineCounter(id)
	if err != nil {
		return err
	}
	if lc == nil {
		return fmt.Errorf("no station counter %d", id)
	}
	label, tagName = strings.TrimSpace(label), strings.TrimSpace(tagName)
	if tagName == "" {
		tagName = lc.TagName
	}
	second := lc.TagName2
	if tagName2 != nil {
		second = strings.TrimSpace(*tagName2)
	}
	if tagName == lc.TagName && second == lc.TagName2 {
		return e.db.RenameLineCounter(id, label)
	}
	proc, err := e.db.GetProcess(lc.ProcessID)
	if err != nil {
		return err
	}
	if err := e.stationTagsFree(proc, tagName, second, id); err != nil {
		return err
	}
	// The publishing each old tag carries, so a tag the counter keeps (in
	// either place) keeps it, and only a tag new to the counter is published.
	held := map[string]bool{lc.TagName: lc.WarlinkManaged}
	if lc.TagName2 != "" {
		held[lc.TagName2] = lc.WarlinkManaged2
	}
	var published []string // new tags this save enabled, for the rollback
	publish := func(tag string) (bool, error) {
		if m, ok := held[tag]; ok {
			return m, nil
		}
		m, err := e.publishStationTag(lc.PLCName, tag)
		if err == nil && m {
			published = append(published, tag)
		}
		return m, err
	}
	undo := func() {
		for _, tag := range published {
			e.unpublishTag(true, lc.PLCName, tag)
		}
	}
	managed, err := publish(tagName)
	if err != nil {
		return err
	}
	managed2 := false
	if second != "" {
		if managed2, err = publish(second); err != nil {
			undo()
			return err
		}
	}
	if err := e.db.RepointLineCounter(id, label, tagName, second, managed, managed2); err != nil {
		undo()
		return err
	}
	for tag, m := range held {
		if tag != tagName && tag != second {
			e.unpublishTag(m, lc.PLCName, tag)
		}
	}
	e.stationHealth.startTrack(id, lc.ProcessID)
	e.logFn("station counter %d: %s/%s → %s/%s; a changed tag is unread — it seeds from its first poll", id,
		lc.PLCName, tagsWords(lc.TagName, lc.TagName2), lc.PLCName, tagsWords(tagName, second))
	return nil
}

// DeleteStationCounter deletes a station counter: its positions go back to the
// production counter, and each of its tags is unpublished when the Edge
// published it.
func (e *Engine) DeleteStationCounter(id int64) error {
	lc, err := e.db.GetLineCounter(id)
	if err != nil {
		return err
	}
	if lc == nil {
		return fmt.Errorf("no station counter %d", id)
	}
	if err := e.db.DeleteLineCounter(id); err != nil {
		return err
	}
	e.unpublishTag(lc.WarlinkManaged, lc.PLCName, lc.TagName)
	if lc.TagName2 != "" {
		e.unpublishTag(lc.WarlinkManaged2, lc.PLCName, lc.TagName2)
	}
	e.logFn("station counter %d (%s/%s) deleted: its positions are counted by the production counter again", id,
		lc.PLCName, tagsWords(lc.TagName, lc.TagName2))
	return nil
}

// tagsWords is a counter's tags as the log and the page say them: one, or
// both joined with " + " (the counter counts their sum).
func tagsWords(tagName, tagName2 string) string {
	if tagName2 == "" {
		return tagName
	}
	return tagName + " + " + tagName2
}

// stationCounterName is a counter as the strip names it: its label, else its
// tags.
func stationCounterName(lc counters.LineCounter) string {
	if lc.Label != "" {
		return lc.Label
	}
	return tagsWords(lc.TagName, lc.TagName2)
}

// SetCountedAt binds a position to a station counter (nil: back to the
// production counter). Refused for a counter of another process, and for a
// position that a loader, a produce claim or an A/B pair holds under any live
// style, or that no live style claims as consume. The move is logged with the
// parts in the line at the station counter it binds to or leaves (its card's
// number, ≈ and measured): they change counter, and net against the cart's
// run-out (TestCountedAt_BindMidRunNetsWithinTheLine).
func (e *Engine) SetCountedAt(nodeID int64, counterID *int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("no position %d", nodeID)
	}
	to := "the production counter"
	if counterID != nil {
		lc, err := e.db.GetLineCounter(*counterID)
		if err != nil {
			return err
		}
		if lc == nil || lc.ProcessID != node.ProcessID {
			return refused("station counter %d is not a counter of %s's process", *counterID, node.Name)
		}
		if err := e.countableAtAStation(node.ProcessID, node.CoreNodeName, node.Name); err != nil {
			return err
		}
		to = fmt.Sprintf("station counter %d (%s)", lc.ID, tagsWords(lc.TagName, lc.TagName2))
	}
	from := "the production counter"
	if node.LineCounterID != nil {
		from = fmt.Sprintf("station counter %d", *node.LineCounterID)
	}
	if err := e.db.SetNodeLineCounter(nodeID, counterID); err != nil {
		return err
	}
	e.logFn("counted at: %s moves from %s to %s", node.Name, from, to)
	return nil
}

// StationCountable applies the binding's rules to a position without binding
// it: nil when a station counter may count it, else its refusal
// (*StationCounterRefused). The station card's dry run: it reads, never
// writes.
func (e *Engine) StationCountable(nodeID int64) error {
	node, err := e.db.GetProcessNode(nodeID)
	if err != nil {
		return err
	}
	if node == nil {
		return fmt.Errorf("no position %d", nodeID)
	}
	return e.countableAtAStation(node.ProcessID, node.CoreNodeName, node.Name)
}

// countableAtAStation applies the binding's rules to every live style's claim
// at the position, not only the one the active style resolves: a binding that
// holds today must hold under the style the process changes to next.
func (e *Engine) countableAtAStation(processID int64, coreNode, name string) error {
	styles, err := e.db.ListStylesByProcess(processID)
	if err != nil {
		return err
	}
	consume := 0
	for _, s := range styles {
		c, err := e.db.GetStyleNodeClaimByNode(s.ID, coreNode)
		if err != nil || c == nil {
			continue
		}
		switch {
		case c.IsLoaderNode():
			return refused("%s is a loader under %s; a loader is counted by its loads", name, s.Name)
		case c.Role == protocol.ClaimRoleProduce:
			return refused("%s produces under %s; a station counter counts parts leaving a cart", name, s.Name)
		case c.PairedCoreNode != "" && !c.IsPressIndex():
			return refused("%s is half of an A/B pair under %s; pairs are counted by the production counter", name, s.Name)
		case c.Role == protocol.ClaimRoleConsume:
			consume++
		}
	}
	if consume == 0 {
		return refused("no live style consumes at %s", name)
	}
	return nil
}

// stationTagsFree applies stationTagFree to each of a counter's tags
// (tagName2 "" for none), and refuses the same tag twice (Logix names are
// case-insensitive): one fixture read twice is two clicks a part.
func (e *Engine) stationTagsFree(proc *processes.Process, tagName, tagName2 string, exceptID int64) error {
	if tagName2 != "" && strings.EqualFold(tagName, tagName2) {
		return refused("%s is already the counter's first tag; a second tag is the other fixture's count", tagName2)
	}
	for _, tag := range []string{tagName, tagName2} {
		if tag == "" {
			continue
		}
		if err := e.stationTagFree(proc, tag, exceptID); err != nil {
			return err
		}
	}
	return nil
}

// stationTagFree refuses, on the process's counter PLC, a member of the
// process's own MES struct (its counter slots and their identities: a sibling
// slot would count the cell's strokes again; Logix names are
// case-insensitive), a tag a reporting point reads, or a station counter other
// than exceptID.
func (e *Engine) stationTagFree(proc *processes.Process, tagName string, exceptID int64) error {
	plcName := proc.CounterPLCName
	if mes := deriveProcessTagPrefix(proc.CounterTagName); mes != "" &&
		strings.HasPrefix(strings.ToLower(tagName), strings.ToLower(mes)+".") {
		return refused("%s is a member of %s, %s's MES struct; a station counter reads the station's own count", tagName, mes, proc.Name)
	}
	if on, err := e.db.ReportingPointOnTag(plcName, tagName); err != nil {
		return err
	} else if on {
		return refused("%s/%s is read by a production counter", plcName, tagName)
	}
	if lc, err := e.db.LineCounterByTag(plcName, tagName); err != nil {
		return err
	} else if lc != nil && lc.ID != exceptID {
		return refused("%s/%s is already station counter %d", plcName, tagName, lc.ID)
	}
	return nil
}

// publishStationTag checks the tag reads an integer and leaves it published,
// reporting whether the Edge enabled the publishing (WarLink was not already
// publishing it). A refused check has taken back what it enabled.
func (e *Engine) publishStationTag(plcName, tagName string) (managed bool, err error) {
	if e.plcMgr == nil {
		return false, nil
	}
	already := e.plcMgr.IsTagPublished(plcName, tagName)
	if err := e.checkCounterTagType(plcName, tagName); err != nil {
		return false, &StationCounterRefused{Sentence: err.Error()}
	}
	if already {
		return false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.plcMgr.EnableTagPublishing(ctx, plcName, tagName); err != nil {
		return false, refused("WarLink would not publish %s/%s: %v", plcName, tagName, err)
	}
	return true, nil
}

// unpublishTag disables a counter tag's publishing when the Edge enabled it.
func (e *Engine) unpublishTag(managed bool, plcName, tagName string) {
	if !managed || e.plcMgr == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := e.plcMgr.DisableTagPublishing(ctx, plcName, tagName); err != nil {
		e.logFn("counter tag %s/%s: unpublish: %v", plcName, tagName, err)
	}
}

// errStationTagRead is the process-counter save's refusal of a tag a station
// counter reads.
var errStationTagRead = errors.New("a station counter reads this tag")
