package dispatch

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/store/bins"
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

// DrainEdgeUUIDPrefix marks a drain's move, beside core-l1- and core-u1- (the
// loader loop's asks) and core-s2- (stage 2's pull). The engine confirms a
// landed one as it confirms stage 2's: there is no receipt to wait for.
const DrainEdgeUUIDPrefix = "core-dr-"

// drainActor is who a drain's move's birth is recorded as taking a bin for.
const drainActor = "drain"

// DrainLoader moves carts of the states an unloader ticked from its inbound
// source into its free windows, one move per free window, and returns how many
// it made. A loader that drains nothing reads its own row and stops.
//
// THE WINDOWS are the loader's delivery targets for a part-less carrier
// (loaders.DeliveryTargets with no part: the shared windows in the plant's
// order, a funnelling unloader's first window only; a dedicated position
// never), each judged free by the predicate stage 2 shares (FreeWindows) and
// taking only a cart it admits (WindowAdmits). THE CARTS are
// bins.ListDrainableInGroup's, best first. A level declared on the source
// since the save stops the pull: the save's own test, asked again
// (store.DeclaresLevel).
//
// THE ACQUIRE RUNS UNDER THE LOCK, unlike ReplenishLoader's. A full ask's
// window choice (plan.LoaderWindows) counts what the capacity gate counts —
// a bin on the window, an order holding a claimed bin bound for it — and the
// asks of its own intent; a drain's move is neither an empty nor a full ask, so
// it is seen only once it holds its claim, which the acquire's dispatch takes.
// Acquired before the lock goes, a move the fleet takes at once is in flight
// before the next decision at the loader reads the windows. One the fleet
// cannot take yet holds its cart and not the window: a full ask may then be
// born for the same window, and the dispatch-time capacity gate, which every
// move and ask meets before it is sent (fulfillment/scanner.go fulfill), lets
// only one of them go there (named residual, W14). The scan lock ranks below
// the loader's, so the order is the lock order's own.
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
// not stage 1 of a pair. The save refuses a box everywhere else
// (service LoaderService.checkDrains); this keeps a row the save never wrote
// from pulling.
// (Stage 2 of a pair is known only by a read, and a pair is born whole with its
// boxes off, so it is the save's alone.)
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
	free, err := d.FreeWindows(windows)
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
	reads := &WindowReads{}
	taken := map[int64]bool{}
	moved := 0
	for _, w := range free {
		for _, c := range carts {
			if taken[c.ID] {
				continue
			}
			ok, err := d.WindowAdmits(w, caps[w.ID], c, reads)
			if err != nil {
				return moved, err
			}
			if !ok {
				continue
			}
			taken[c.ID] = true
			if err := d.drainMove(l, c, w); err != nil {
				// This cart could not go (lost to another mover, the planner
				// refused it); the next one may.
				d.dbg("drain loader=%d: cart %s to %s: %v", l.ID, c.Label, w.Name, err)
				continue
			}
			moved++
			break
		}
	}
	return moved, nil
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

// drainMove births one move of cart c to window w, naming the cart, and
// acquires it once. The plan is the bin move's (PlanMove: the cart at its node,
// the window's capacity and carrier rule). A drain is a robot's errand, never
// a person's, so it takes no bin another order has promised itself (Plan.Take
// refuses here; the population already leaves such a bin out, so only a race
// meets it). Station-unaware like every Core-born move; its material notices
// project to the window's Edge (Need.Projects), which binds the cart when it
// lands, as stage 2's do.
func (d *Dispatcher) drainMove(l *loaders.Loader, c *bins.Bin, w *nodes.Node) error {
	if c.NodeID == nil {
		return errors.New("the cart is at no node")
	}
	at, err := d.db.GetNode(*c.NodeID)
	if err != nil {
		return err
	}
	p := d.PlanMove(plan.Given{Bin: c, Source: at, Dest: w})
	if p.Refused != nil {
		return fmt.Errorf("the move's plan refused it (rule %d): %v", p.Refused.Rule, p.Refused.Err)
	}
	if p.Take {
		return errors.New("another order has promised itself the cart")
	}
	station := d.WindowStation(w)
	order, _, err := d.BirthMove(Need{
		Type:     OrderTypeMove,
		Station:  station,
		Ref:      DrainEdgeUUIDPrefix + uuid.New().String(),
		Quantity: 1,
		Desc:     "drain for " + l.Name,
		// No place asked for material: the unloader is clearing a cart it was
		// set up to clear, so there is no episode, and blank would read orphan.
		OriginClass: protocol.OriginClassNoDemand,
		Projects:    station,
		HoldsBin:    true,
	}, p, drainActor)
	if err != nil {
		log.Printf("dispatch: drain %s: birth of cart %s to %s: %v", l.Name, c.Label, w.Name, err)
		return err
	}
	// The row is whole at its INSERT, as a Core ask's is, so it takes the same
	// one acquire (acquireCoreAsk), under the loader's lock (DrainLoader).
	d.acquireCoreAsk(order)
	return nil
}
