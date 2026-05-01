package rds

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"time"
)

// PollerEmitter receives state transition events from the poller.
//
// EmitBlockCompleted fires once per block transition into FINISHED while
// the parent order is still mid-flight. This is the per-pickup signal
// the bin-transit-state design needs — vendor doesn't expose a separate
// "PICKED_UP" order state, but block-level state IS in the poll snapshot
// (just unused pre-2026-04). For pickup blocks, the engine handler
// transitions the corresponding bin onto the synthetic _TRANSIT node so
// the source slot is freed immediately.
type PollerEmitter interface {
	EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string, orderDetail *OrderDetail)
	EmitBlockCompleted(orderID int64, rdsOrderID, blockID, location, binTask string)
}

// OrderIDResolver maps RDS order IDs back to ShinGo order IDs.
type OrderIDResolver interface {
	ResolveRDSOrderID(rdsOrderID string) (int64, error)
}

// Poller periodically checks active RDS orders for state transitions.
type Poller struct {
	client   *Client
	emitter  PollerEmitter
	resolver OrderIDResolver
	interval time.Duration
	DebugLog func(string, ...any)

	mu       sync.Mutex
	active   map[string]OrderState // rdsOrderID -> last known state
	// blockStates tracks per-block state per active RDS order so we can
	// fire EmitBlockCompleted on the FIRST transition into FINISHED for
	// each block. Map: rdsOrderID -> blockID -> last-seen state. Cleared
	// alongside `active` when the parent order reaches a terminal state.
	blockStates map[string]map[string]OrderState
	stopChan    chan struct{}
	stopOnce    sync.Once
}

func NewPoller(client *Client, emitter PollerEmitter, resolver OrderIDResolver, interval time.Duration) *Poller {
	return &Poller{
		client:      client,
		emitter:     emitter,
		resolver:    resolver,
		interval:    interval,
		active:      make(map[string]OrderState),
		blockStates: make(map[string]map[string]OrderState),
		stopChan:    make(chan struct{}),
	}
}

func (p *Poller) dbg(format string, args ...any) {
	if fn := p.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// Track adds an RDS order ID to the active poll set.
func (p *Poller) Track(rdsOrderID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, exists := p.active[rdsOrderID]; !exists {
		p.active[rdsOrderID] = StateCreated
	}
}

// Untrack removes an RDS order ID from the active poll set.
func (p *Poller) Untrack(rdsOrderID string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.active, rdsOrderID)
	delete(p.blockStates, rdsOrderID)
}

// ActiveCount returns the number of orders being polled.
func (p *Poller) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

func (p *Poller) Start() {
	go p.run()
}

func (p *Poller) Stop() {
	p.stopOnce.Do(func() { close(p.stopChan) })
}

func (p *Poller) run() {
	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-p.stopChan:
			return
		case <-ticker.C:
			p.poll()
		}
	}
}

func (p *Poller) poll() {
	p.mu.Lock()
	ids := make([]string, 0, len(p.active))
	for id := range p.active {
		ids = append(ids, id)
	}
	p.mu.Unlock()

	if len(ids) > 0 {
		if len(ids) <= 10 {
			p.dbg("poll: %d active orders [%s]", len(ids), strings.Join(ids, ", "))
		} else {
			p.dbg("poll: %d active orders", len(ids))
		}
	}

	for _, rdsID := range ids {
		detail, err := p.client.GetOrderDetails(rdsID)
		if err != nil {
			log.Printf("poller: get order %s: %v", rdsID, err)
			p.dbg("poll error: GetOrderDetails(%s): %v", rdsID, err)
			continue
		}

		p.mu.Lock()
		oldState, exists := p.active[rdsID]
		p.mu.Unlock()
		if !exists {
			continue
		}

		newState := detail.State

		// Resolve once for both the order-state and per-block transitions.
		// Doing it inside the per-event branches would either re-resolve
		// multiple times per poll cycle or risk firing one branch with a
		// stale ID.
		var resolvedOrderID int64
		var resolvedOnce bool
		resolveOrderID := func() (int64, bool) {
			if resolvedOnce {
				return resolvedOrderID, resolvedOrderID != 0
			}
			resolvedOnce = true
			oid, err := p.resolver.ResolveRDSOrderID(rdsID)
			if err != nil {
				log.Printf("poller: resolve %s: %v", rdsID, err)
				p.dbg("poll error: resolve(%s): %v — will retry next cycle", rdsID, err)
				return 0, false
			}
			resolvedOrderID = oid
			return oid, true
		}

		// Per-block diff: compare each block's current state against the
		// last seen state for that block. Fire EmitBlockCompleted on the
		// first transition into FINISHED. We do this BEFORE the order-
		// state transition check so the per-block events arrive in
		// causally-correct order (block FINISHED → order moves on) — even
		// though the underlying RDS state field is sampled at one moment.
		p.diffBlockStates(rdsID, detail, resolveOrderID)

		if newState == oldState {
			continue
		}

		p.dbg("transition %s: %s -> %s (robot=%s)", rdsID, oldState, newState, detail.Vehicle)

		orderID, ok := resolveOrderID()
		if !ok {
			// Resolution failed — keep the old tracked state so the
			// transition is retried on the next poll cycle instead of
			// being silently lost.
			continue
		}

		// Resolution succeeded — now commit the state transition.
		p.mu.Lock()
		if newState.IsTerminal() {
			delete(p.active, rdsID)
			delete(p.blockStates, rdsID)
		} else {
			p.active[rdsID] = newState
		}
		p.mu.Unlock()

		p.emitter.EmitOrderStatusChanged(orderID, rdsID, string(oldState), string(newState), detail.Vehicle, fmt.Sprintf("fleet state: %s -> %s", oldState, newState), detail)
	}
}

// diffBlockStates compares the per-block states in `detail` against the
// last-seen states tracked for this RDS order, and fires EmitBlockCompleted
// for each block that newly transitioned to FINISHED. Updates the tracked
// per-block map in-place under the poller mutex.
//
// Block-level FINISHED events are the per-pickup signal the bin-transit-
// state design uses: a "load" or "unload" block reaching FINISHED means
// the robot has physically completed that step. The pickup-block case
// drives a bin's transition onto the synthetic _TRANSIT node (engine
// handler — see wiring_block_completed.go). Pre-2026-04 the per-block
// state was already in the poll snapshot but only marshalled into
// mission_events JSON — never compared, never surfaced as an event.
func (p *Poller) diffBlockStates(rdsID string, detail *OrderDetail, resolveOrderID func() (int64, bool)) {
	if len(detail.Blocks) == 0 {
		return
	}

	p.mu.Lock()
	prev, ok := p.blockStates[rdsID]
	if !ok {
		prev = make(map[string]OrderState, len(detail.Blocks))
	}
	type blockTransition struct{ blockID, location, binTask string }
	var newlyFinished []blockTransition
	for _, b := range detail.Blocks {
		if b.BlockID == "" {
			continue
		}
		old := prev[b.BlockID]
		if b.State == old {
			continue
		}
		prev[b.BlockID] = b.State
		// Only fire on the transition INTO FINISHED — once. Subsequent
		// polls keep `prev[blockID] = FINISHED` so the equality check
		// above short-circuits.
		if b.State == StateFinished && old != StateFinished {
			newlyFinished = append(newlyFinished, blockTransition{
				blockID:  b.BlockID,
				location: b.Location,
				binTask:  b.BinTask,
			})
		}
	}
	p.blockStates[rdsID] = prev
	p.mu.Unlock()

	if len(newlyFinished) == 0 {
		return
	}

	orderID, ok := resolveOrderID()
	if !ok {
		// Resolution failed — drop these block events. They'll be
		// re-emitted next cycle since `prev` was already updated, but
		// re-emit on the same already-FINISHED state is suppressed by
		// the equality check. Lose these events but don't loop.
		// Acceptable: order resolution failure means the ShinGo order
		// row is missing, in which case binding events to it isn't
		// useful anyway. Alternative would be to NOT update `prev`
		// here, but that risks duplicate emissions on the next
		// success.
		return
	}

	for _, b := range newlyFinished {
		p.dbg("block FINISHED %s/%s @ %s (binTask=%s)", rdsID, b.blockID, b.location, b.binTask)
		p.emitter.EmitBlockCompleted(orderID, rdsID, b.blockID, b.location, b.binTask)
	}
}
