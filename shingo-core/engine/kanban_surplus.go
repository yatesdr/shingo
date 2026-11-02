package engine

import (
	"errors"
	"sort"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/kanban"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// kanban_surplus.go — the keeper moving a resident out of its own kanban: a type is short,
// no child is free for it, and another type stands above its want.
//
//	SHARED FAMILY. The oldest resident of the type with the largest surplus on the short
//	type's floor goes to a free window of the unloader whose outbound is the kanban. The
//	walk lights it as the short type (unloader_lines.go), CLEAR re-types it, and its
//	send-out comes back as that type, counted as coming. Never the last free window, at
//	most (free - 1) a tick, so the process's fulls have somewhere to land
//	(dispatch.SurplusToWindows).
//
//	ANOTHER FLOOR, OR A TYPE WITH NO FLOOR ENTERED. Re-typing cannot help, so the oldest
//	surplus goes to the first source of the kanban declaring a level of its own type with
//	room: residents and comings under that level, no supports, and a free slot. None: it
//	waits, and its line says why (KanbanLineState.SurplusCause).
//
// A PLAIN GROUP AND THE OVERFLOW ARE NEVER ASKED, here. A plain group has no level, so
// no count says it wants the cart, and only a door or a keeper's source reaches it. A
// source that has supports is another process's. Pin:
// TestKanbanSurplus_NeverToAPlainGroupOrTheOverflow.
//
// ONE COUNT OF ONE GAP. A cart the arm moved stands at a window the walk has lit as S:
// the keeper's asks for S leave it out, so asks and re-types do not both fill one gap
// (surplusAtWindows, tickLine). The walk is bounded by the same shortfall, so what is
// left out is never more than the gap. Pin: TestKanbanSurplus_TheAsksAreNetted.
//
// WHAT IT READS. The precondition's numbers are the tick's four batched reads, and its
// exact "no child is free" is the ask's own read of the group, which only a short kanban
// with a surplus pays when no ask paid it first. Firing adds the surplus list and the
// move. Pin: TestKanbanCost_SurplusArm.
//
// NO MEMORY. The row's prefix is all a restart needs: the keeper reads its moves back
// from the orders, and the scanner and the fleet drive them as they do any other. Pin:
// TestKanbanSurplus_ARestartMidMoveIsNotStranded.

// surplusShort is a type short with no child free for it, and how many more it needs.
type surplusShort struct {
	code  string
	floor domain.Footprint
	sized bool
	need  int
}

// surplusLine is a type above its want, and by how many.
type surplusLine struct {
	code  string
	floor domain.Footprint
	sized bool
	above int
}

// surplusPlan is which types the tick moves, one entry a cart, in order, and which have
// a cart that needs a source with room and has none.
type surplusPlan struct {
	toWindows []string
	toSources []string
	noRoom    []string
}

// planSurplus decides for each short type, largest need first, cart by cart: the type
// with the largest surplus on its floor (never age alone: age only picks among carts of
// that type), and with none there, the largest surplus anywhere with a source's room. A
// type with no floor entered shares a floor with nothing. Ties take the lines' order. It
// reads nothing.
func planSurplus(shorts []surplusShort, above []surplusLine, room map[string]int) surplusPlan {
	left := append([]surplusLine(nil), above...)
	spare := make(map[string]int, len(room))
	for k, v := range room {
		spare[k] = v
	}
	order := append([]surplusShort(nil), shorts...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].need > order[j].need })
	largest := func(ok func(surplusLine) bool) *surplusLine {
		var best *surplusLine
		for i := range left {
			if left[i].above > 0 && ok(left[i]) && (best == nil || left[i].above > best.above) {
				best = &left[i]
			}
		}
		return best
	}
	var p surplusPlan
	noRoom := map[string]bool{}
	for _, s := range order {
		for i := 0; i < s.need; i++ {
			if x := largest(func(l surplusLine) bool { return s.sized && l.sized && l.floor == s.floor }); x != nil {
				x.above--
				p.toWindows = append(p.toWindows, x.code)
				continue
			}
			x := largest(func(l surplusLine) bool { return spare[l.code] > 0 })
			if x == nil {
				for _, l := range left {
					if l.above > 0 && !noRoom[l.code] {
						noRoom[l.code] = true
						p.noRoom = append(p.noRoom, l.code)
					}
				}
				break
			}
			x.above--
			spare[x.code]--
			p.toSources = append(p.toSources, x.code)
		}
	}
	return p
}

// surplusAtWindows is what the walk asks of the arm's carts, by the type each is lit as:
// the walk's own (kanban.Assign, the kanban's pass, which runs first whatever the sources
// say), over the same counts the keeper is about to ask on. A cart on its way reads as
// standing there. None of the arm's carts: no walk.
func surplusAtWindows(rows []store.KanbanPictureRow, lines []keeperLine, windows []store.KanbanWindow) map[string]int {
	some := false
	for _, w := range windows {
		some = some || w.Surplus
	}
	if !some {
		return nil
	}
	typeLines := make([]kanban.Line, len(lines))
	sizes := map[string]domain.Footprint{}
	inbound := map[int64]bool{}
	for i, l := range lines {
		lv := l.count.Level
		typeLines[i] = kanban.LineOf(lv.BinTypeCode, lv.Want, l.count.Have)
		if rows[i].Sized {
			sizes[rows[i].BinTypeCode] = rows[i].Floor
		}
		for _, ib := range l.count.Inbound {
			inbound[ib.BinID] = true
		}
	}
	carts := make([]protocol.NodeBinsRow, len(windows))
	mine := map[string]bool{}
	for i, w := range windows {
		carts[i] = w.Row
		if w.Sized {
			sizes[w.Row.BinTypeCode] = w.Floor
		}
		if w.Surplus {
			mine[w.Row.Node] = true
		}
	}
	out := map[string]int{}
	for _, a := range kanban.Assign(typeLines, carts, inbound, sizes) {
		if mine[a.Node] {
			out[a.BinType]++
		}
	}
	return out
}

// surplusArm runs the arm on a kanban the tick found short, after its asks, and answers
// the causes by surplus type. Nothing above its want, or a child free for every short
// type: nothing is moved, and with nothing above its want nothing is read.
func (m *KanbanKeeper) surplusArm(k store.KanbanPictureRow, rows []store.KanbanPictureRow, lines []keeperLine,
	ask *kanbanAsks, windows []store.KanbanWindow, picture map[int64][]store.KanbanPictureRow,
	counts *store.KanbanCounts) map[string]string {

	var above []surplusLine
	for i, l := range lines {
		if n := l.count.Resident - l.count.Level.Want; n > 0 {
			above = append(above, surplusLine{code: rows[i].BinTypeCode, floor: rows[i].Floor, sized: rows[i].Sized, above: n})
		}
	}
	if len(above) == 0 {
		return nil
	}
	var shorts []surplusShort
	for i := range lines {
		l := &lines[i]
		need := l.count.Short() - l.atWindows - l.state.Created
		if need > 0 && !m.childFreeFor(ask, l.count.Level.BinTypeID) {
			shorts = append(shorts, surplusShort{code: rows[i].BinTypeCode, floor: rows[i].Floor, sized: rows[i].Sized, need: need})
		}
	}
	if len(shorts) == 0 {
		return nil
	}
	room := map[string]int{}
	for _, a := range above {
		for _, s := range sourcesWithRoom(k, a.code, picture, counts) {
			room[a.code] += s.room
		}
	}
	p := planSurplus(shorts, above, room)
	causes := map[string]string{}
	for _, code := range p.noRoom {
		causes[code] = noSourceHasRoom(k.KanbanName)
	}
	if len(p.toSources) > 0 {
		p.toSources = m.lessLeaving(k, p.toSources, windows)
	}
	if len(p.toWindows)+len(p.toSources) == 0 {
		return causes
	}
	carts, err := m.eng.db.ListKanbanSurplus(k.KanbanNodeID, distinct(p.toWindows, p.toSources))
	if err != nil {
		m.eng.logFn("keeper: %s: read its surplus: %v", k.KanbanName, err)
		return causes
	}
	handed := map[int64]bool{}
	m.surplusToWindows(k, oldestOf(p.toWindows, carts, handed), windows, causes)
	m.surplusToSources(k, oldestOf(p.toSources, carts, handed), picture, counts, causes)
	return causes
}

func noSourceHasRoom(kanban string) string {
	return "above its want, and no source of " + kanban + " has room for it"
}

// childFreeFor asks the keeper's own slot chooser, through the tick's one view of the
// group, whether a child would take this type. Exact where the counts are not: it sees
// a loaded cart, a claimed one, a locked slot and an ask's hold. A failed read says a
// child is free: the arm moves nothing on a count that was not made.
func (m *KanbanKeeper) childFreeFor(ask *kanbanAsks, binTypeID int64) bool {
	res, err := m.eng.dispatcher.PlanKanbanSlot(ask.reads, ask.group, binTypeID)
	if err != nil {
		if plan.CapacityShape(err) == plan.CapacitySlot {
			return false
		}
		m.eng.logFn("keeper: %s: is a child free: %v", ask.group.Name, err)
		return true
	}
	return res != nil && res.Node != nil
}

// sourceRoom is one source of a kanban with room for a type.
type sourceRoom struct {
	group *nodes.Node
	room  int
}

// sourcesWithRoom is the kanban's sources, in rank order, that may take a cart of the
// type: live, with no supports, declaring a level of it, and short by residents and
// comings under it. From the tick's batched reads, so no statement. A source's free slot
// is asked when a cart goes (PlanGroupStoreSlot).
func sourcesWithRoom(k store.KanbanPictureRow, code string, picture map[int64][]store.KanbanPictureRow,
	counts *store.KanbanCounts) []sourceRoom {
	var out []sourceRoom
	for _, ref := range k.Sources {
		for _, r := range picture[ref.ID] {
			if r.BinTypeCode != code || !r.Live || len(r.Supports) > 0 {
				continue
			}
			if short := counts.Level(r.KanbanLevel(), 0).Short(); short > 0 {
				out = append(out, sourceRoom{room: short,
					group: &nodes.Node{ID: r.KanbanNodeID, Name: r.KanbanName, IsSynthetic: true, Enabled: true}})
			}
		}
	}
	return out
}

// lessLeaving takes off the plan the carts the arm already has on their way to a source:
// each frees a child when it is lifted, and the short type still reads short only because
// a robot has not got there. A move to a window is not one: its cart is netted as what it
// is lit as. One statement, only when a cart would go to a source.
func (m *KanbanKeeper) lessLeaving(k store.KanbanPictureRow, codes []string, windows []store.KanbanWindow) []string {
	leaving, err := m.eng.db.CountSurplusLeaving(k.KanbanNodeID)
	if err != nil {
		m.eng.logFn("keeper: %s: read its surplus moves: %v", k.KanbanName, err)
		return nil
	}
	for _, w := range windows {
		if w.Moving {
			leaving--
		}
	}
	return codes[:max(0, len(codes)-max(0, leaving))]
}

// surplusToWindows sends the carts to the windows of the kanban's unloaders, loader by
// loader, each under its own lock. Fewer than two windows standing open is what the
// tick's read already says, with none of freeWindows' statements.
func (m *KanbanKeeper) surplusToWindows(k store.KanbanPictureRow, queue []*bins.Bin, windows []store.KanbanWindow,
	causes map[string]string) {
	if len(queue) == 0 {
		return
	}
	why := "above its want, and no unloader sends back to " + k.KanbanName
	byLoader := map[int64][]*nodes.Node{}
	var order []int64
	seen := map[int64]bool{}
	for _, w := range windows {
		first := !seen[w.LoaderID]
		seen[w.LoaderID] = true
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
		moved, free, err := m.eng.dispatcher.SurplusToWindows(id, open, queue, "surplus of "+k.KanbanName)
		if err != nil {
			m.eng.logFn("keeper: %s: surplus to loader %d: %v", k.KanbanName, id, err)
		}
		if free >= 2 && len(moved) == 0 {
			why = "above its want, and no free window takes it"
		}
		went := make(map[int64]bool, len(moved))
		for _, c := range moved {
			went[c] = true
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

// surplusToSources sends the carts to the kanban's sources: the first in rank order whose
// count has room and which has a free slot, one read of it a cart. A slot lost since it
// was read free is the next source's question.
func (m *KanbanKeeper) surplusToSources(k store.KanbanPictureRow, queue []*bins.Bin,
	picture map[int64][]store.KanbanPictureRow, counts *store.KanbanCounts, causes map[string]string) {
	used := map[int64]int{}
	for _, c := range queue {
		sent := false
		for _, s := range sourcesWithRoom(k, c.BinTypeCode, picture, counts) {
			if s.room-used[s.group.ID] <= 0 {
				continue
			}
			res, err := m.eng.dispatcher.PlanGroupStoreSlot(s.group, "", binresolver.KnownCarrier(c))
			if err != nil || res == nil || res.Node == nil {
				continue
			}
			err = m.eng.dispatcher.SurplusToSlot(c, res.Node, k.StationID, "surplus of "+k.KanbanName+" to "+s.group.Name)
			if errors.Is(err, dispatch.ErrWindowTaken) {
				continue
			}
			if err != nil {
				m.eng.logFn("keeper: %s: surplus %s to %s: %v", k.KanbanName, c.Label, res.Node.Name, err)
				continue
			}
			used[s.group.ID]++
			sent = true
			m.eng.logFn("keeper: SURPLUS bin=%s of %s to %s", c.Label, k.KanbanName, res.Node.Name)
			break
		}
		if !sent {
			causes[c.BinTypeCode] = noSourceHasRoom(k.KanbanName)
		}
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

func distinct(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range lists {
		for _, s := range l {
			if !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	return out
}
