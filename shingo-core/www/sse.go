package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"shingo/shared/clock"
	"shingocore/dispatch/eta"
	"shingocore/engine"
)

// serverInstance is a per-process identifier emitted on the SSE
// `connected` event. The client compares it across reconnects: if it
// changes, the core has been restarted (likely with a new JS bundle)
// and the tab hard-reloads. Without this, a long-lived admin tab keeps
// its previously-cached static assets forever — cacheBust on HTML only
// fires on fresh page loads.
var serverInstance = fmt.Sprintf("%x", time.Now().UnixNano())

type SSEEvent struct {
	Event string
	Data  string
}

type EventHub struct {
	mu sync.RWMutex
	// clients maps each subscriber channel to its topic filter: a set of
	// event names the client wants. A nil set means "all events" (the
	// legacy, unfiltered behavior). Topic filtering lets the dashboard SSE
	// bus (shared/utils.js onSSE) request only the event types a tab
	// subscribed to via /events?topics=… so a /missions admin tab never
	// receives the per-pulse cell-heartbeat firehose (plan §6).
	clients   map[chan SSEEvent]map[string]bool
	broadcast chan SSEEvent
	stopChan  chan struct{}
	stopOnce  sync.Once
}

func NewEventHub() *EventHub {
	return &EventHub{
		clients:   make(map[chan SSEEvent]map[string]bool),
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
			for ch, topics := range h.clients {
				if topics != nil && !topics[evt.Event] {
					continue // client filtered this event type out
				}
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

// AddClient registers an unfiltered subscriber that receives every event.
func (h *EventHub) AddClient() chan SSEEvent {
	return h.AddClientFiltered(nil)
}

// AddClientFiltered registers a subscriber that receives only the named
// event types. An empty/nil topics slice means "all events" (same as
// AddClient). Blank entries are ignored. The always-on connected/heartbeat
// frames are written directly by SSEHandler and are never filtered here.
func (h *EventHub) AddClientFiltered(topics []string) chan SSEEvent {
	ch := make(chan SSEEvent, 64)
	var set map[string]bool
	for _, t := range topics {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		if set == nil {
			set = make(map[string]bool, len(topics))
		}
		set[t] = true
	}
	h.mu.Lock()
	h.clients[ch] = set
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
		payload := map[string]any{"type": "status_changed", "order_id": ev.OrderID, "new_status": ev.NewStatus}
		h.Broadcast("order-update", sseJSON(payload))
		if ev.NewStatus == "in_transit" {
			go func(orderID int64) {
				if order, err := eng.DB().GetOrder(orderID); err == nil && order != nil && string(order.Status) == "in_transit" {
					if etaStr := eta.Stamp(eng.EtaCache(), order.SourceNode, order.DeliveryNode); etaStr != "" {
						h.Broadcast("order-update", sseJSON(map[string]any{"type": "eta_update", "order_id": orderID, "eta": etaStr}))
					}
				}
			}(ev.OrderID)
		}
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
		ev := evt.Payload.(engine.OrderSkippedEvent)
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "skipped", "order_id": ev.OrderID, "detail": ev.Detail}))
	}, engine.EventOrderSkipped)

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

	// Production heartbeat (Phase E): each projected tick pulses the Cells D
	// section and the /heartbeat kiosk. station + process_id let the client
	// match the tick to a cell_config row; ts is server time so a long-running
	// kiosk renders "X ago" without clock drift.
	eng.Events.SubscribeTypes(func(evt engine.Event) {
		ev := evt.Payload.(engine.CellTickEvent)
		h.Broadcast("cell-heartbeat", sseJSON(map[string]any{
			"station":     ev.Station,
			"process_id":  ev.ProcessID,
			"style_id":    ev.StyleID,
			"recorded_at": ev.RecordedAt.UTC().Format(time.RFC3339Nano),
			// ts is the SIM clock, not wall — the kiosk syncs serverNow() to it and
			// windows fires by recorded_at (also sim-stamped). Under fast-forward the
			// sim clock runs days behind wall; a wall ts would put serverNow() ahead of
			// every back-dated fire, so the 60s strip window would always read empty
			// ("No data"). clock.Now() == time.Now() in production, so live is unchanged.
			"ts": clock.Now().UTC().Format(time.RFC3339Nano),
		}))
	}, engine.EventCellTick)
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

	// Optional ?topics=a,b,c narrows this client to the listed event types
	// (plan §6 SSE bus). Absent → unfiltered, matching legacy behavior.
	var ch chan SSEEvent
	if topicsParam := r.URL.Query().Get("topics"); topicsParam != "" {
		ch = h.AddClientFiltered(strings.Split(topicsParam, ","))
	} else {
		ch = h.AddClient()
	}
	defer h.RemoveClient(ch)

	// Send connected event with the per-process build id so reconnects
	// after a core restart trigger a hard-reload on the client. ts is the SIM
	// clock — the /heartbeat kiosk (§13) syncs its clock offset from it so its
	// 60s strip window aligns with the (sim-stamped) fires under fast-forward,
	// and "X ago" timers don't drift over a 72h soak. clock.Now()==time.Now() in prod.
	if _, err := fmt.Fprintf(w, "event: connected\ndata: {\"build\":\"%s\",\"ts\":\"%s\"}\n\n", serverInstance, clock.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return
	}
	flusher.Flush()

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
			// Named heartbeat event carries the build id on the existing
			// connection — mid-stream version comparison without reconnect.
			// The bare `: keepalive` comment was stripped by EventSource
			// and never reached the JS client, so it could not carry the
			// build id.
			if _, err := fmt.Fprintf(w, "event: heartbeat\ndata: {\"build\":\"%s\",\"ts\":\"%s\"}\n\n", serverInstance, clock.Now().UTC().Format(time.RFC3339Nano)); err != nil {
				log.Printf("sse: keepalive write error: %v", err)
				return
			}
			flusher.Flush()
		}
	}
}
