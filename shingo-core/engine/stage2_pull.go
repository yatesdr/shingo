// stage2_pull.go — Core moves the named bare cart from a two-stage unloader's
// wait group into a free stage-2 window.
//
// In pull mode stage 1's CLEAR stamps the cart bare and the Edge's empty-out
// sends it into the wait group (stage 2's inbound source). From there Core does
// the rest itself: loaders are a Core function and Core owns bins and orders,
// so the Edge does nothing new. Each move names its bin, so it goes straight to
// dispatch and never reaches an empty finder — bins.EmptyCarrierWhere keeps
// saying a bare cart is nobody's empty, absolutely.
//
// A wait group is an ordinary group: several pairs may name it, like any
// supermarket feeding several loaders. A pull takes only carts the target
// window admits (a marker fits where its carrier fits, bins.TypeAdmits), so two
// pairs with different cart types share a group without taking each other's
// carts, and two pairs with the same cart type share it first come first
// served.
//
// Triggers, all existing events: a bin leaving a stage-2 window (the window
// freed), an order completing into a wait group (a cart arrived), and a sweep at
// startup. Each runs the pull on a goroutine, serialized per pair, so a burst of
// triggers makes one move per free window.

package engine

import (
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"shingocore/dispatch"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Stage2EdgeUUIDPrefix marks a stage-2 pull, beside core-l1- (the loader loop)
// and core-mnt- (the level keeper).
const Stage2EdgeUUIDPrefix = "core-s2-"

// stage2Receipt is the receipt type Core files when it confirms its own landed
// pull.
const stage2Receipt = "core_s2_landed"

// stage2Puller serializes pulls per pair and tracks the trigger goroutines so
// Stop can wait for them. The zero value is ready.
type stage2Puller struct {
	mu      sync.Mutex
	locks   map[int64]*sync.Mutex // keyed by stage-1 loader id
	stopped bool
	wg      sync.WaitGroup
}

func (p *stage2Puller) pairLock(stage1ID int64) *sync.Mutex {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.locks == nil {
		p.locks = map[int64]*sync.Mutex{}
	}
	lk, ok := p.locks[stage1ID]
	if !ok {
		lk = &sync.Mutex{}
		p.locks[stage1ID] = lk
	}
	return lk
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

// PullStage2 moves bare carts from the pair's wait group into its free stage-2
// windows, one move per free window, and returns how many it made. A pair in
// direct mode (stage 2 has no inbound source) never pulls.
//
// Free window: a stage-2 home with no bin on it and no non-terminal order
// delivering there — Core's own truth, no Edge heuristic. Candidates: bare carts
// in the wait group's subtree, unclaimed and sourceable, oldest arrival first
// (bins.ListBareInGroup), each taken only by a window that admits it.
func (e *Engine) PullStage2(stage1ID int64) (int, error) {
	lk := e.stage2.pairLock(stage1ID)
	lk.Lock()
	defer lk.Unlock()

	one, two, err := e.pullPair(stage1ID)
	if err != nil || two == nil || two.InboundSource == "" {
		return 0, err
	}
	group, err := e.db.GetNodeByDotName(two.InboundSource)
	if err != nil {
		return 0, fmt.Errorf("stage 2 of %s pulls from %q: %w", one.Name, two.InboundSource, err)
	}
	free, err := e.freeStage2Windows(two)
	if err != nil || len(free) == 0 {
		return 0, err
	}
	carts, err := e.db.ListBareCartsInGroup(group.ID)
	if err != nil || len(carts) == 0 {
		return 0, err
	}
	caps, err := e.db.ListLoaderHomeBinTypes(two.ID)
	if err != nil {
		return 0, err
	}
	types := map[int64]*bins.BinType{}
	taken := map[int64]bool{}
	moved := 0
	for _, w := range free {
		for _, c := range carts {
			if taken[c.ID] {
				continue
			}
			ok, err := e.windowAdmits(w, caps[w.ID], c, types)
			if err != nil {
				return moved, err
			}
			if !ok {
				continue
			}
			taken[c.ID] = true
			if _, err := e.CreateBinMove(BinMoveRequest{
				Selection: BinSelectionByLabel, BinLabel: c.Label, DestNodeID: w.ID,
				StationID: e.stage2Station(w), EdgeUUID: Stage2EdgeUUIDPrefix + uuid.New().String(),
				Desc: "stage 2 pull for " + strings.TrimSuffix(one.Name, " · stage 1"),
			}); err != nil {
				// This cart could not go (lost to another mover, a lane that
				// refused it); the next one may.
				e.logFn("engine: stage-2 pull %s: cart %s to %s: %v", one.Name, c.Label, w.Name, err)
				continue
			}
			moved++
			break
		}
	}
	return moved, nil
}

// pullPair returns a live pair by its stage-1 id, or nils.
func (e *Engine) pullPair(stage1ID int64) (*loaders.Loader, *loaders.Loader, error) {
	one, err := e.db.GetLoader(stage1ID)
	if err != nil || one == nil || one.ArchivedAt != nil || one.SecondStageLoaderID == nil {
		return nil, nil, err
	}
	two, err := e.db.GetLoader(*one.SecondStageLoaderID)
	if err != nil || two == nil || two.ArchivedAt != nil {
		return nil, nil, err
	}
	return one, two, nil
}

// freeStage2Windows returns stage 2's windows with no bin and no non-terminal
// order delivering there.
func (e *Engine) freeStage2Windows(two *loaders.Loader) ([]*nodes.Node, error) {
	homes, err := e.db.ListLoaderHomes(two.ID)
	if err != nil {
		return nil, err
	}
	var free []*nodes.Node
	for _, h := range homes {
		w, err := e.db.GetNode(h.PositionNodeID)
		if err != nil {
			return nil, err
		}
		if !w.Enabled {
			continue
		}
		n, err := e.db.CountBinsByNode(w.ID)
		if err != nil {
			return nil, err
		}
		inbound, err := e.db.CountActiveOrdersByDeliveryNode(w.Name)
		if err != nil {
			return nil, err
		}
		if n == 0 && inbound == 0 {
			free = append(free, w)
		}
	}
	return free, nil
}

// windowAdmits applies both carrier restrictions a cart meets at a window —
// the node's Allowed Bin Types and the window's own capability list — with the
// marker admitted wherever its carrier is.
func (e *Engine) windowAdmits(w *nodes.Node, capCodes []string, c *bins.Bin, types map[int64]*bins.BinType) (bool, error) {
	typeOf := func(id int64) (*bins.BinType, error) {
		if bt, ok := types[id]; ok {
			return bt, nil
		}
		bt, err := e.db.GetBinType(id)
		if err != nil {
			return nil, err
		}
		types[id] = bt
		return bt, nil
	}
	marker, err := typeOf(c.BinTypeID)
	if err != nil {
		return false, err
	}
	allowed, err := e.db.GetEffectiveBinTypes(w.ID)
	if err != nil {
		return false, err
	}
	if !bins.TypeAdmits(allowed, marker.ID, marker.BareOf) {
		return false, nil
	}
	if len(capCodes) == 0 {
		return true, nil
	}
	carrierCode := ""
	if marker.BareOf != nil {
		carrier, err := typeOf(*marker.BareOf)
		if err != nil {
			return false, err
		}
		carrierCode = carrier.Code
	}
	for _, code := range capCodes {
		if code == marker.Code || (carrierCode != "" && code == carrierCode) {
			return true, nil
		}
	}
	return false, nil
}

// stage2Station is the Edge station a pull belongs to, so the delivered notice
// reaches the board holding the window: the window's one effective station,
// else the plant's one Edge, else none (the move still runs; the Edge learns of
// the cart from the bin on the window).
func (e *Engine) stage2Station(w *nodes.Node) string {
	if sts, err := e.db.GetEffectiveStations(w.ID); err == nil && len(sts) == 1 {
		return sts[0]
	}
	if edges, err := e.db.ListEdges(); err == nil && len(edges) == 1 {
		return edges[0].StationID
	}
	return ""
}

// pullPairsAsync runs a pull for each stage-1 id on the tracked goroutine.
func (e *Engine) pullPairsAsync(stage1IDs ...int64) {
	for _, id := range stage1IDs {
		id := id
		e.stage2.spawn(func() {
			if _, err := e.PullStage2(id); err != nil {
				e.logFn("engine: stage-2 pull for loader %d: %v", id, err)
			}
		})
	}
}

// pullModePairs lists the live pairs whose stage 2 pulls from a wait group,
// as stage-1 id → the group's node id.
func (e *Engine) pullModePairs() (map[int64]int64, error) {
	all, err := e.db.ListLoaders()
	if err != nil {
		return nil, err
	}
	byID := make(map[int64]*loaders.Loader, len(all))
	for i := range all {
		byID[all[i].ID] = &all[i]
	}
	out := map[int64]int64{}
	for _, l := range all {
		if l.SecondStageLoaderID == nil {
			continue
		}
		two, ok := byID[*l.SecondStageLoaderID]
		if !ok || two.InboundSource == "" {
			continue
		}
		g, err := e.db.GetNodeByDotName(two.InboundSource)
		if err != nil || g == nil {
			continue
		}
		out[l.ID] = g.ID
	}
	return out, nil
}

// sweepStage2Pulls is the startup trigger: every pull-mode pair, once.
func (e *Engine) sweepStage2Pulls() {
	pairs, err := e.pullModePairs()
	if err != nil {
		e.logFn("engine: stage-2 startup sweep: %v", err)
		return
	}
	for id := range pairs {
		e.pullPairsAsync(id)
	}
}

// onStage2WindowLeft is the pickup trigger: a bin left a node, and if that node
// is a stage-2 window of a pull-mode pair the window may now be free.
func (e *Engine) onStage2WindowLeft(fromNodeID int64) {
	if fromNodeID == 0 {
		return
	}
	h, err := e.db.GetLoaderHomeByPositionNode(fromNodeID)
	if err != nil || h == nil {
		return
	}
	pairs, err := e.pullModePairs()
	if err != nil {
		e.logFn("engine: stage-2 pull trigger: %v", err)
		return
	}
	for stage1ID := range pairs {
		one, err := e.db.GetLoader(stage1ID)
		if err == nil && one != nil && one.SecondStageLoaderID != nil && *one.SecondStageLoaderID == h.LoaderID {
			e.pullPairsAsync(stage1ID)
		}
	}
}

// onStage2OrderCompleted handles both halves of the completion event for the
// pull. A core-s2- move that is DELIVERED is confirmed here: the robot set the
// cart down on an empty window and there is no human receipt to wait for —
// the reason the Edge's own U2/L2 moves are created autoConfirm — while a
// delivered order stays non-terminal, so the window would count in flight until
// the auto-confirm sweep. And an order delivering into a wait group is a cart
// arriving there, which may be pulled.
func (e *Engine) onStage2OrderCompleted(ev OrderCompletedEvent) {
	if strings.HasPrefix(ev.EdgeUUID, Stage2EdgeUUIDPrefix) {
		order, err := e.db.GetOrder(ev.OrderID)
		if err != nil || order == nil || order.Status != dispatch.StatusDelivered || e.dispatcher == nil {
			return
		}
		// On the tracked goroutine, so the confirm lands after this event's
		// subscriber chain has finished — the same order the compound-child
		// auto-confirm gets by running after MarkDelivered returns
		// (wiring_vendor_status.go).
		e.stage2.spawn(func() {
			if _, err := e.dispatcher.Lifecycle().ConfirmReceipt(order, order.StationID, stage2Receipt, 0); err != nil {
				e.logFn("engine: confirm landed stage-2 pull %d: %v", order.ID, err)
			}
		})
		return
	}
	pairs, err := e.pullModePairs()
	if err != nil || len(pairs) == 0 {
		return
	}
	order, err := e.db.GetOrder(ev.OrderID)
	if err != nil || order == nil {
		return
	}
	e.pullOnArrival(order, pairs)
}

// pullOnArrival pulls for every pair whose wait group holds the order's
// delivery node.
func (e *Engine) pullOnArrival(order *orders.Order, pairs map[int64]int64) {
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
	for stage1ID, groupID := range pairs {
		if ancestors[groupID] {
			e.pullPairsAsync(stage1ID)
		}
	}
}
