package engine

import (
	"sort"

	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_surplus.go — the arm: the keeper re-typing a cart of its own family.
//
// A TYPE IS SHORT INSIDE A WHOLE FAMILY. Short of the type, net of asks, comings and what the
// walk has lit at the windows, and a same-family cart standing in the kanban above its want:
// the oldest arrival of the type with the largest surplus goes to a free window of the
// kanban's own unloader. The walk lights it as the short type, CLEAR re-types it, and its
// send-out lands as that type. The keeper keeps the loop whole; this keeps the mix.
//
// NEVER THE LAST FREE WINDOW, so the process's fulls have somewhere to land
// (dispatch.SurplusToWindows). NEVER A WINDOW THE OPEN CHANGEOVER'S EVACS STILL NEED: a line cart
// standing past its type's cover is on its way to a window, and at a START the arm runs before
// the line's legs exist. With every free window spoken for it still sends one, if none of its
// carts is out: a change that needs a re-type before its first swap would otherwise wait on the
// very windows that wait on it.
//
// ONE COUNT OF ONE GAP. A cart the arm moved reads as what the walk lights it, so the keeper and
// the arm do not both fill one gap. Idle with the mix whole there is no shortfall, no read and
// no move.
//
// WHAT IT READS. Its precondition is the tick's reads. Firing adds the windows, the surplus
// list and the move. Pin: TestKanbanCost_SurplusArm.
//
// NO MEMORY. The row's prefix is all a restart needs: the keeper reads its moves back from the
// orders, and the scanner and the fleet drive them as they do any other. Pin:
// TestKanbanSurplus_ARestartMidMoveIsNotStranded.

// surplusShort is a type short inside a family, and how many more it needs.
type surplusShort struct {
	code   string
	family int64
	need   int
}

// surplusLine is a type above its want, and by how many.
type surplusLine struct {
	code   string
	family int64
	above  int
}

// planSurplus decides for each short type, largest need first, cart by cart: the type of the
// same family with the largest surplus. Ties take the lines' order. It reads nothing. The
// answer is one code a cart to move, in order.
func planSurplus(shorts []surplusShort, above []surplusLine) []string {
	left := append([]surplusLine(nil), above...)
	order := append([]surplusShort(nil), shorts...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].need > order[j].need })
	var out []string
	for _, s := range order {
		for i := 0; i < s.need; i++ {
			var best *surplusLine
			for j := range left {
				if left[j].above > 0 && kanban.SameFamily(left[j].family, s.family) && (best == nil || left[j].above > best.above) {
					best = &left[j]
				}
			}
			if best == nil {
				break
			}
			best.above--
			out = append(out, best.code)
		}
	}
	return out
}

// evacsOnTheLine is the line carts standing past their type's cover with no order yet: what the
// open changeover still has to bring to a window.
func evacsOnTheLine(lines []keeperLine, rows []store.KanbanPictureRow) int {
	n := 0
	for i, l := range lines {
		n += max(0, l.count.Held-rows[i].Cover)
	}
	return n
}

// surplusArm runs the arm on a kanban the tick found short of a type, after its asks, and
// answers the causes by surplus type. Nothing above its want, or no short type with a family
// member above: nothing is read and nothing moved.
func (m *KanbanKeeper) surplusArm(k store.KanbanPictureRow, rows []store.KanbanPictureRow, lines []keeperLine) map[string]string {
	var above []surplusLine
	var shorts []surplusShort
	for i, l := range lines {
		if n := l.count.Resident - l.count.Level.Want; n > 0 {
			above = append(above, surplusLine{code: rows[i].BinTypeCode, family: rows[i].FamilyID, above: n})
		}
		if need := l.count.Short() - l.atWindows - l.state.Created; need > 0 {
			shorts = append(shorts, surplusShort{code: rows[i].BinTypeCode, family: rows[i].FamilyID, need: need})
		}
	}
	codes := planSurplus(shorts, above)
	if len(codes) == 0 {
		return nil
	}
	causes := map[string]string{}
	windows, err := m.eng.db.ListKanbanWindows(k.KanbanNodeID)
	if err != nil {
		m.eng.logFn("keeper: %s: read its windows: %v", k.KanbanName, err)
		return causes
	}
	carts, err := m.eng.db.ListKanbanSurplus(k.KanbanNodeID, distinct(codes))
	if err != nil {
		m.eng.logFn("keeper: %s: read its surplus: %v", k.KanbanName, err)
		return causes
	}
	m.surplusToWindows(k, oldestOf(codes, carts, map[int64]bool{}), windows, evacsOnTheLine(lines, rows), causes)
	return causes
}

// surplusToWindows sends the carts to the windows of the kanban's unloaders, loader by loader,
// each under its own lock. Fewer than two windows standing open is what the tick's read already
// says, with none of freeWindows' statements.
func (m *KanbanKeeper) surplusToWindows(k store.KanbanPictureRow, queue []*bins.Bin, windows []store.KanbanWindow, evacs int,
	causes map[string]string) {
	if len(queue) == 0 {
		return
	}
	why := "above its want, and no unloader sends back to " + k.KanbanName
	byLoader := map[int64][]*nodes.Node{}
	var order []int64
	seen := map[int64]bool{}
	armOut := false
	for _, w := range windows {
		first := !seen[w.LoaderID]
		seen[w.LoaderID] = true
		armOut = armOut || w.Surplus
		if !w.Shared || (w.Funnel && !first) {
			continue
		}
		if first {
			order = append(order, w.LoaderID)
		}
		if !w.Row.Occupied && w.Node.Enabled {
			byLoader[w.LoaderID] = append(byLoader[w.LoaderID], w.Node)
		}
	}
	for _, id := range order {
		if len(queue) == 0 {
			break
		}
		why = "above its want, waiting on a free window: the unloader keeps its last one free"
		open := byLoader[id]
		if len(open) < 2 {
			continue
		}
		reserve := evacs
		if !armOut {
			reserve = min(reserve, len(open)-2)
		}
		moved, free, err := m.eng.dispatcher.SurplusToWindows(id, open, queue, "surplus of "+k.KanbanName, reserve)
		if err != nil {
			m.eng.logFn("keeper: %s: surplus to loader %d: %v", k.KanbanName, id, err)
		}
		evacs = max(0, evacs-max(0, free-1))
		if free >= 2 && len(moved) == 0 {
			why = "above its want, and its windows are for the carts coming off the line"
		}
		went := make(map[int64]bool, len(moved))
		for _, c := range moved {
			went[c] = true
			armOut = true
			m.eng.logFn("keeper: SURPLUS bin=%d of %s to a window of loader %d", c, k.KanbanName, id)
		}
		var rest []*bins.Bin
		for _, c := range queue {
			if !went[c.ID] {
				rest = append(rest, c)
			}
		}
		queue = rest
	}
	for _, c := range queue {
		causes[c.BinTypeCode] = why
	}
}

// oldestOf is, for each entry of codes in order, the oldest cart of that type not yet
// handed out, noting it in handed. carts is oldest first. A type short of carts (one was
// claimed between the count and the list) simply has fewer: the next tick reads again.
func oldestOf(codes []string, carts []*bins.Bin, handed map[int64]bool) []*bins.Bin {
	var out []*bins.Bin
	for _, code := range codes {
		for _, c := range carts {
			if c.BinTypeCode == code && !handed[c.ID] {
				handed[c.ID] = true
				out = append(out, c)
				break
			}
		}
	}
	return out
}

func distinct(list []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range list {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}
