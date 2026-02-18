package rds

import (
	"log"
	"sync"
	"time"
)

// PollerEmitter receives state transition events from the poller.
type PollerEmitter interface {
	EmitOrderStatusChanged(orderID int64, rdsOrderID, oldStatus, newStatus, robotID, detail string)
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

	mu       sync.Mutex
	active   map[string]OrderState // rdsOrderID -> last known state
	stopChan chan struct{}
}

func NewPoller(client *Client, emitter PollerEmitter, resolver OrderIDResolver, interval time.Duration) *Poller {
	return &Poller{
		client:   client,
		emitter:  emitter,
		resolver: resolver,
		interval: interval,
		active:   make(map[string]OrderState),
		stopChan: make(chan struct{}),
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
	select {
	case p.stopChan <- struct{}{}:
	default:
	}
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

	for _, rdsID := range ids {
		detail, err := p.client.GetOrderDetails(rdsID)
		if err != nil {
			log.Printf("poller: get order %s: %v", rdsID, err)
			continue
		}

		p.mu.Lock()
		oldState, exists := p.active[rdsID]
		p.mu.Unlock()
		if !exists {
			continue
		}

		newState := detail.State
		if newState == oldState {
			continue
		}

		// State transition detected
		p.mu.Lock()
		if newState.IsTerminal() {
			delete(p.active, rdsID)
		} else {
			p.active[rdsID] = newState
		}
		p.mu.Unlock()

		orderID, err := p.resolver.ResolveRDSOrderID(rdsID)
		if err != nil {
			log.Printf("poller: resolve %s: %v", rdsID, err)
			continue
		}

		p.emitter.EmitOrderStatusChanged(orderID, rdsID, string(oldState), string(newState), detail.Vehicle, "")
	}
}
