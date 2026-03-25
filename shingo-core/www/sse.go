package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"shingocore/engine"
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
	stopOnce  sync.Once
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
	h.stopOnce.Do(func() { close(h.stopChan) })
}

func (h *EventHub) run() {
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
					log.Printf("sse: dropped %s event for slow client", evt.Event)
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
		log.Printf("sse: broadcast buffer full, dropped %s event", event)
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

// sseJSON safely marshals data to JSON for SSE broadcast.
// Falls back to an error payload if marshaling fails.
func sseJSON(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		log.Printf("sse: marshal error: %v", err)
		return `{"error":"marshal_failed"}`
	}
	return string(data)
}

// SetupEngineListeners wires engine events to SSE broadcasts.
func (h *EventHub) SetupEngineListeners(eng *engine.Engine) {
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderReceivedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "received", "order_id": ev.OrderID}))
	}, engine.EventOrderReceived)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderDispatchedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "dispatched", "order_id": ev.OrderID, "vendor_order_id": ev.VendorOrderID}))
	}, engine.EventOrderDispatched)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderStatusChangedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "status_changed", "order_id": ev.OrderID, "new_status": ev.NewStatus}))
	}, engine.EventOrderStatusChanged)

	// Mission telemetry live updates (separate event name from order-update)
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderStatusChangedEvent)
		data := map[string]any{
			"order_id":        ev.OrderID,
			"vendor_order_id": ev.VendorOrderID,
			"old_state":       ev.OldStatus,
			"new_state":       ev.NewStatus,
			"robot_id":        ev.RobotID,
		}
		if ev.Snapshot != nil {
			if len(ev.Snapshot.Blocks) > 0 {
				data["blocks"] = ev.Snapshot.Blocks
			}
			if len(ev.Snapshot.Errors) > 0 {
				data["errors"] = ev.Snapshot.Errors
			}
		}
		if ev.RobotID != "" {
			if rs, ok := eng.GetCachedRobotStatus(ev.RobotID); ok {
				data["robot_x"] = rs.X
				data["robot_y"] = rs.Y
				data["robot_station"] = rs.CurrentStation
				data["robot_battery"] = rs.BatteryLevel
			}
		}
		h.Broadcast("mission-event", sseJSON(data))
	}, engine.EventOrderStatusChanged)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderCompletedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "completed", "order_id": ev.OrderID}))
	}, engine.EventOrderCompleted)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderFailedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "failed", "order_id": ev.OrderID, "detail": ev.Detail}))
	}, engine.EventOrderFailed)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderCancelledEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "cancelled", "order_id": ev.OrderID, "reason": ev.Reason}))
	}, engine.EventOrderCancelled)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.OrderQueuedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "queued", "order_id": ev.OrderID, "payload_code": ev.PayloadCode}))
	}, engine.EventOrderQueued)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.BinUpdatedEvent)
		h.Broadcast("bin-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": ev.Action, "bin_id": ev.BinID, "actor": ev.Actor, "detail": ev.Detail}))
	}, engine.EventBinUpdated)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.CorrectionAppliedEvent)
		h.Broadcast("inventory-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": "correction", "type": ev.CorrectionType}))
	}, engine.EventCorrectionApplied)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.NodeUpdatedEvent)
		h.Broadcast("node-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": ev.Action}))
	}, engine.EventNodeUpdated)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"fleet":"connected"}`)
	}, engine.EventFleetConnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"fleet":"disconnected"}`)
	}, engine.EventFleetDisconnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"messaging":"connected"}`)
	}, engine.EventMessagingConnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"messaging":"disconnected"}`)
	}, engine.EventMessagingDisconnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"database":"connected"}`)
	}, engine.EventDBConnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		h.Broadcast("system-status", `{"database":"disconnected"}`)
	}, engine.EventDBDisconnected)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.CMSTransactionEvent)
		h.Broadcast("cms-transaction", sseJSON(ev.Transactions))
	}, engine.EventCMSTransaction)

	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.RobotsUpdatedEvent)
		type robotJSON struct {
			VehicleID      string  `json:"vehicle_id"`
			State          string  `json:"state"`
			IP             string  `json:"ip"`
			Model          string  `json:"model"`
			CurrentMap     string  `json:"map"`
			Battery        string  `json:"battery"`
			Charging       bool    `json:"charging"`
			CurrentStation string  `json:"station"`
			LastStation    string  `json:"last_station"`
			Available      bool    `json:"available"`
			Connected      bool    `json:"connected"`
			Blocked        bool    `json:"blocked"`
			Emergency      bool    `json:"emergency"`
			Busy           bool    `json:"processing"`
			IsError        bool    `json:"error"`
			X              float64 `json:"x"`
			Y              float64 `json:"y"`
			Angle          float64 `json:"angle"`
		}
		out := make([]robotJSON, len(ev.Robots))
		for i, r := range ev.Robots {
			out[i] = robotJSON{
				VehicleID:      r.VehicleID,
				State:          r.State(),
				IP:             r.IP,
				Model:          r.Model,
				CurrentMap:     r.CurrentMap,
				Battery:        fmt.Sprintf("%.0f", r.BatteryLevel),
				Charging:       r.Charging,
				CurrentStation: r.CurrentStation,
				LastStation:    r.LastStation,
				Available:      r.Available,
				Connected:      r.Connected,
				Blocked:        r.Blocked,
				Emergency:      r.Emergency,
				Busy:           r.Busy,
				IsError:        r.IsError,
				X:              r.X,
				Y:              r.Y,
				Angle:          r.Angle,
			}
		}
		h.Broadcast("robot-update", sseJSON(out))
	}, engine.EventRobotsUpdated)
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
	w.Header().Set("X-Accel-Buffering", "no")

	ch := h.AddClient()
	defer h.RemoveClient(ch)

	keepalive := time.NewTicker(30 * time.Second)
	defer keepalive.Stop()

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
		case <-keepalive.C:
			if _, err := fmt.Fprintf(w, ": keepalive\n\n"); err != nil {
				log.Printf("sse: keepalive write error: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
