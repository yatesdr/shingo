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
// the bin-transit-state design needs â€” vendor doesn't expose a separate
// "PICKED_UP" order state, but block-level state IS in the poll snapshot
// (just unused pre-2026-04). For pickup blocks, the engine handler
// transitions the corresponding bin onto the synthetic _TRANSIT node so
// the source slot is freed immediately.
type PollerEmitter interface {
	EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string, orderDetail *OrderDetail)
	EmitBlockCompleted(orderID int64, rdsOrderID, blockID, location, binTask string)
	EmitGraceExpired(orderID int64, rdsOrderID string)
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
	// faultedDeadline tracks RDS orders currently in FAILED state that
	// are within the grace period. Map: rdsOrderID -> deadline. Orders
	// past their deadline are escalated to grace-expiry on the next poll
	// cycle. Cleared on recovery (FAILED->RUNNING) or terminal transition.
	faultedDeadline map[string]time.Time
	graceDuration   time.Duration
	stopChan    chan struct{}
	stopOnce    sync.Once
}

func NewPoller(client *Client, emitter PollerEmitter, resolver OrderIDResolver, interval time.Duration, graceDuration ...time.Duration) *Poller {
	gd := 10 * time.Minute
	if len(graceDuration) > 0 && graceDuration[0] > 0 {
		gd = graceDuration[0]
	}
	return &Poller{
		client:          client,
		emitter:         emitter,
		resolver:        resolver,
		interval:        interval,
		active:          make(map[string]OrderState),
		blockStates:     make(map[string]map[string]OrderState),
		faultedDeadline: make(map[string]time.Time),
		graceDuration:   gd,
		stopChan:        make(chan struct{}),
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
	delete(p.faultedDeadline, rdsOrderID)
}

// ActiveCount returns the number of orders being polled.
func (p *Poller) ActiveCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.active)
}

// FaultedCount returns the number of orders currently in the grace period.
func (p *Poller) FaultedCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.faultedDeadline)
}

// SetGraceDuration updates the grace period for future FAILED entries.
func (p *Poller) SetGraceDuration(d time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.graceDuration = d
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

	p.checkGraceExpiry()

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
				p.dbg("poll error: resolve(%s): %v â€” will retry next cycle", rdsID, err)
				return 0, false
			}
			resolvedOrderID = oid
			return oid, true
		}

		// Per-block diff: compare each block's current state against the
		// last seen state for that block. Fire EmitBlockCompleted on the
		// first transition into FINISHED. We do this BEFORE the order-
		// state transition check so the per-block events arrive in
		// causally-correct order (block FINISHED â†’ order moves on) â€” even
		// though the underlying RDS state field is sampled at one moment.
		p.diffBlockStates(rdsID, detail, resolveOrderID)

		if newState == oldState {
			continue
		}

		p.dbg("transition %s: %s -> %s (robot=%s)", rdsID, oldState, newState, detail.Vehicle)

		orderID, ok := resolveOrderID()
		if !ok {
			// Resolution failed â€” keep the old tracked state so the
			// transition is retried on the next poll cycle instead of
			// being silently lost.
			continue
		}

		// Resolution succeeded â€” now commit the state transition.
		p.mu.Lock()
		if newState == StateFailed {
			// FAILED is no longer terminal per SEER docs. Record grace deadline
			// and keep polling so the engine can recover or escalate on expiry.
			p.active[rdsID] = newState
			if _, hasDeadline := p.faultedDeadline[rdsID]; !hasDeadline {
				p.faultedDeadline[rdsID] = time.Now().Add(p.graceDuration)
				p.dbg("faulted: %s entered FAILED, grace deadline in %s", rdsID, p.graceDuration)
			}
		} else if newState.IsTerminal() {
			delete(p.active, rdsID)
			delete(p.blockStates, rdsID)
			delete(p.faultedDeadline, rdsID)
		} else {
			p.active[rdsID] = newState
			// Recovery: order left FAILED state, clear any grace deadline.
			if _, wasFaulted := p.faultedDeadline[rdsID]; wasFaulted {
				delete(p.faultedDeadline, rdsID)
				p.dbg("faulted: %s recovered from FAILED", rdsID)
			}
		}
		p.mu.Unlock()

		p.emitter.EmitOrderStatusChanged(orderID, rdsID, string(oldState), string(newState), detail.Vehicle, fmt.Sprintf("fleet state: %s -> %s", oldState, newState), detail)
	}
}

// checkGraceExpiry scans faultedDeadline for expired entries and emits
// grace-expiry events for any orders that exceeded their grace period while
// still in FAILED state. Expired entries are untracked.
func (p *Poller) checkGraceExpiry() {
	now := time.Now()
	p.mu.Lock()
	var expired []string
	for rdsID, deadline := range p.faultedDeadline {
		if now.After(deadline) {
			if state, ok := p.active[rdsID]; ok && state == StateFailed {
				expired = append(expired, rdsID)
			}
		}
	}
	p.mu.Unlock()

	for _, rdsID := range expired {
		p.mu.Lock()
		delete(p.active, rdsID)
		delete(p.blockStates, rdsID)
		delete(p.faultedDeadline, rdsID)
		p.mu.Unlock()

		oid, err := p.resolver.ResolveRDSOrderID(rdsID)
		if err != nil {
			log.Printf("poller: resolve expired %s: %v", rdsID, err)
			continue
		}
		p.dbg("faulted: %s grace expired, emitting grace-expiry", rdsID)
		p.emitter.EmitGraceExpired(oid, rdsID)
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
// handler â€” see wiring_block_completed.go). Pre-2026-04 the per-block
// state was already in the poll snapshot but only marshalled into
// mission_events JSON â€” never compared, never surfaced as an event.
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
		// Only fire on the transition INTO FINISHED â€” once. Subsequent
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
		// Resolution failed â€” drop these block events. They'll be
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
