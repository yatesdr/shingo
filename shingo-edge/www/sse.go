package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
	"time"

	"shingoedge/engine"
)

// SSEEvent is the typed envelope sent to SSE clients.
type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

// serverInstance is a per-process identifier emitted on every SSE
// connect. The HMI compares it across reconnects: if it changes, the
// edge has been restarted (likely with a new JS bundle) and the tab
// hard-reloads. Without this, a long-lived operator-station tab keeps
// the previously-cached ES module graph forever — the cacheBust query
// param only affects fresh HTML loads.
var serverInstance = fmt.Sprintf("%x", time.Now().UnixNano())

// sseKeepaliveInterval is the heartbeat tick. Variable rather than
// const so tests can compress the interval and exercise the heartbeat
// path without a 30-second wait.
var sseKeepaliveInterval = 30 * time.Second

type sseClient struct {
	events chan SSEEvent
	drops  int // consecutive event drops; eviction trigger.
}

// MaxSSEClients caps concurrent SSE connections to prevent a
// misbehaving browser / scraper from growing the clients map
// unboundedly.
const MaxSSEClients = 256

// MaxConsecutiveDrops is the threshold past which a stuck client is
// evicted from the EventHub. The slow-client policy at run() drops
// events; without this cap a truly stuck client would stay in the
// map forever.
const MaxConsecutiveDrops = 10

// EventHub manages SSE client connections and broadcasts.
type EventHub struct {
	mu        sync.RWMutex
	clients   map[*sseClient]struct{}
	broadcast chan SSEEvent
	stopChan  chan struct{}
}

// NewEventHub creates a new EventHub.
func NewEventHub() *EventHub {
	return &EventHub{
		clients:   make(map[*sseClient]struct{}),
		broadcast: make(chan SSEEvent, 256),
		stopChan:  make(chan struct{}),
	}
}

// Start begins the event fan-out loop.
func (h *EventHub) Start() {
	go h.run()
}

// Stop shuts down the event hub.
func (h *EventHub) Stop() {
	select {
	case <-h.stopChan:
	default:
		close(h.stopChan)
	}
}

// Broadcast sends an event to all connected clients.
func (h *EventHub) Broadcast(evt SSEEvent) {
	select {
	case h.broadcast <- evt:
	default:
		log.Printf("sse: broadcast buffer full, dropped %s event", evt.Type)
	}
}

func (h *EventHub) register(c *sseClient) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
}

func (h *EventHub) unregister(c *sseClient) {
	h.mu.Lock()
	delete(h.clients, c)
	close(c.events)
	h.mu.Unlock()
}

func (h *EventHub) run() {
	// Wrap-with-exit (NOT wrap-with-restart). A panic in the fan-out
	// loop is almost certainly a real bug — closed channel, nil
	// map. Restarting would mask it. Process keeps running; UI dies
	// until restart. Log loudly so the next on-call grep finds the
	// cause.
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC sse-EventHub: %v\n%s", r, debug.Stack())
		}
	}()
	for {
		select {
		case <-h.stopChan:
			return
		case evt := <-h.broadcast:
			// Track per-client drop counts and evict the stuck ones
			// after MaxConsecutiveDrops. Collect targets under
			// RLock, evict under Lock outside the loop (avoid
			// upgrade-during-iterate races on h.clients).
			h.mu.RLock()
			var stuck []*sseClient
			for c := range h.clients {
				select {
				case c.events <- evt:
					c.drops = 0
				default:
					c.drops++
					log.Printf("sse: dropped %s event for slow client (drops=%d)", evt.Type, c.drops)
					if c.drops > MaxConsecutiveDrops {
						stuck = append(stuck, c)
					}
				}
			}
			h.mu.RUnlock()
			for _, c := range stuck {
				log.Printf("WARNING sse: evicting stuck client after %d consecutive drops", c.drops)
				h.unregister(c)
			}
		}
	}
}

// HandleSSE is the HTTP handler for SSE connections.
func (h *EventHub) HandleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "SSE not supported", http.StatusInternalServerError)
		return
	}

	// Cap concurrent clients. Refuse with 503 at cap so an
	// aggressive reconnect loop can't grow the map unboundedly.
	h.mu.RLock()
	count := len(h.clients)
	h.mu.RUnlock()
	if count >= MaxSSEClients {
		http.Error(w, "too many SSE clients", http.StatusServiceUnavailable)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	client := &sseClient{events: make(chan SSEEvent, 64)}
	h.register(client)
	defer h.unregister(client)

	// Send connected event with the per-process build id so reconnects
	// after an edge restart trigger a hard-reload on the client.
	fmt.Fprintf(w, "event: connected\ndata: {\"build\":\"%s\"}\n\n", serverInstance)
	flusher.Flush()

	keepalive := time.NewTicker(sseKeepaliveInterval)
	defer keepalive.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.stopChan:
			return
		case evt, ok := <-client.events:
			if !ok {
				return
			}
			data, err := json.Marshal(evt.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Type, data)
			flusher.Flush()
		case <-keepalive.C:
			// Named heartbeat event carries the build id on the existing
			// connection — mid-stream version comparison without reconnect.
			// The bare `: keepalive` comment was stripped by EventSource and
			// never reached the JS client, so it could not carry the build id.
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"build\":\"%s\"}\n\n", serverInstance)
			flusher.Flush()
		}
	}
}

// SetupEngineListeners wires engine events to SSE broadcasts.
func (h *EventHub) SetupEngineListeners(eng *engine.Engine) {
	eng.Events.Subscribe(func(evt engine.Event) {
		var sseEvt SSEEvent

		switch evt.Type {
		case engine.EventOrderCreated:
			if p, ok := evt.Payload.(engine.OrderCreatedEvent); ok {
				sseEvt = SSEEvent{Type: "order-update", Data: p}
			}
		case engine.EventOrderStatusChanged:
			if p, ok := evt.Payload.(engine.OrderStatusChangedEvent); ok {
				sseEvt = SSEEvent{Type: "order-update", Data: p}
			}
		case engine.EventOrderCompleted:
			if p, ok := evt.Payload.(engine.OrderCompletedEvent); ok {
				sseEvt = SSEEvent{Type: "order-update", Data: p}
			}
		case engine.EventOrderFailed:
			// Broadcast as a distinct "order-failed" event so the client can
			// fire a sticky toast with the parsed failure reason. Without this
			// case the operator has no async indication that their order died
			// on Core (fleet failure, structural error, admin terminate, etc.).
			if p, ok := evt.Payload.(engine.OrderFailedEvent); ok {
				sseEvt = SSEEvent{Type: "order-failed", Data: p}
			}
		case engine.EventOrderFaulted:
			if p, ok := evt.Payload.(engine.OrderFaultedEvent); ok {
				sseEvt = SSEEvent{Type: "order-faulted", Data: p}
			}
		case engine.EventCounterDelta:
			if p, ok := evt.Payload.(engine.CounterDeltaEvent); ok {
				sseEvt = SSEEvent{Type: "counter-update", Data: p}
			}
		case engine.EventCounterAnomaly:
			if p, ok := evt.Payload.(engine.CounterAnomalyEvent); ok {
				sseEvt = SSEEvent{Type: "counter-anomaly", Data: p}
			}
		case engine.EventCounterRead:
			if p, ok := evt.Payload.(engine.CounterReadEvent); ok {
				sseEvt = SSEEvent{Type: "counter-read", Data: p}
			}
		case engine.EventPLCHealthAlert:
			if p, ok := evt.Payload.(engine.PLCHealthAlertEvent); ok {
				sseEvt = SSEEvent{Type: "plc-health-alert", Data: p}
			}
		case engine.EventPLCHealthRecover:
			if p, ok := evt.Payload.(engine.PLCHealthRecoverEvent); ok {
				sseEvt = SSEEvent{Type: "plc-health-recover", Data: p}
			}
		case engine.EventPLCConnected:
			if p, ok := evt.Payload.(engine.PLCEvent); ok {
				sseEvt = SSEEvent{Type: "plc-status", Data: map[string]interface{}{"plcName": p.PLCName, "connected": true}}
			}
		case engine.EventPLCDisconnected:
			if p, ok := evt.Payload.(engine.PLCEvent); ok {
				sseEvt = SSEEvent{Type: "plc-status", Data: map[string]interface{}{"plcName": p.PLCName, "connected": false, "error": p.Error}}
			}
		case engine.EventWarLinkConnected, engine.EventWarLinkDisconnected:
			if p, ok := evt.Payload.(engine.WarLinkEvent); ok {
				sseEvt = SSEEvent{Type: "warlink-status", Data: p}
			}
		case engine.EventCoreNodesUpdated:
			if p, ok := evt.Payload.(engine.CoreNodesUpdatedEvent); ok {
				sseEvt = SSEEvent{Type: "core-nodes", Data: p}
			}
		case engine.EventCounterReadError:
			if p, ok := evt.Payload.(engine.CounterReadErrorEvent); ok {
				sseEvt = SSEEvent{Type: "counter-read-error", Data: p}
			}
		default:
			return
		}

		if sseEvt.Type != "" {
			h.Broadcast(sseEvt)
		}
	})

	log.Printf("SSE listeners wired to engine events")
}
