package www

import (
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"warpath/engine"
)

type SSEEvent struct {
	Event string
	Data  string
}

type EventHub struct {
	mu        sync.RWMutex
	clients   map[chan SSEEvent]struct{}
	broadcast chan SSEEvent
	stopChan  chan struct{}
}

func NewEventHub() *EventHub {
	return &EventHub{
		clients:   make(map[chan SSEEvent]struct{}),
		broadcast: make(chan SSEEvent, 256),
		stopChan:  make(chan struct{}),
	}
}

func (h *EventHub) Start() {
	go h.run()
}

func (h *EventHub) Stop() {
	select {
	case h.stopChan <- struct{}{}:
	default:
	}
}

func (h *EventHub) run() {
	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

	for {
		select {
		case <-h.stopChan:
			return
		case evt := <-h.broadcast:
			h.mu.RLock()
			for ch := range h.clients {
				select {
				case ch <- evt:
				default:
					// drop if full
				}
			}
			h.mu.RUnlock()
		case <-keepalive.C:
			h.mu.RLock()
			for ch := range h.clients {
				select {
				case ch <- SSEEvent{Event: "keepalive", Data: "ping"}:
				default:
				}
			}
			h.mu.RUnlock()
		}
	}
}

func (h *EventHub) Broadcast(event, data string) {
	select {
	case h.broadcast <- SSEEvent{Event: event, Data: data}:
	default:
	}
}

func (h *EventHub) AddClient() chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	h.mu.Lock()
	h.clients[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

func (h *EventHub) RemoveClient(ch chan SSEEvent) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
	close(ch)
}

func (h *EventHub) ClientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// SetupEngineListeners wires engine events to SSE broadcasts.
func (h *EventHub) SetupEngineListeners(eng *engine.Engine) {
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"received","order_id":%d}`, evt.Payload.(engine.OrderReceivedEvent).OrderID))
	}, engine.EventOrderReceived)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderDispatchedEvent)
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"dispatched","order_id":%d,"rds_order_id":"%s"}`, ev.OrderID, ev.RDSOrderID))
	}, engine.EventOrderDispatched)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderStatusChangedEvent)
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"status_changed","order_id":%d,"new_status":"%s"}`, ev.OrderID, ev.NewStatus))
	}, engine.EventOrderStatusChanged)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderCompletedEvent)
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"completed","order_id":%d}`, ev.OrderID))
	}, engine.EventOrderCompleted)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderFailedEvent)
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"failed","order_id":%d,"detail":"%s"}`, ev.OrderID, ev.Detail))
	}, engine.EventOrderFailed)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderCancelledEvent)
		h.Broadcast("order-update", fmt.Sprintf(`{"type":"cancelled","order_id":%d,"reason":"%s"}`, ev.OrderID, ev.Reason))
	}, engine.EventOrderCancelled)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.PayloadChangedEvent)
		h.Broadcast("payload-update", fmt.Sprintf(`{"node_id":%d,"action":"%s","payload_id":%d}`, ev.NodeID, ev.Action, ev.PayloadID))
	}, engine.EventPayloadChanged)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.CorrectionAppliedEvent)
		h.Broadcast("inventory-update", fmt.Sprintf(`{"node_id":%d,"action":"correction","type":"%s"}`, ev.NodeID, ev.CorrectionType))
	}, engine.EventCorrectionApplied)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.NodeUpdatedEvent)
		h.Broadcast("node-update", fmt.Sprintf(`{"node_id":%d,"action":"%s"}`, ev.NodeID, ev.Action))
	}, engine.EventNodeUpdated)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"rds":"connected"}`)
	}, engine.EventRDSConnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"rds":"disconnected"}`)
	}, engine.EventRDSDisconnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"messaging":"connected"}`)
	}, engine.EventMessagingConnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"messaging":"disconnected"}`)
	}, engine.EventMessagingDisconnected)
}

// SSEHandler serves the SSE endpoint.
func (h *EventHub) SSEHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	ch := h.AddClient()
	defer h.RemoveClient(ch)

	for {
		select {
		case <-r.Context().Done():
			return
		case evt := <-ch:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Event, evt.Data); err != nil {
				log.Printf("sse: write error: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
