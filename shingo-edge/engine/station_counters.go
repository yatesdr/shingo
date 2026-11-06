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

package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/store/counters"
)

// StationCounterRefused is a station-counter save or binding refused, with
// the sentence the page shows.
type StationCounterRefused struct{ Sentence string }

func (r *StationCounterRefused) Error() string { return r.Sentence }

func refused(format string, args ...any) error {
	return &StationCounterRefused{Sentence: fmt.Sprintf(format, args...)}
}

// ListStationCounters returns a process's station counters ([] for none).
func (e *Engine) ListStationCounters(processID int64) ([]counters.LineCounter, error) {
	list, err := e.db.ListLineCounters(processID)
	if list == nil && err == nil {
		list = []counters.LineCounter{}
	}
	return list, err
}

// CreateStationCounter adds a station counter reading tagName on the process's
// counter PLC. Refused for a process with no wired counter, a tag a reporting
// point or another station counter reads, and a tag that is not an integer
// (declared, or read whole when WarLink declares no type). The tag's
// publishing is enabled when WarLink is not already publishing it, and
// recorded as the Edge's; a refusal after that unpublishes it again. The row
// is unread and seeds from its first poll.
func (e *Engine) CreateStationCounter(processID int64, label, tagName string) (*counters.LineCounter, error) {
	proc, err := e.db.GetProcess(processID)
	if err != nil {
		return nil, err
	}
	tagName = strings.TrimSpace(tagName)
	if proc.CounterPLCName == "" || proc.CounterTagName == "" {
		return nil, refused("%s has no production counter wired; a station counter reads beside it", proc.Name)
	}
	if tagName == "" {
		return nil, refused("a station counter needs its tag")
	}
	if err := e.stationTagFree(proc.CounterPLCName, tagName, 0); err != nil {
		return nil, err
	}
	managed, err := e.publishStationTag(proc.CounterPLCName, tagName)
	if err != nil {
		return nil, err
	}
	id, err := e.db.CreateLineCounter(processID, strings.TrimSpace(label), proc.CounterPLCName, tagName)
	if err != nil {
		e.unpublishTag(managed, proc.CounterPLCName, tagName)
		return nil, err
	}
	if managed {
		if err := e.db.SetLineCounterManaged(id, true); err != nil {
			e.logFn("station counter %d: record managed publishing: %v", id, err)
		}
	}
	e.logFn("station counter %d on %s: %s/%s created (%q), unread — it seeds at its first poll", id, proc.Name,
		proc.CounterPLCName, tagName, label)
	return e.db.GetLineCounter(id)
}

// UpdateStationCounter renames a station counter or moves it to another tag:
// the new tag is checked and published as on create, the old one unpublished
// when the Edge published it, and the row re-seeds from the new tag.
func (e *Engine) UpdateStationCounter(id int64, label, tagName string) error {
	lc, err := e.db.GetLineCounter(id)
	if err != nil {
		return err
	}
	if lc == nil {
		return fmt.Errorf("no station counter %d", id)
	}
	label, tagName = strings.TrimSpace(label), strings.TrimSpace(tagName)
	if tagName == "" || tagName == lc.TagName {
		return e.db.RenameLineCounter(id, label)
	}
	if err := e.stationTagFree(lc.PLCName, tagName, id); err != nil {
		return err
	}
	managed, err := e.publishStationTag(lc.PLCName, tagName)
	if err != nil {
		return err
	}
	if err := e.db.RepointLineCounter(id, label, tagName, managed); err != nil {
		e.unpublishTag(managed, lc.PLCName, tagName)
		return err
	}
	e.unpublishTag(lc.WarlinkManaged, lc.PLCName, lc.TagName)
	e.logFn("station counter %d: %s/%s → %s/%s, unread — it seeds from the new tag's first poll", id,
		lc.PLCName, lc.TagName, lc.PLCName, tagName)
	return nil
}

// DeleteStationCounter deletes a station counter: its positions go back to the
// production counter, and its tag is unpublished when the Edge published it.
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
	e.logFn("station counter %d (%s/%s) deleted: its positions are counted by the production counter again", id, lc.PLCName, lc.TagName)
	return nil
}

// SetCountedAt binds a position to a station counter (nil: back to the
// production counter). Refused for a counter of another process, and for a
// position that a loader, a produce claim or an A/B pair holds under any live
// style, or that no live style claims as consume. The move is logged: the
// parts already in the line between the cart and the counter change counter.
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
		to = fmt.Sprintf("station counter %d (%s)", lc.ID, lc.TagName)
	}
	from := "the production counter"
	if node.LineCounterID != nil {
		from = fmt.Sprintf("station counter %d", *node.LineCounterID)
	}
	if err := e.db.SetNodeLineCounter(nodeID, counterID); err != nil {
		return err
	}
	e.logFn("counted at: %s moves from %s to %s — the parts already in the line between its cart and the counter move counter with it",
		node.Name, from, to)
	return nil
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

// stationTagFree refuses a tag a reporting point reads, or a station counter
// other than exceptID.
func (e *Engine) stationTagFree(plcName, tagName string, exceptID int64) error {
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
