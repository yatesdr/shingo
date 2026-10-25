package dispatch

import (
	"database/sql"
	"errors"
	"fmt"

	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// loader_drain.go — an unloader drains the part-less carts its boxes name out
// of its inbound source (W14; owner, 2026-10-22: "allow the unloader to click if
// it wants that option to auto drain for the different states").
//
// An unloader pulls fulls of the parts it serves (ReplenishLoader, the side
// cycle's U1) and partials only with Accept partials; it never pulled a carrier
// with no part. So a press whose outbound is a plain group, not the unloader,
// stranded SEND BACK's leftover-tote cart there. With a box ticked
// (loaders.DrainStates), Core moves each cart of that state that lands in the
// unloader's inbound source into a free window, where the crew strips and
// re-sorts it: the card walks it as an empty (engine/unloader_lines.go).
//
// THE DECISION IS UNDER THE LOADER'S OWN LOCK (loaderLock,
// lockorder.LoaderBudget), the one ReplenishLoader holds: no second lock, no
// new rank. So a drain and a full ask at one unloader are decided one at a
// time, and whichever asks first takes a free window. NO PREFERENCE between a
// full and a drained cart (owner, round 4 Q1: "why would anything have
// preference over the other? I don't think it matters either way"), and no read
// is made to choose one. A full ask that finds every window taken waits for the
// next, as it does when the windows are full (TestDrain_AndAFullAskNeverShareAWindow).
//
// EACH MOVE NAMES ITS BIN, so it goes straight to dispatch and never reaches an
// empty finder, and the empties-to-maintainer door (W9) never re-points it: it
// lifts off no line position.

// DrainLoader moves carts of the states an unloader ticked from its inbound
// source into its free windows, one move per free window, and returns how many
// it made. A loader that drains nothing reads its own row and stops.
//
// THE WINDOWS are the loader's delivery targets for a part-less carrier
// (loaders.DeliveryTargets with no part: the shared windows in the plant's
// order, a funnelling unloader's first window only; a dedicated position
// never), each judged free by the predicate stage 2 shares (freeWindows) and
// taking only a cart it admits (windowAdmits). THE CARTS are
// bins.ListDrainableInGroup's, best first. A level declared on the source
// since the save stops the pull: the save's own test, asked again
// (store.DeclaresLevel).
//
// THE MOVE HOLDS ITS WINDOW FROM ITS ISSUE (windowMove, Need.Window): a slot
// reservation taken in the issue's transaction. A full ask's window choice
// (plan.LoaderWindows) counts what the capacity gate counts (a bin on the
// window, an order holding a claimed bin bound for it), the asks of its own
// intent, and another order's reservation on the window. A drain's move is
// neither an empty nor a full ask, and holds no claim until the fleet takes it,
// so the reservation is what a full ask sees: born after the move, it is asked
// at another free window, even while the fleet cannot take the move yet. The
// move's dispatch reuses the row and its end releases it (TerminalizeOrder's
// ReleaseByOrder).
//
// THE ACQUIRE RUNS UNDER THE LOCK, unlike ReplenishLoader's, so a move the
// fleet takes at once is in flight before the next decision at the loader. The
// scan lock ranks below the loader's, so the order is the lock order's own.
func (d *Dispatcher) DrainLoader(loaderID int64) (int, error) {
	l, err := d.db.GetLoader(loaderID)
	if err != nil {
		return 0, fmt.Errorf("drain loader %d: %w", loaderID, err)
	}
	if !drainsAt(l) {
		return 0, nil
	}
	mu := d.loaderLock(loaderID)
	mu.Lock()
	defer mu.Unlock()
	return d.drainLocked(l)
}

// drainsAt reports whether a loader's own row lets it drain, reading nothing:
// a live unloader with a box ticked, an inbound source and shared windows, and
// not stage 1 of a pair. The save refuses a box everywhere else, the layout
// among them (service LoaderService.checkDrains, ErrDrainDedicatedLayout), and
// the page draws the boxes where the save would keep them (DrainAllowed); this
// keeps a row the save never wrote from pulling. (Stage 2 of a pair is known
// only by a read, and a pair is born whole with its boxes off, so it is the
// save's alone.)
//
// THE UPDATE DRAINS AT THE SAVE, THE CREATE DOES NOT. A save that leaves a box
// on drains at once (engine SaveLoader). A create with boxes on drains nothing
// until the events: a new loader has no windows until its homes are added, so
// there is nothing to drain into.
func drainsAt(l *loaders.Loader) bool {
	return l != nil && l.ArchivedAt == nil && len(l.Drains) > 0 &&
		l.Role == loaders.RoleConsume && l.InboundSource != "" &&
		l.SecondStageLoaderID == nil && l.Layout == loaders.LayoutSharedWindow
}

// drainLocked is DrainLoader's decision and moves, with the loader's lock held.
func (d *Dispatcher) drainLocked(l *loaders.Loader) (int, error) {
	source, err := d.db.GetNodeByDotName(l.InboundSource)
	switch {
	case errors.Is(err, sql.ErrNoRows), err == nil && source == nil:
		// The save resolve-checks the source; a node deleted since leaves
		// nothing to drain.
		d.dbg("drain loader=%d: inbound source %q names no node", l.ID, l.InboundSource)
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("%s drains from %q: %w", l.Name, l.InboundSource, err)
	}
	maintained, err := d.db.DeclaresLevel(source.ID)
	if err != nil {
		return 0, err
	}
	if maintained {
		d.dbg("drain loader=%d: %s declares a level now; an unloader never drains a maintained group", l.ID, source.Name)
		return 0, nil
	}
	windows, err := d.drainWindows(l)
	if err != nil {
		return 0, err
	}
	free, err := d.freeWindows(windows)
	if err != nil || len(free) == 0 {
		return 0, err
	}
	carts, err := d.db.ListDrainableCarts(source.ID, l.Drains.Ticked())
	if err != nil || len(carts) == 0 {
		return 0, err
	}
	caps, err := d.db.ListLoaderHomeBinTypes(l.ID)
	if err != nil {
		return 0, err
	}
	return d.pullInto(free, carts, caps, DrainEdgeUUIDPrefix, "drain for "+l.Name)
}

// drainWindows is the loader's delivery targets for a part-less carrier, as
// nodes, in target order: two reads, and one per target.
func (d *Dispatcher) drainWindows(l *loaders.Loader) ([]*nodes.Node, error) {
	homes, err := d.db.ListLoaderHomes(l.ID)
	if err != nil {
		return nil, err
	}
	names, err := d.db.LoaderMemberNodeNames(l.ID)
	if err != nil {
		return nil, err
	}
	targets, _ := loaders.DeliveryTargets(loaders.DeliveryTargetsInput{
		Layout: l.Layout, FunnelWindows: l.FunnelWindows, Homes: homes, NodeNames: names,
	})
	ids := make(map[string]int64, len(names))
	for id, name := range names {
		ids[name] = id
	}
	windows := make([]*nodes.Node, 0, len(targets))
	for _, t := range targets {
		w, err := d.db.GetNode(ids[t.NodeName])
		if err != nil {
			return nil, err
		}
		windows = append(windows, w)
	}
	return windows, nil
}
