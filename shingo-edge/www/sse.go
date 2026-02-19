package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"shingoedge/engine"
)

// SSEEvent is the typed envelope sent to SSE clients.
type SSEEvent struct {
	Type string      `json:"type"`
	Data interface{} `json:"data"`
}

type sseClient struct {
	events chan SSEEvent
}

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
		// Drop if broadcast buffer is full
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
	for {
		select {
		case <-h.stopChan:
			return
		case evt := <-h.broadcast:
			h.mu.RLock()
			for c := range h.clients {
				select {
				case c.events <- evt:
				default:
					// Client buffer full, drop event
				}
			}
			h.mu.RUnlock()
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

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	client := &sseClient{events: make(chan SSEEvent, 64)}
	h.register(client)
	defer h.unregister(client)

	// Send connected event
	fmt.Fprintf(w, "event: connected\ndata: {}\n\n")
	flusher.Flush()

	keepalive := time.NewTicker(30 * time.Second)
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
			fmt.Fprintf(w, ": keepalive\n\n")
			flusher.Flush()
		}
	}
}

// SetupEngineListeners wires engine events to SSE broadcasts.
func (h *EventHub) SetupEngineListeners(eng *engine.Engine) {
	eng.Events.Subscribe(func(evt engine.Event) {
		var sseEvt SSEEvent

		switch evt.Type {
		case engine.EventPayloadUpdated:
			p := evt.Payload.(engine.PayloadUpdatedEvent)
			sseEvt = SSEEvent{Type: "payload-update", Data: p}
		case engine.EventPayloadReorder:
			p := evt.Payload.(engine.PayloadReorderEvent)
			sseEvt = SSEEvent{Type: "payload-reorder", Data: p}
		case engine.EventOrderCreated:
			p := evt.Payload.(engine.OrderCreatedEvent)
			sseEvt = SSEEvent{Type: "order-update", Data: p}
		case engine.EventOrderStatusChanged:
			p := evt.Payload.(engine.OrderStatusChangedEvent)
			sseEvt = SSEEvent{Type: "order-update", Data: p}
		case engine.EventOrderCompleted:
			p := evt.Payload.(engine.OrderCompletedEvent)
			sseEvt = SSEEvent{Type: "order-update", Data: p}
		case engine.EventCounterDelta:
			p := evt.Payload.(engine.CounterDeltaEvent)
			sseEvt = SSEEvent{Type: "counter-update", Data: p}
		case engine.EventCounterAnomaly:
			p := evt.Payload.(engine.CounterAnomalyEvent)
			sseEvt = SSEEvent{Type: "counter-anomaly", Data: p}
		case engine.EventChangeoverStarted, engine.EventChangeoverStateChanged, engine.EventChangeoverCompleted:
			sseEvt = SSEEvent{Type: "changeover-update", Data: evt.Payload}
		case engine.EventCounterRead:
			p := evt.Payload.(engine.CounterReadEvent)
			sseEvt = SSEEvent{Type: "counter-read", Data: p}
		case engine.EventPLCHealthAlert:
			p := evt.Payload.(engine.PLCHealthAlertEvent)
			sseEvt = SSEEvent{Type: "plc-health-alert", Data: p}
		case engine.EventPLCHealthRecover:
			p := evt.Payload.(engine.PLCHealthRecoverEvent)
			sseEvt = SSEEvent{Type: "plc-health-recover", Data: p}
		case engine.EventPLCConnected:
			p := evt.Payload.(engine.PLCEvent)
			sseEvt = SSEEvent{Type: "plc-status", Data: map[string]interface{}{"plcName": p.PLCName, "connected": true}}
		case engine.EventPLCDisconnected:
			p := evt.Payload.(engine.PLCEvent)
			sseEvt = SSEEvent{Type: "plc-status", Data: map[string]interface{}{"plcName": p.PLCName, "connected": false, "error": p.Error}}
		case engine.EventWarLinkConnected, engine.EventWarLinkDisconnected:
			p := evt.Payload.(engine.WarLinkEvent)
			sseEvt = SSEEvent{Type: "warlink-status", Data: p}
		case engine.EventCoreNodesUpdated:
			p := evt.Payload.(engine.CoreNodesUpdatedEvent)
			sseEvt = SSEEvent{Type: "core-nodes", Data: p}
		default:
			return
		}

		h.Broadcast(sseEvt)
	})

	log.Printf("SSE listeners wired to engine events")
}
