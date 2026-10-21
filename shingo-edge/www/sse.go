package www

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol/lockcheck"
	"shingoedge/engine"
	"shingoedge/internal/lockorder"
)

// SSEEvent is the typed envelope sent to SSE clients.
type SSEEvent struct {
	Type string `json:"type"`
	Data any    `json:"data"`
	// seq is the hub's number for this frame, assigned at fan-out and written
	// as the frame's SSE id. Zero until the hub fans it out.
	seq int64
}

// serverInstance is a per-process identifier emitted on every SSE
// connect. The HMI compares it across reconnects: if it changes, the
// edge has been restarted (likely with a new JS bundle) and the tab
// hard-reloads. Without this, a long-lived operator-station tab keeps
// the previously-cached ES module graph forever: the static files' ETags
// change with a restart (serverInstanceETag), but only a fresh page load
// asks for them again.
var serverInstance = fmt.Sprintf("%x", time.Now().UnixNano())

// sseKeepaliveInterval is the shipped heartbeat tick. NewEventHub copies it
// into EventHub.keepalive; a test that wants a short tick sets that field on
// its own hub before serving, rather than writing a package var other tests
// read.
const sseKeepaliveInterval = 30 * time.Second

// sseNudgeTopic is the frame EventHub.Nudge sends: "something a board draws
// may have changed; refetch if you care". Every station takes it
// (operator-state.js eventConcerns). It is material-refresh, the type the
// boards already took for "every station", rather than a second name for the
// same instruction.
const sseNudgeTopic = "material-refresh"

// sseNudgeWindow is how long a nudge waits for the rest of its burst: a
// preset applied to forty parts is forty saves, and one nudge for all of them.
const sseNudgeWindow = 250 * time.Millisecond

// lossySSETopics names the high-rate diagnostic feeds. They are delivered
// best-effort on a separate queue so a burst of them can never consume the
// capacity reserved for state changes.
//
// Before the split every topic shared one 64-slot queue and the fan-out
// dropped whatever arrived once it was full. These two are the bulk of the
// traffic, so what they evicted was everything else — including order-update,
// which the operator board rebuilds on at once. The board does not poll: a
// durable frame a client cannot take evicts the client (fanOut), and its
// reconnect refetches. Losing one of these costs a gap in a diagnostic view.
//
// Both are safe to shed because neither is authoritative:
//   - counter-read is the raw PLC poll reading. The count the HMI acts on
//     arrives as counter-update (EventCounterDelta / EventUOPAdjusted), which
//     stays on the durable queue.
//   - debug-log is the debug-console feed, wired from dbg.SetOnEntry in
//     router.go. It reaches only a client that asked for it (sseDebugTopic);
//     with --log-debug on it is pure volume, and logging should not be able
//     to degrade the operator UI.
//
// Do NOT add a state change here to quieten a drop log. Anything the operator
// or the engine acts on belongs on the durable queue, including the per-PLC
// plc-status and the plc-health-alert / plc-health-recover pair.
var lossySSETopics = map[string]bool{
	"counter-read": true,
	"debug-log":    true,
}

// sseDebugTopic is delivered only to a client that connects with ?debug=1
// (the Diagnostics page). Before, every /events client got every debug line:
// Production, the station boards and Manual Order received a stream none of
// them renders, for as long as the page stayed open.
const sseDebugTopic = "debug-log"

type sseClient struct {
	// events carries state changes. Nothing lossy is written here, so its
	// capacity is reserved for frames whose loss is observable.
	events chan SSEEvent
	// lossy carries lossySSETopics, best-effort. Shallower than events on
	// purpose: a client this far behind on diagnostics gains nothing from a
	// deeper backlog of them.
	lossy chan SSEEvent
	// debug is set when the client asked for sseDebugTopic (?debug=1).
	debug bool
	// topics is what the client asked for (?topics=a,b); nil is every topic.
	topics map[string]bool
	// queued is the seq of the last durable frame queued on events for this
	// client. Stored only after the send succeeded, so every frame up to it
	// is already in the channel; the heartbeat carries it, and a client that
	// saw a lower id knows it lost one.
	queued atomic.Int64
}

// wants reports whether the client takes frames of this type.
func (c *sseClient) wants(typ string) bool {
	if typ == sseDebugTopic && !c.debug {
		return false
	}
	return c.topics == nil || c.topics[typ]
}

// parseTopics reads ?topics=a,b into a set, nil when absent or blank: the
// shared bus (shared/utils.js onSSE) asks for exactly the types it listens to.
func parseTopics(raw string) map[string]bool {
	var out map[string]bool
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			if out == nil {
				out = map[string]bool{}
			}
			out[t] = true
		}
	}
	return out
}

// MaxSSEClients caps concurrent SSE connections to prevent a
// misbehaving browser / scraper from growing the clients map
// unboundedly.
const MaxSSEClients = 256

// EventHub manages SSE client connections and broadcasts.
type EventHub struct {
	mu        lockcheck.RWMutex[lockorder.SSEHub]
	clients   map[*sseClient]struct{}
	broadcast chan SSEEvent
	stopChan  chan struct{}
	stopOnce  sync.Once

	// seq numbers every frame the hub fans out. Taken under mu's read lock in
	// fanOut and read under the write lock in register, so a client registered
	// between two frames starts exactly between their numbers.
	seq atomic.Int64
	// overflow is rung by Broadcast when the buffer could not take a durable
	// frame; run evicts every client, whose reconnects refetch.
	overflow chan struct{}
	// nudge is nudgeLoop's doorbell (Nudge).
	nudge chan struct{}

	// keepalive and nudgeWindow are set by NewEventHub; a test may shorten
	// them on its own hub before Start.
	keepalive   time.Duration
	nudgeWindow time.Duration
	// toucher marks a station's screen online (StationService.Touch). Set
	// once, before serving (SetStationToucher).
	toucher func(stationID int64)
}

// NewEventHub creates a new EventHub.
func NewEventHub() *EventHub {
	return &EventHub{
		clients:     make(map[*sseClient]struct{}),
		broadcast:   make(chan SSEEvent, 256),
		stopChan:    make(chan struct{}),
		overflow:    make(chan struct{}, 1),
		nudge:       make(chan struct{}, 1),
		keepalive:   sseKeepaliveInterval,
		nudgeWindow: sseNudgeWindow,
	}
}

// SetStationToucher wires the station "screen online" write. A stream that
// names its station (?station=<id>) touches it on connect and on every
// heartbeat: with no view poll, the stream is what says a screen is alive.
// Call before serving.
func (h *EventHub) SetStationToucher(fn func(stationID int64)) { h.toucher = fn }

// Start begins the event fan-out loop and the nudge coalescer.
func (h *EventHub) Start() {
	go h.run()
	go h.nudgeLoop()
}

// Nudge asks for one sseNudgeTopic frame. Non-blocking and coalesced: every
// nudge that lands before the pending one is sent rides it. The frame is
// broadcast after the call, so it always follows the write that asked.
func (h *EventHub) Nudge() {
	select {
	case h.nudge <- struct{}{}:
	default:
	}
}

// nudgeLoop waits out each burst of nudges and broadcasts one frame for it.
func (h *EventHub) nudgeLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC sse-nudgeLoop: %v\n%s", r, debug.Stack())
		}
	}()
	for {
		select {
		case <-h.stopChan:
			return
		case <-h.nudge:
			if !h.waitOutNudgeBurst() {
				return
			}
			h.Broadcast(SSEEvent{Type: sseNudgeTopic, Data: map[string]string{"action": "nudge"}})
		}
	}
}

// waitOutNudgeBurst absorbs nudges for the window. False when the hub stopped.
func (h *EventHub) waitOutNudgeBurst() bool {
	timer := time.NewTimer(h.nudgeWindow)
	defer timer.Stop()
	for {
		select {
		case <-h.stopChan:
			return false
		case <-h.nudge:
		case <-timer.C:
			return true
		}
	}
}

// Stop shuts down the event hub. Idempotent: the check-then-act this
// replaced (select on stopChan, close in the default arm) is not atomic, so
// two concurrent Stop calls could both observe the channel open and double
// close it, which panics. Only one call site today, so this was latent —
// core's hub already carried the sync.Once (shingo-core/www/sse.go).
func (h *EventHub) Stop() {
	h.stopOnce.Do(func() { close(h.stopChan) })
}

// Broadcast sends an event to all connected clients.
//
// A DURABLE FRAME THE BUFFER CANNOT TAKE IS NOT DROPPED SILENTLY. It used to
// be, with a log line: lost for every client, and no client could know. Now
// every client is evicted (run, on overflow) and each reconnect refetches. A
// lossy topic's overflow is still the best-effort drop it always was.
func (h *EventHub) Broadcast(evt SSEEvent) {
	select {
	case h.broadcast <- evt:
	default:
		if lossySSETopics[evt.Type] {
			return
		}
		log.Printf("sse: broadcast buffer full, lost %s event: evicting every client", evt.Type)
		select {
		case h.overflow <- struct{}{}:
		default:
		}
	}
}

// register adds a client and returns the seq it starts at: every frame
// numbered above it is queued for the client, every frame at or below it
// went out before it registered.
func (h *EventHub) register(c *sseClient) int64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	start := h.seq.Load()
	c.queued.Store(start)
	h.clients[c] = struct{}{}
	return start
}

func (h *EventHub) unregister(c *sseClient) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, ok := h.clients[c]; !ok {
		// Already unregistered — e.g. run() evicted a stuck client and then
		// HandleSSE's deferred unregister also fires when its read sees the
		// closed channel. close() is not idempotent, so guard it: without this,
		// the second call panics with "close of closed channel".
		return
	}
	delete(h.clients, c)
	close(c.events)
	close(c.lossy)
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
			h.fanOut(evt)
		case <-h.overflow:
			h.evictAll()
		}
	}
}

// fanOut numbers one frame and queues it for every client that wants it.
//
// A CLIENT WHOSE QUEUE IS FULL IS EVICTED ON ITS FIRST DURABLE DROP. It used to
// stay connected through ten dropped frames, each a silent loss to a board
// that rebuilds only on events. Evicted, its stream ends, the bus reconnects
// and the board refetches on the connect. Targets are collected under RLock
// and evicted under Lock outside the loop (no upgrade-during-iterate on
// h.clients).
func (h *EventHub) fanOut(evt SSEEvent) {
	h.mu.RLock()
	evt.seq = h.seq.Add(1)
	var stuck []*sseClient
	lossy := lossySSETopics[evt.Type]
	for c := range h.clients {
		if !c.wants(evt.Type) {
			continue
		}
		if lossy {
			// Best-effort, and deliberately not logged: these are the bulk
			// of the traffic, so a line per drop is itself noise. They do not
			// evict either (falling behind on diagnostics is not being
			// stuck), and they do not move queued, so the heartbeat never
			// reports one as lost.
			select {
			case c.lossy <- evt:
			default:
			}
			continue
		}
		select {
		case c.events <- evt:
			c.queued.Store(evt.seq)
		default:
			stuck = append(stuck, c)
		}
	}
	h.mu.RUnlock()
	for _, c := range stuck {
		log.Printf("WARNING sse: evicting a client that could not take %s (frame %d): its reconnect refetches", evt.Type, evt.seq)
		h.unregister(c)
	}
}

// evictAll ends every stream, after the hub's own buffer lost a durable frame.
func (h *EventHub) evictAll() {
	h.mu.RLock()
	all := make([]*sseClient, 0, len(h.clients))
	for c := range h.clients {
		all = append(all, c)
	}
	h.mu.RUnlock()
	for _, c := range all {
		h.unregister(c)
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

	q := r.URL.Query()
	client := &sseClient{
		events: make(chan SSEEvent, 64),
		lossy:  make(chan SSEEvent, 16),
		debug:  q.Get("debug") == "1",
		topics: parseTopics(q.Get("topics")),
	}
	start := h.register(client)
	defer h.unregister(client)
	station, _ := strconv.ParseInt(q.Get("station"), 10, 64)
	h.touch(station)

	// Send connected event with the per-process build id so reconnects
	// after an edge restart trigger a hard-reload on the client, and the seq
	// the client starts at.
	fmt.Fprintf(w, "event: connected\ndata: {\"build\":\"%s\",\"seq\":%d}\n\n", serverInstance, start)
	flusher.Flush()

	keepalive := time.NewTicker(h.keepalive)
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
			writeSSEFrame(w, evt)
			flusher.Flush()
		case evt, ok := <-client.lossy:
			// Diagnostics. Ordering against the durable stream is not
			// preserved across the two queues, which is fine: each event type
			// is handled by its own client-side listener, and ordering WITHIN
			// a type still holds.
			if !ok {
				return
			}
			writeSSEFrame(w, evt)
			flusher.Flush()
		case <-keepalive.C:
			// Named heartbeat event carries the build id on the existing
			// connection — mid-stream version comparison without reconnect.
			// The bare `: keepalive` comment was stripped by EventSource and
			// never reached the JS client, so it could not carry the build id.
			//
			// It carries the seq of the last durable frame queued for this
			// client, read BEFORE the queue is drained: every frame up to it
			// is in the queue by then (fanOut stores it after the send), so
			// draining first means the heartbeat never runs ahead of a frame
			// that is merely waiting to be written. A client whose last id is
			// lower lost a frame and refetches (shared/utils.js).
			seq := client.queued.Load()
			if !drainSSEFrames(w, client.events) {
				return
			}
			fmt.Fprintf(w, "event: heartbeat\ndata: {\"build\":\"%s\",\"seq\":%d}\n\n", serverInstance, seq)
			flusher.Flush()
			h.touch(station)
		}
	}
}

// touch marks a station's screen online, when the stream named one.
func (h *EventHub) touch(station int64) {
	if station > 0 && h.toucher != nil {
		h.toucher(station)
	}
}

// writeSSEFrame writes one frame with the hub's seq as its id.
func writeSSEFrame(w http.ResponseWriter, evt SSEEvent) {
	data, err := json.Marshal(evt.Data)
	if err != nil {
		return
	}
	fmt.Fprintf(w, "event: %s\nid: %d\ndata: %s\n\n", evt.Type, evt.seq, data)
}

// drainSSEFrames writes every frame already queued, without waiting for more.
// False when the queue was closed (the client was evicted).
func drainSSEFrames(w http.ResponseWriter, events chan SSEEvent) bool {
	for {
		select {
		case evt, ok := <-events:
			if !ok {
				return false
			}
			writeSSEFrame(w, evt)
		default:
			return true
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
		case engine.EventUOPAdjusted:
			if p, ok := evt.Payload.(engine.UOPAdjustedEvent); ok {
				sseEvt = SSEEvent{Type: "counter-update", Data: p}
			}
		// EventCounterRead and EventCounterReadError (the raw PLC poll reading
		// and its failure) are not broadcast: no page listens for either, and
		// counter-read alone was ~2,090 frames a minute per client (LC3). The
		// engine events stay; they fall to the default arm below.
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
				sseEvt = SSEEvent{Type: "plc-status", Data: map[string]any{"plcName": p.PLCName, "connected": true}}
			}
		case engine.EventPLCDisconnected:
			if p, ok := evt.Payload.(engine.PLCEvent); ok {
				sseEvt = SSEEvent{Type: "plc-status", Data: map[string]any{"plcName": p.PLCName, "connected": false, "error": p.Error}}
			}
		case engine.EventWarLinkConnected, engine.EventWarLinkDisconnected:
			if p, ok := evt.Payload.(engine.WarLinkEvent); ok {
				sseEvt = SSEEvent{Type: "warlink-status", Data: p}
			}
		case engine.EventCoreNodesUpdated:
			if p, ok := evt.Payload.(engine.CoreNodesUpdatedEvent); ok {
				sseEvt = SSEEvent{Type: "core-nodes", Data: p}
			}
		case engine.EventUOPStranded:
			// Parked-ticks alarm (P2-C8): push a refresh so the operator tile
			// picks up the StrandedAlarm chip the moment the stranding is
			// detected, not on the next poll. The sentence itself rides the
			// station view, so the payload only needs to name the node.
			if p, ok := evt.Payload.(engine.UOPStrandedEvent); ok {
				sseEvt = SSEEvent{Type: "uop-stranded-alarm", Data: p}
			}
		case engine.EventCATIDMismatch:
			// A5: wrong physical part on a press vs the active style. Distinct
			// event so the HMI can raise a sticky station alert naming the
			// press + both CATID values (outgoing relief is already blocked).
			if p, ok := evt.Payload.(engine.CATIDMismatchEvent); ok {
				sseEvt = SSEEvent{Type: "catid-mismatch", Data: p}
			}
		case engine.EventCATIDChangePrompt:
			// B1 prompt half: the press's part changed. Prompt the operator to
			// start a changeover (target pre-filled when the new CATID maps to a
			// known style). Prompt only — the operator still confirms.
			if p, ok := evt.Payload.(engine.CATIDChangePromptEvent); ok {
				sseEvt = SSEEvent{Type: "catid-change-prompt", Data: p}
			}
		case engine.EventCATIDAutoCutover:
			// B1 auto-cutover: the monitor pressed CUTOVER on an in-progress
			// changeover because the press's part had stably left the outgoing
			// style — the press had already cut over and nobody had told shingo.
			// Notify the station so the operator sees it happened. If the press
			// turns out to be running some third style, the post-cutover
			// verification watch below raises its own flag right after this.
			if p, ok := evt.Payload.(engine.CATIDAutoCutoverEvent); ok {
				sseEvt = SSEEvent{Type: "catid-auto-cutover", Data: p}
			}
		case engine.EventChangeoverVerifyMismatch:
			// Post-cutover verification: the press's part still disagrees with the
			// style the cutover was set to. Push a refresh so the station fetches
			// and shows the "please confirm" flag with its resolutions.
			if p, ok := evt.Payload.(engine.CATIDVerifyMismatchEvent); ok {
				sseEvt = SSEEvent{Type: "changeover-verify-mismatch", Data: p}
			}
		case engine.EventContainmentUpdated:
			// The held copy of Core's containment feed changed digest. The
			// containment page reloads on it. A state change, so it rides the
			// durable queue, not lossySSETopics: a dropped frame would leave the
			// page showing the old state with nothing to correct it.
			if p, ok := evt.Payload.(engine.ContainmentUpdatedEvent); ok {
				sseEvt = SSEEvent{Type: "containment", Data: p}
			}
		case engine.EventNodeBinsUpdated:
			// The Edge's copy of Core's node-bins feed changed: the nodes and
			// groups it names. A station refetches when it draws one of them. A
			// state change, so durable like containment, not lossy.
			if p, ok := evt.Payload.(engine.NodeBinsUpdatedEvent); ok {
				sseEvt = SSEEvent{Type: "bins", Data: p}
			}
		case engine.EventSupplyRefusalChanged:
			// The refusal table changed at a card (blank names: Core's snapshot was
			// applied). A station drawing that loader window, or a cell waiting on
			// that part, rebuilds. A state change: durable.
			if p, ok := evt.Payload.(engine.SupplyRefusalChangedEvent); ok {
				sseEvt = SSEEvent{Type: "supply-refusal", Data: p}
			}
		case engine.EventSourcingStateChanged:
			// A sourcing report was stored, naming its processes. A station of a
			// named process, or serving one, rebuilds (its picker's verdicts and
			// its served cells' waiting). A state change: durable.
			if p, ok := evt.Payload.(engine.SourcingStateChangedEvent); ok {
				sseEvt = SSEEvent{Type: "sourcing", Data: p}
			}
		case engine.EventBoardNudge:
			// A station view's input changed with no scoped event of its own.
			// About one order: an order-update naming it, which only the boards
			// drawing that order or its node rebuild on (eventConcerns). Else
			// plant-wide: the coalesced nudge every station takes.
			if p, ok := evt.Payload.(engine.BoardNudgeEvent); ok {
				sseEvt = SSEEvent{Type: "order-update", Data: p}
				break
			}
			h.Nudge()
			return
		default:
			return
		}

		if sseEvt.Type != "" {
			h.Broadcast(sseEvt)
		}
	})

	log.Printf("SSE listeners wired to engine events")
}
