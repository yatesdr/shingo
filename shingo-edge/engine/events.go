package engine

import (
	"shingo/protocol"
	"shingo/protocol/eventbus"
	"shingoedge/domain"
	"shingoedge/plc"
)

// EventType identifies the kind of event emitted by the Engine.
type EventType int

const (
	// Counter events
	EventCounterRead EventType = iota + 1
	EventCounterDelta
	EventCounterReadError
	// EventLineCounterDelta is a station counter's counted strokes
	// (LineCounterDeltaEvent): the positions bound to it are charged. It runs
	// no hourly, snapshot or Core path — the production counter is the record.
	EventLineCounterDelta

	// Order events
	EventOrderCreated
	EventOrderStatusChanged
	EventOrderCompleted
	EventOrderFailed

	// PLC events
	EventPLCConnected
	EventPLCDisconnected
	EventPLCHealthAlert
	EventPLCHealthRecover

	// WarLink events
	EventWarLinkConnected
	EventWarLinkDisconnected

	// Core node sync events
	EventCoreNodesUpdated
	EventOrderFaulted

	// EventOrderDelivered fires the moment an order transitions to
	// StatusDelivered — i.e., the bin physically arrived at its
	// destination node. The runtime UOP cache for the destination slot
	// (when DeliveryNode == process_node.CoreNodeName) flips to the
	// delivered bin's authoritative count via handleNodeOrderDelivered.
	// Distinct from EventOrderCompleted, which fires on terminal status
	// (Confirmed/Failed/Cancelled) and is reserved for operator-semantic
	// side-effects (state machine, side-cycle dispatch).
	EventOrderDelivered

	// EventUOPAdjusted fires when an admin-originated UOP adjustment
	// arrives from Core. Handled by the SSE broadcaster as a
	// counter-update event so the operator screen refreshes its view.
	// NOT subscribed in wiring.go — avoids triggering PLC
	// counter processing that EventCounterDelta would cause.
	EventUOPAdjusted

	// EventDeliveredNotBound fires when a bin arrives at one of our nodes but
	// the delivered handler could NOT bind it to the runtime — the silent
	// detachment behind the SNF3 CARRIER-0024 stranding (2026-07-24). Today
	// each such skip was a no-op; now it names the exact bin/order + node +
	// reason + the operator's front-door fix. Alarm only — it never changes
	// binding behavior. Rendered on the operator-station tile (C8) and greppable
	// in the journal via its "delivered but NOT bound:" log line.
	EventDeliveredNotBound

	// EventCATIDMismatch fires when a press's live WarLink CATID_01 (its
	// physical part identity) diverges from the active style's expected_catid
	// (A5, Hopkinsville 2026-07-23). It is the ground-truth "wrong part on the
	// press for the running style" alert — the exact condition that let LK41
	// relief fire into a physically-KK21 line. Alarm only: it never cancels or
	// re-plans anything. The blocking of outgoing-style relief is done
	// separately by guardCatidMismatch on the request path; this event just
	// surfaces the divergence, naming the press + both CATID values. Edge-
	// triggered on a debounced value change — no fixed threshold, no timer.
	EventCATIDMismatch

	// EventCATIDChangePrompt fires when a press's debounced CATID_01 CHANGES to
	// a value that no longer matches the active style (B1 prompt-arm half, hop
	// 2026-07-23). It PROMPTS the operator to start a changeover — pre-filling
	// the target style when the new part's CATID maps to a known style's
	// expected_catid — but never starts one: the operator still confirms
	// through the existing Start Changeover flow. Raised only for processes whose
	// changeover_auto_arm mode is `prompt`; `auto` processes auto-cut-over an
	// in-progress changeover instead (EventCATIDAutoCutover) and `off` processes
	// do neither.
	EventCATIDChangePrompt

	// EventCATIDAutoCutover fires when the CATID monitor presses CUTOVER on an
	// ALREADY-IN-PROGRESS changeover, because the press's live part id stably left
	// the outgoing style — evidence the press itself already cut over.
	//
	// The monitor used to START changeovers (an event, EventCATIDAutoArmed, now
	// deleted), and the inversion is the point. Operators start a changeover, the material moves,
	// and then they forget to press CUTOVER; shingo keeps attributing the new
	// parts to the old style until someone notices hours later. The monitor now
	// presses the button they forget, and never starts anything — starting
	// dispatches robots, and nothing here knows whether the material exists.
	EventCATIDAutoCutover

	// EventChangeoverVerifyMismatch fires when, within the short window after a
	// cutover completed, the press's live part id still disagrees with the new
	// active style's expected_catid. It flags that changeover for operator
	// confirmation on the station (the changeover was set to style A, but the
	// press reports a part matching style B or nothing). It never blocks beyond
	// the existing request-path mismatch guard — it is a confirmation prompt.
	EventChangeoverVerifyMismatch

	// EventContainmentUpdated fires when the Edge's held copy of Core's
	// containment feed changes digest — a snapshot applied that differs from
	// the one held — and when the copy crosses FeedAsOfAfter either way
	// (feeds.go crossingsLocked), since the page shows its "as of". The
	// containment page reloads on it; an identical re-send does not fire it.
	EventContainmentUpdated

	// EventNodeBinsUpdated fires when a node-bins snapshot from Core changed
	// the Edge's copy: it names the nodes whose row or assignment changed and
	// the groups whose wants changed. An identical re-send does not fire it.
	EventNodeBinsUpdated

	// EventSupplyRefusalChanged fires when this Edge's supply refusal table
	// changed: a refusal, an undo, an answer, the clear a LOAD makes, a refusal
	// broadcast from another Edge applied, or Core's snapshot applied (which
	// names no card). A station board draws refusals at its loader windows and
	// on the cells waiting on the part, and rebuilds on it.
	EventSupplyRefusalChanged

	// EventSourcingStateChanged fires on every sourcing report from Core once
	// it is stored, the periodic full one included, naming the processes it
	// carries. Nothing is read to tell whether a verdict moved.
	EventSourcingStateChanged

	// EventBoardNudge fires when something a station view reads changed and
	// no scoped event says so. Plant-wide inputs (Core's loader list and
	// payload catalog applied, a watched curtain tag's reading moved, a
	// keep-staged sentence changed, a feed crossing its as-of age) carry no
	// payload: the SSE hub turns them into its one coalesced nudge (www
	// EventHub.Nudge), which every station takes. An order's writes (an order
	// update's last write, a projected order) carry a BoardNudgeEvent naming
	// the order and its process node: the hub sends it as an order-update,
	// which only a board drawing that node or that order rebuilds on
	// (operator-state.js eventConcerns), the answer every order-update gets.
	EventBoardNudge

	// EventOrderCancelledByCore fires when Core cancels an order that was still
	// live on this Edge — one this station did not ask to cancel. The fleet
	// stopping it, an admin terminating it on Core, a swap partner's death:
	// Core's reason rides along, and the station board toasts it (SSE
	// "order-cancelled"). A cancel this station sent is already terminal here
	// when Core's echo arrives (orders.Manager.AbortOrderWithReason transitions
	// locally first), so it never fires this.
	EventOrderCancelledByCore
	// EventCATIDHealthChanged hints that a process's part-identity health
	// items may have moved (a confirm edge, a switch or cutover, a start, the
	// post-cutover flag changing). Pages refetch; the items are computed from
	// state on each read, so a missed hint costs a refresh, never a wrong item.
	EventCATIDHealthChanged

	// EventChangeoverVerifyCleared fires when a standing post-cutover flag
	// clears, by whatever path (ClearPostCutoverFlag): the press now reports a
	// part of the style it was set to, the station's Dismiss or Update CATID, a
	// new changeover, a switch. Every station of the process takes the banner
	// down (SSE "changeover-verify-cleared").
	EventChangeoverVerifyCleared
)

// Event is the envelope emitted by the Engine's EventBus.
type Event = eventbus.Event[EventType]

// --- Event payloads ---
//
// Each payload struct embeds eventbus.PayloadBase (zero-size marker) so
// it satisfies the sealed eventbus.Payload interface and can flow through
// SubscribeTyped / EmitTyped. Field layout is unchanged.

// CounterReadEvent is emitted on every PLC poll.
type CounterReadEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Value            int64  `json:"value"`
}

// CounterDeltaEvent is emitted when production count increases.
//
// StyleID is the ATTRIBUTED style (attributeTick): the style that runs the
// part the PLC reported, or the reporting point's when the part says nothing.
// CATID is that part ("" when none) and Kind how the style was decided.
type CounterDeltaEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64        `json:"reporting_point_id"`
	ProcessID        int64        `json:"process_id"`
	StyleID          int64        `json:"style_id"`
	CATID            string       `json:"catid,omitempty"`
	Kind             plc.TickKind `json:"kind,omitempty"`
	Delta            int64        `json:"delta"`
	NewCount         int64        `json:"new_count"`
	// Anomaly is how the counter moved: "" for normal, "jump" past the jump
	// threshold, "reset" backward (Delta is then the new count). A record
	// only; every consumer counts the delta whatever it says.
	Anomaly string `json:"anomaly"`
}

// LineCounterDeltaEvent is one station counter's counted strokes, with the
// identity the poll attributed them (plc.EventEmitter.AttributeTick against
// the process's active style).
type LineCounterDeltaEvent struct {
	eventbus.PayloadBase
	CounterID int64        `json:"counter_id"`
	ProcessID int64        `json:"process_id"`
	StyleID   int64        `json:"style_id"`
	CATID     string       `json:"catid,omitempty"`
	Kind      plc.TickKind `json:"kind,omitempty"`
	Delta     int64        `json:"delta"`
	Anomaly   string       `json:"anomaly"`
}

// OrderCreatedEvent is emitted when a new order is placed.
type OrderCreatedEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// OrderStatusChangedEvent is emitted on order state transitions.
type OrderStatusChangedEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	OldStatus     string             `json:"old_status"`
	NewStatus     string             `json:"new_status"`
	ETA           string             `json:"eta"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// OrderCompletedEvent is emitted when an order reaches terminal state.
type OrderCompletedEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
}

// PLCEvent is emitted for PLC connection state changes.
type PLCEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthAlertEvent is emitted when a PLC goes offline.
type PLCHealthAlertEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
	Error   string `json:"error,omitempty"`
}

// PLCHealthRecoverEvent is emitted when a PLC comes back online.
type PLCHealthRecoverEvent struct {
	eventbus.PayloadBase
	PLCName string `json:"plc_name"`
}

// WarLinkEvent is emitted when the WarLink connection state changes.
type WarLinkEvent struct {
	eventbus.PayloadBase
	Connected bool   `json:"connected"`
	Error     string `json:"error,omitempty"`
}

// BoardNudgeEvent is an EventBoardNudge about one order: the order and the
// process node it is at (nil when it is at none, which every board takes).
// Its JSON is an order-update's keys, so the board judges it as one.
type BoardNudgeEvent struct {
	eventbus.PayloadBase
	OrderID       int64  `json:"order_id,omitempty"`
	OrderUUID     string `json:"order_uuid"`
	ProcessNodeID *int64 `json:"process_node_id,omitempty"`
}

// ContainmentUpdatedEvent names the digest of the containment copy just held.
type ContainmentUpdatedEvent struct {
	eventbus.PayloadBase
	Digest string `json:"digest"`
}

// NodeBinsUpdatedEvent names what a node-bins snapshot changed in the copy:
// the nodes whose row or assignment moved, and the groups whose wants moved.
// A station refetches its view when it draws one of them.
type NodeBinsUpdatedEvent struct {
	eventbus.PayloadBase
	Nodes  []string `json:"nodes"`
	Groups []string `json:"groups"`
}

// SupplyRefusalChangedEvent names the card whose refusal changed: the loader
// window and the part. Both blank when Core's snapshot was applied, which may
// have changed any card.
type SupplyRefusalChangedEvent struct {
	eventbus.PayloadBase
	LoaderNode  string `json:"loader_node"`
	PayloadCode string `json:"payload_code"`
}

// SourcingStateChangedEvent names the processes a stored sourcing report
// carried (their names, the feed's process key), each once; none for a
// snapshot that carried no state.
type SourcingStateChangedEvent struct {
	eventbus.PayloadBase
	Processes []string `json:"processes"`
}

// CoreNodesUpdatedEvent is emitted when the core node list is received.
type CoreNodesUpdatedEvent struct {
	eventbus.PayloadBase
	Nodes []protocol.NodeInfo `json:"nodes"`
}

// CounterReadErrorEvent is emitted when a tag read fails.
type CounterReadErrorEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	Error            string `json:"error"`
}

// OrderFailedEvent is emitted when an order transitions to failed state.
type OrderFailedEvent struct {
	eventbus.PayloadBase
	OrderID   int64              `json:"order_id"`
	OrderUUID string             `json:"order_uuid"`
	OrderType protocol.OrderType `json:"order_type"`
	Reason    string             `json:"reason"`
}

// OrderCancelledByCoreEvent is EventOrderCancelledByCore's payload. Same shape
// as OrderFailedEvent on purpose: the station's event filter and its toast read
// order_id and reason from both. Reason is Core's OrderCancelled.Reason as sent.
type OrderCancelledByCoreEvent struct {
	eventbus.PayloadBase
	OrderID   int64              `json:"order_id"`
	OrderUUID string             `json:"order_uuid"`
	OrderType protocol.OrderType `json:"order_type"`
	Reason    string             `json:"reason"`
}

// OrderFaultedEvent is emitted when an order transitions to faulted state.
// The HMI shows an amber indicator with elapsed-time-in-state so operators
// can distinguish a brief blip from an about-to-escalate fault.
type OrderFaultedEvent struct {
	eventbus.PayloadBase
	OrderID   int64  `json:"order_id"`
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// OrderDeliveredEvent is emitted when an order transitions to
// StatusDelivered. Carries the BinID Core resolved at delivery time so
// the delivered handler can look up the bin's authoritative
// uop_remaining and bind the slot's runtime cache to it. ProcessNodeID
// is the dispatch-time process node hint — it is a TRACKING pointer, not
// a destination: removal-shaped orders (e.g. the evac leg of a two-robot
// swap) attach to the process node but deliver to the supermarket, so the
// delivered handler still has to decide whether the bin landed here.
//
// It decides that from the order's STEPS for a complex order —
// finalDropoffNode(steps_json), the last dropoff, which is where a
// single-bin order's one bin comes to rest — and from delivery_node for a
// simple order, where that column is unambiguous. It does NOT compare
// delivery_node for complex orders: they have many dropoffs, so one
// per-order destination field cannot say where any particular bin ended up,
// and the swap legs stamp it with values that name neither (HK 2026-07-14).
type OrderDeliveredEvent struct {
	eventbus.PayloadBase
	OrderID       int64              `json:"order_id"`
	OrderUUID     string             `json:"order_uuid"`
	OrderType     protocol.OrderType `json:"order_type"`
	ProcessNodeID *int64             `json:"process_node_id,omitempty"`
	BinID         *int64             `json:"bin_id,omitempty"`
	// BinUOP and BinEpoch are the arrived bin's authoritative count and
	// load-lifecycle epoch, carried from the OrderDelivered Kafka envelope
	// (Core's snapshot at delivery). handleNodeOrderDelivered seeds the
	// runtime cache + active_bin_epoch from these — no HTTP pull. BinUOP
	// nil = no count was sent: an older Core, or a current one that named a bin
	// and could not read its row. The Edge seats 0 and logs it (BlindDeliveryMarker)
	// rather than assuming a full carrier.
	BinUOP *int `json:"bin_uop,omitempty"`
	// BinPayloadCode is what the arriving carrier IS, off the same bin row as
	// BinUOP and BinEpoch. It is the lineside identity — the answer this side
	// used to infer from the requested style's claim, which is right only
	// while the two agree. nil means an older Core did not send it, which is
	// not the same as an empty carrier ("").
	BinPayloadCode *string `json:"bin_payload_code,omitempty"`
	BinEpoch       int64   `json:"bin_epoch,omitempty"`
	// DeliveryNode is the Core dot-name of the destination. Set only for
	// Core-admin (stationless) deliveries where ProcessNodeID is nil because
	// the Edge has no order row. handleNodeOrderDelivered uses it as a fallback
	// to resolve the process node when ProcessNodeID is absent.
	DeliveryNode string `json:"delivery_node,omitempty"`
	// BinDestNode is the Core dot-name of the node the carried bin came to rest
	// at — set ONLY for multi-tote deliveries (F1b), where Core already selected
	// the one bin destined for the consuming process node. When present,
	// handleNodeOrderDelivered binds iff BinDestNode == the node's CoreNodeName,
	// trusting Core's per-bin resolution instead of the steps finalDropoff (which
	// for a swap names the last leg, not where this bin landed). Empty for
	// single-bin orders — their existing delivery gate is unchanged.
	BinDestNode string `json:"bin_dest_node,omitempty"`
	// LandingSent is protocol.OrderDelivered.LandingSent: the Core announced
	// this carrier's drop on transit.bin_landed, so the delivered binds
	// nothing and only checks (Lane H, H.5). False is an older Core: the
	// carrier fields above bind through the landing verb with no placement
	// stamp, into an empty slot or onto the same bin only (H.7).
	LandingSent bool `json:"landing_sent,omitempty"`
}

// UOPAdjustedEvent is emitted when Core sends an admin-originated UOP
// adjustment. The SSE broadcaster maps this to the "counter-update" SSE
// type so the operator screen auto-refreshes.
type UOPAdjustedEvent struct {
	eventbus.PayloadBase
	ProcessNodeID int64  `json:"process_node_id"`
	CoreNodeName  string `json:"core_node_name"`
	BinID         int64  `json:"bin_id"`
	NewRemaining  int    `json:"new_remaining"`
	Actor         string `json:"actor"`
}

// CATIDMismatchEvent names the press and both part-identity values when the
// live CATID_01 diverges from the active style's expected_catid. Rendered on
// the operator station and greppable in the journal via its "CATID mismatch:"
// log line. Purely informational — the relief block is enforced on the request
// path by guardCatidMismatch.
//
// ChangeoverToStyle is set when the live part is one the open changeover's
// target runs: the program changed ahead of the cutover, as it does at every
// planned changeover. The station shows a plain "on <style>, waiting on …"
// notice for it instead of the red wrong-part alert; WaitingOn is the gate's
// blockers at the confirm.
type CATIDMismatchEvent struct {
	eventbus.PayloadBase
	ProcessID         int64    `json:"process_id"`
	ProcessName       string   `json:"process_name"`
	PLCName           string   `json:"plc_name"`
	StyleID           int64    `json:"style_id"`
	StyleName         string   `json:"style_name"`
	LiveCATID         string   `json:"live_catid"`
	ExpectedCATID     string   `json:"expected_catid"`
	ChangeoverToStyle string   `json:"changeover_to_style,omitempty"`
	WaitingOn         []string `json:"waiting_on,omitempty"`
}

// CATIDChangePromptEvent asks the operator to start a changeover after the
// press's part physically changed. TargetStyleID/Name are pre-filled when the
// new CATID maps to exactly one known style's expected_catid (HasTarget=true);
// otherwise the operator picks the target manually. This is a prompt, not an
// arm — nothing acts on it until the operator confirms Start Changeover.
type CATIDChangePromptEvent struct {
	eventbus.PayloadBase
	ProcessID   int64  `json:"process_id"`
	ProcessName string `json:"process_name"`
	PLCName     string `json:"plc_name"`
	NewCATID    string `json:"new_catid"`
	// HasTarget is true only when the new part maps to EXACTLY ONE style, so the
	// prompt can pre-fill it. When the part is ambiguous (in more than one style's
	// set) HasTarget is false and Candidates names them for the operator to pick.
	HasTarget       bool             `json:"has_target"`
	TargetStyleID   int64            `json:"target_style_id"`
	TargetStyleName string           `json:"target_style_name"`
	Candidates      []CATIDCandidate `json:"candidates,omitempty"`
}

// CATIDCandidate is one style a live CATID maps to (via its part-identity set).
type CATIDCandidate struct {
	StyleID   int64  `json:"style_id"`
	StyleName string `json:"style_name"`
}

// CATIDAutoCutoverEvent reports that the monitor pressed CUTOVER on an
// in-progress changeover because the press's live part id had stably left the
// outgoing style — i.e. the press had already cut over and the operator had not
// told shingo yet. Carries no style: whether the new active style actually
// matches the press is decided afterwards by the post-cutover verification
// watch, which raises its own flag when it does not.
//
// Forced and Blockers are the record of a completion past a red gate: the
// blockers the door overrode (also on the row's notes). Forced is false and
// Blockers empty for a gated completion.
type CATIDAutoCutoverEvent struct {
	eventbus.PayloadBase
	ProcessID    int64            `json:"process_id"`
	ProcessName  string           `json:"process_name"`
	ChangeoverID int64            `json:"changeover_id"`
	NewCATID     string           `json:"new_catid"`
	Forced       bool             `json:"forced"`
	Blockers     []domain.Blocker `json:"blockers,omitempty"`
}

// CATIDVerifyMismatchEvent carries a post-cutover verification flag: after the
// changeover to StyleID completed, the press's live part id (LiveCATID) still
// did not match its ExpectedCATID within the watch window. The station renders
// it as a "please confirm" prompt on ChangeoverID.
type CATIDVerifyMismatchEvent struct {
	eventbus.PayloadBase
	ProcessID     int64  `json:"process_id"`
	ProcessName   string `json:"process_name"`
	ChangeoverID  int64  `json:"changeover_id"`
	StyleID       int64  `json:"style_id"`
	ExpectedCATID string `json:"expected_catid"`
	LiveCATID     string `json:"live_catid"`
}

// CATIDVerifyClearedEvent names the process whose post-cutover flag (on
// ChangeoverID) cleared.
type CATIDVerifyClearedEvent struct {
	ProcessID    int64 `json:"process_id"`
	ChangeoverID int64 `json:"changeover_id"`
}

// DeliveredNotBoundEvent carries the detail for a delivery that arrived at one
// of our nodes but did not bind the runtime. BinID is nil for a multi-bin
// delivery (the envelope carried no per-bin id, F1b); otherwise it names the
// exact carrier. Instruction is the operator's front-door correction, worded
// so the tile (C8) can render it verbatim.
type DeliveredNotBoundEvent struct {
	eventbus.PayloadBase
	OrderID      int64  `json:"order_id"`
	OrderUUID    string `json:"order_uuid"`
	CoreNodeName string `json:"core_node_name"`
	BinID        *int64 `json:"bin_id,omitempty"`
	Reason       string `json:"reason"`
	Instruction  string `json:"instruction"`
}
