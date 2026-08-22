package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/eventbus"
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

// coalescingSSETopics names the events whose payload is a COMPLETE snapshot,
// so a newer frame wholly supersedes an older one and only the newest needs
// to reach the client.
//
// These arrive far more often than everything else combined (robot-update is
// ~97% of all frames), and before the split they shared one fixed-size queue
// with the state-change events. A client that fell behind therefore lost
// whatever happened to arrive next — including order-update, which the orders
// board is edge-triggered on (hx-trigger sse:order-update). A dropped
// order-update leaves that board silently stale, with no retry and no signal:
// telemetry nobody would miss was evicting the frames the operator acts on.
//
// Only add a topic here when a newer frame makes the older one worthless.
// Per-entity events do NOT qualify even when they are frequent: coalescing
// keys on the event name alone, so a per-entity topic would let one entity's
// frame discard another's. That is why plc-status (keyed by plcName) and
// system-status stay durable.
var coalescingSSETopics = map[string]bool{
	// RobotsUpdatedEvent carries every robot's full state (position, battery,
	// blocked, error). The next poll re-sends all of it, so a superseded frame
	// has no value.
	"robot-update": true,
}

// sseClient is one subscriber. It holds two independent queues so a burst on
// one class can never consume the other's capacity.
type sseClient struct {
	// durable carries state changes. Nothing coalescing is written here, so
	// its capacity is reserved for frames whose loss is observable.
	durable chan SSEEvent
	// topics is the client's event-name filter; nil means "all events".
	topics map[string]bool

	mu sync.Mutex
	// coalesced holds the newest pending frame per coalescing topic. Writing
	// an entry that already exists overwrites it, which is the coalescing.
	coalesced map[string]SSEEvent
	// wake has capacity 1 and signals "coalesced is non-empty". A signal
	// already pending needs no second one — the reader drains the whole map.
	wake chan struct{}
}

type EventHub struct {
	mu sync.RWMutex
	// clients holds every subscriber. Each carries its own topic filter: a
	// set of event names the client wants, nil meaning "all events". Topic
	// filtering lets the dashboard SSE bus (shared/utils.js onSSE) request
	// only the event types a tab subscribed to via /events?topics=… so a
	// /missions admin tab never receives the per-pulse cell-heartbeat
	// firehose (plan §6).
	clients   map[*sseClient]struct{}
	broadcast chan SSEEvent
	stopChan  chan struct{}
	stopOnce  sync.Once
}

func NewEventHub() *EventHub {
	return &EventHub{
		clients:   make(map[*sseClient]struct{}),
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
			coalescing := coalescingSSETopics[evt.Event]
			h.mu.RLock()
			for c := range h.clients {
				if c.topics != nil && !c.topics[evt.Event] {
					continue // client filtered this event type out
				}
				if coalescing {
					c.offerCoalesced(evt)
					continue
				}
				select {
				case c.durable <- evt:
				default:
					// The durable queue is full of state changes the client
					// has not read. Nothing coalescing can land here, so this
					// is genuine backpressure rather than telemetry crowding.
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

// offerCoalesced stores evt as the newest pending frame for its topic,
// replacing any earlier one, and signals the reader. It never blocks and
// never drops: a superseded snapshot is not a loss, and an unread signal
// already covers whatever else is waiting in the map.
func (c *sseClient) offerCoalesced(evt SSEEvent) {
	c.mu.Lock()
	c.coalesced[evt.Event] = evt
	c.mu.Unlock()
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// takeCoalesced removes and returns every pending coalesced frame.
func (c *sseClient) takeCoalesced() []SSEEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.coalesced) == 0 {
		return nil
	}
	out := make([]SSEEvent, 0, len(c.coalesced))
	for _, evt := range c.coalesced {
		out = append(out, evt)
	}
	clear(c.coalesced)
	return out
}

// AddClient registers an unfiltered subscriber that receives every event.
func (h *EventHub) AddClient() *sseClient {
	return h.AddClientFiltered(nil)
}

// AddClientFiltered registers a subscriber that receives only the named
// event types. An empty/nil topics slice means "all events" (same as
// AddClient). Blank entries are ignored. The always-on connected/heartbeat
// frames are written directly by SSEHandler and are never filtered here.
func (h *EventHub) AddClientFiltered(topics []string) *sseClient {
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
	c := &sseClient{
		durable:   make(chan SSEEvent, 64),
		topics:    set,
		coalesced: make(map[string]SSEEvent),
		wake:      make(chan struct{}, 1),
	}
	h.mu.Lock()
	h.clients[c] = struct{}{}
	h.mu.Unlock()
	return c
}

// RemoveClient unregisters c and closes its durable queue. The close is
// done under the same lock the fan-out holds while sending, so a send can
// never race a close. The coalesced side needs no close — its reader only
// ever wakes on a signal the fan-out no longer sends.
func (h *EventHub) RemoveClient(c *sseClient) {
	h.mu.Lock()
	_, ok := h.clients[c]
	delete(h.clients, c)
	h.mu.Unlock()
	if ok {
		close(c.durable)
	}
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
//
// Payload-carrying handlers use eventbus.SubscribeTyped, which binds the
// concrete event struct at the call site: the compiler checks the closure
// against that payload type, and a mis-emitted payload is skipped with a
// logged warning instead of the panic the bare evt.Payload.(T) assertion
// used to raise. The six fleet/messaging/database system-status handlers
// stay on plain SubscribeTypes — they broadcast a constant and never read a
// payload, so there is no type to bind.
func (h *EventHub) SetupEngineListeners(eng *engine.Engine) {
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderReceivedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "received", "order_id": ev.OrderID}))
	}, engine.EventOrderReceived)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderDispatchedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "dispatched", "order_id": ev.OrderID, "vendor_order_id": ev.VendorOrderID}))
	}, engine.EventOrderDispatched)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderStatusChangedEvent]) {
		ev := evt.Payload
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
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderStatusChangedEvent]) {
		ev := evt.Payload
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

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderCompletedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "completed", "order_id": ev.OrderID}))
	}, engine.EventOrderCompleted)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderFailedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "failed", "order_id": ev.OrderID, "detail": ev.Detail}))
	}, engine.EventOrderFailed)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderCancelledEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "cancelled", "order_id": ev.OrderID, "reason": ev.Reason}))
	}, engine.EventOrderCancelled)

	// Faulted and recovered, mirroring failed above. Without these the board
	// learned about a fault only from its periodic refresh, which is the one
	// status where a row's own clock is the point.
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderFaultedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "faulted", "order_id": ev.OrderID, "detail": ev.Reason}))
	}, engine.EventOrderFaulted)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderFaultedRecoveredEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "recovered", "order_id": ev.OrderID, "robot_id": ev.RobotID}))
	}, engine.EventOrderFaultedRecovered)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderSkippedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "skipped", "order_id": ev.OrderID, "detail": ev.Detail}))
	}, engine.EventOrderSkipped)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.OrderQueuedEvent]) {
		ev := evt.Payload
		h.Broadcast("order-update", sseJSON(map[string]any{"type": "queued", "order_id": ev.OrderID, "payload_code": ev.PayloadCode}))
	}, engine.EventOrderQueued)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.BinUpdatedEvent]) {
		ev := evt.Payload
		h.Broadcast("bin-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": ev.Action, "bin_id": ev.BinID, "actor": ev.Actor, "detail": ev.Detail}))
	}, engine.EventBinUpdated)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.CorrectionAppliedEvent]) {
		ev := evt.Payload
		h.Broadcast("inventory-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": "correction", "type": ev.CorrectionType}))
	}, engine.EventCorrectionApplied)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.NodeUpdatedEvent]) {
		ev := evt.Payload
		h.Broadcast("node-update", sseJSON(map[string]any{"node_id": ev.NodeID, "action": ev.Action}))
	}, engine.EventNodeUpdated)

	// sourcing-update fires only when a sourceability VERDICT moved, never on a
	// plain recompute. It is what the /sourcing page refreshes on; the page used
	// to key off bin-update, which is a pool read that feeds a verdict rather
	// than the verdict itself, so a bin moving anywhere refreshed a page whose
	// content was identical.
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.SourcingUpdatedEvent]) {
		h.Broadcast("sourcing-update", sseJSON(map[string]any{"changed": evt.Payload.Changed}))
	}, engine.EventSourcingUpdated)

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

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.CMSTransactionEvent]) {
		ev := evt.Payload
		h.Broadcast("cms-transaction", sseJSON(ev.Transactions))
	}, engine.EventCMSTransaction)

	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.RobotsUpdatedEvent]) {
		ev := evt.Payload
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
			// The order this robot is on. See RobotOrderLine — no alarms.
			RobotOrderLine
		}
		// Once per broadcast, not once per robot.
		orderLines := robotOrderLines(eng.OrderService(), eng.AppConfig())
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
				RobotOrderLine: orderLines[r.VehicleID],
			}
		}
		h.Broadcast("robot-update", sseJSON(out))
	}, engine.EventRobotsUpdated)

	// Production heartbeat (Phase E): each projected tick pulses the Cells D
	// section and the /heartbeat kiosk. station + process_id let the client
	// match the tick to a cell_config row; ts is server time so a long-running
	// kiosk renders "X ago" without clock drift.
	eventbus.SubscribeTyped(eng.Events, func(evt eventbus.TypedEvent[engine.EventType, engine.CellTickEvent]) {
		ev := evt.Payload
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
	var c *sseClient
	if topicsParam := r.URL.Query().Get("topics"); topicsParam != "" {
		c = h.AddClientFiltered(strings.Split(topicsParam, ","))
	} else {
		c = h.AddClient()
	}
	defer h.RemoveClient(c)

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
		case evt := <-c.durable:
			if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Event, evt.Data); err != nil {
				log.Printf("sse: write error: %v", err)
				return
			}
			flusher.Flush()
		case <-c.wake:
			// Snapshot frames. Ordering against the durable stream is not
			// preserved across the two queues, which is fine: each event type
			// is handled by its own client-side listener, and ordering WITHIN
			// a type still holds.
			for _, evt := range c.takeCoalesced() {
				if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evt.Event, evt.Data); err != nil {
					log.Printf("sse: write error: %v", err)
					return
				}
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
