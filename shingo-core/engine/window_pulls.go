// window_pulls.go — the triggers of Core's two window pulls, the goroutines
// they run on, and the confirm of a landed pull's move. The pulls themselves
// decide in dispatch (dispatch/loader_window.go): stage 2's (PullStage2: a bare
// cart from a two-stage unloader's wait group into a free stage-2 window) and
// the drain (DrainLoader: a part-less cart from an unloader's inbound source
// into a free window).
//
// In pull mode stage 1's CLEAR stamps the cart bare and the Edge's empty-out
// sends it into the wait group (stage 2's inbound source). From there Core does
// the rest itself: loaders are a Core function and Core owns bins and orders,
// so the Edge does nothing new. Each move names its bin, so it goes straight to
// dispatch and never reaches an empty finder — bins.EmptyCarrierWhere keeps
// saying a bare cart is nobody's empty, absolutely.
//
// Triggers, all existing events: a bin leaving a stage-2 window (the window
// freed), an order completing into a wait group (a cart arrived), and a sweep at
// startup. Each runs the pull on a goroutine; stage 2's loader lock serializes
// the pulls, so a burst of triggers makes one move per free window.
//
// THE SAME TRIGGERS RUN THE DRAIN (W14): a bin leaving a draining unloader's
// window, an order completing into its inbound source, and the same startup
// sweep. Each event reads the loaders once (readWindowPulls) and runs both
// pulls; the two subscriptions were widened, not copied, so registration order
// is unchanged. One more runs it outside the events: the loader save that
// leaves a box on (www apiUpdateLoader), which is the crew ticking it.

package engine

import (
	"strings"
	"sync"

	"shingo/protocol/lockcheck"
	"shingocore/dispatch"
	"shingocore/internal/lockorder"
	"shingocore/store/loaders"
	"shingocore/store/orders"
)

// stage2Receipt and drainReceipt are the receipt types Core files when it
// confirms its own landed pull: stage 2's, and the drain's
// (dispatch.DrainEdgeUUIDPrefix).
const (
	stage2Receipt = "core_s2_landed"
	drainReceipt  = "core_drain_landed"
)

// stage2Puller tracks the trigger goroutines of both window pulls (stage 2's
// and the drain's, each decided under its loader's own lock in dispatch) so
// Stop can wait for them. The zero value is ready.
type stage2Puller struct {
	mu      lockcheck.Mutex[lockorder.Stage2Pulls]
	stopped bool
	wg      sync.WaitGroup
}

// spawn runs fn on a tracked goroutine, or not at all once stopped.
func (p *stage2Puller) spawn(fn func()) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	p.wg.Add(1)
	go func() {
		defer p.wg.Done()
		fn()
	}()
}

// stop refuses new goroutines and waits for the running ones.
func (p *stage2Puller) stop() {
	p.mu.Lock()
	p.stopped = true
	p.mu.Unlock()
	p.wg.Wait()
}

// pullPairsAsync runs a pull for each stage-1 id on the tracked goroutine.
func (e *Engine) pullPairsAsync(stage1IDs ...int64) {
	for _, id := range stage1IDs {
		id := id
		e.stage2.spawn(func() {
			if _, err := e.dispatcher.PullStage2(id); err != nil {
				e.logFn("engine: stage-2 pull for loader %d: %v", id, err)
			}
		})
	}
}

// drainAsync runs an unloader's drain (dispatch.DrainLoader) for each loader id
// on the same tracked goroutines: the loader's own lock serializes them, so a
// burst of triggers makes one move per free window.
func (e *Engine) drainAsync(loaderIDs ...int64) {
	for _, id := range loaderIDs {
		id := id
		e.stage2.spawn(func() {
			if _, err := e.dispatcher.DrainLoader(id); err != nil {
				e.logFn("engine: drain for loader %d: %v", id, err)
			}
		})
	}
}

// windowPulls is the loader set as Core's two window pulls read it, from ONE
// read of the loaders, so an event that may concern either reads them once and
// runs both (W14: stage 2's subscriptions widened, not copied).
type windowPulls struct {
	// pairs is the live pairs whose stage 2 pulls from a wait group: stage-1
	// id → the group's node id. stage2 maps the stage-2 id back to stage 1's.
	pairs  map[int64]int64
	stage2 map[int64]int64
	// drains is the unloaders with a drain box ticked and an inbound source:
	// loader id → the source's node id. DrainLoader re-checks the row.
	drains map[int64]int64
}

func (p windowPulls) none() bool { return len(p.pairs) == 0 && len(p.drains) == 0 }

// readWindowPulls reads the loaders once and resolves each pull's group: one
// statement, and one per pull-mode pair and per draining unloader.
func (e *Engine) readWindowPulls() (windowPulls, error) {
	all, err := e.db.ListLoaders()
	if err != nil {
		return windowPulls{}, err
	}
	byID := make(map[int64]*loaders.Loader, len(all))
	for i := range all {
		byID[all[i].ID] = &all[i]
	}
	p := windowPulls{pairs: map[int64]int64{}, stage2: map[int64]int64{}, drains: map[int64]int64{}}
	groupOf := func(name string) (int64, bool) {
		g, err := e.db.GetNodeByDotName(name)
		if err != nil || g == nil {
			return 0, false
		}
		return g.ID, true
	}
	for _, l := range all {
		if len(l.Drains) > 0 && l.InboundSource != "" {
			if id, ok := groupOf(l.InboundSource); ok {
				p.drains[l.ID] = id
			}
		}
		if l.SecondStageLoaderID == nil {
			continue
		}
		two, ok := byID[*l.SecondStageLoaderID]
		if !ok || two.InboundSource == "" {
			continue
		}
		if id, ok := groupOf(two.InboundSource); ok {
			p.pairs[l.ID] = id
			p.stage2[two.ID] = l.ID
		}
	}
	return p, nil
}

// startTwoStage is the two-stage unloaders' startup. Every pair is re-derived
// first, so one set up before stage 1 had a group of its windows gets one now
// rather than at its next edit (LoaderService.SyncPairs). Then a cart that
// reached a wait group or a drained unloader's inbound source while Core was
// down, or a window freed then, is pulled now rather than on the next event.
func (e *Engine) startTwoStage() {
	e.loaderService.SyncPairs()
	e.sweepWindowPulls()
}

// sweepWindowPulls is the startup trigger: every pull-mode pair and every
// draining unloader, once.
func (e *Engine) sweepWindowPulls() {
	p, err := e.readWindowPulls()
	if err != nil {
		e.logFn("engine: window pulls' startup sweep: %v", err)
		return
	}
	for id := range p.pairs {
		e.pullPairsAsync(id)
	}
	for id := range p.drains {
		e.drainAsync(id)
	}
}

// onWindowLeft is the pickup trigger: a bin left a node, and if that node is a
// window of a pull-mode pair's stage 2 or of a draining unloader the window may
// now be free.
func (e *Engine) onWindowLeft(fromNodeID int64) {
	if fromNodeID == 0 {
		return
	}
	h, err := e.db.GetLoaderHomeByPositionNode(fromNodeID)
	if err != nil || h == nil {
		return
	}
	p, err := e.readWindowPulls()
	if err != nil {
		e.logFn("engine: window pull trigger: %v", err)
		return
	}
	if stage1ID, ok := p.stage2[h.LoaderID]; ok {
		e.pullPairsAsync(stage1ID)
	}
	if _, ok := p.drains[h.LoaderID]; ok {
		e.drainAsync(h.LoaderID)
	}
}

// landedReceipt is the receipt type Core files when it confirms a landed move
// of one of its own window pulls, by the move's prefix, or "" for any other
// order.
func landedReceipt(edgeUUID string) string {
	switch {
	case strings.HasPrefix(edgeUUID, dispatch.Stage2EdgeUUIDPrefix):
		return stage2Receipt
	case strings.HasPrefix(edgeUUID, dispatch.DrainEdgeUUIDPrefix):
		return drainReceipt
	}
	return ""
}

// onWindowPullOrderCompleted handles both halves of the completion event for
// the window pulls. A core-s2- or core-dr- move that is DELIVERED is confirmed
// here: the robot set the cart down on an empty window and there is no human
// receipt to wait for — the reason the Edge's own U2/L2 moves are created
// autoConfirm — while a delivered order stays non-terminal and keeps its bin's
// claim, so the window would count in flight and the crew's CLEAR could not
// source the cart until the auto-confirm sweep. And an order delivering into a
// wait group, or into a draining unloader's inbound source, is a cart arriving
// there, which may be pulled.
func (e *Engine) onWindowPullOrderCompleted(ev OrderCompletedEvent) {
	if receipt := landedReceipt(ev.EdgeUUID); receipt != "" {
		order, err := e.db.GetOrder(ev.OrderID)
		if err != nil || order == nil || order.Status != dispatch.StatusDelivered || e.dispatcher == nil {
			return
		}
		// On the tracked goroutine, so the confirm lands after this event's
		// subscriber chain has finished — the same order the compound-child
		// auto-confirm gets by running after MarkDelivered returns
		// (wiring_vendor_status.go).
		e.stage2.spawn(func() {
			if _, err := e.dispatcher.Lifecycle().ConfirmReceipt(order, order.StationID, receipt, 0); err != nil {
				e.logFn("engine: confirm landed window pull %d: %v", order.ID, err)
			}
		})
		return
	}
	p, err := e.readWindowPulls()
	if err != nil || p.none() {
		return
	}
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil || order == nil {
		return
	}
	e.pullOnArrival(order, p)
}

// pullOnArrival pulls for every pair whose wait group holds the order's
// delivery node, and drains every unloader whose inbound source is that node
// or holds it.
func (e *Engine) pullOnArrival(order *orders.Order, p windowPulls) {
	if order.DeliveryNode == "" {
		return
	}
	node, err := e.db.GetNodeByDotName(order.DeliveryNode)
	if err != nil || node == nil {
		return
	}
	ancestors := map[int64]bool{}
	for n, depth := node, 0; n != nil && n.ParentID != nil && depth < 32; depth++ {
		ancestors[*n.ParentID] = true
		if n, err = e.db.GetNode(*n.ParentID); err != nil {
			break
		}
	}
	for stage1ID, groupID := range p.pairs {
		if ancestors[groupID] {
			e.pullPairsAsync(stage1ID)
		}
	}
	for loaderID, sourceID := range p.drains {
		if sourceID == node.ID || ancestors[sourceID] {
			e.drainAsync(loaderID)
		}
	}
}
