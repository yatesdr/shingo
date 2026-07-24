package engine

import (
	"shingo/protocol"
	"shingo/protocol/eventbus"
)

// EventType identifies the kind of event emitted by the Engine.
type EventType int

const (
	// Counter events
	EventCounterRead EventType = iota + 1
	EventCounterDelta
	EventCounterAnomaly
	EventCounterReadError

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

	// EventProducedReport fires once per produce-node tick, carrying the
	// node's resolved payload code. The production reporter subscribes to
	// this (rather than the raw EventCounterDelta) so it keys finished-good
	// counts by the catalog part code (cat_id) Core matches demands on,
	// instead of the style name. See ProducedReportEvent.
	EventProducedReport

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

	// EventUOPStranded fires when a node's pending_uop_delta has grown across
	// several consecutive flush intervals while no bin is bound — consume ticks
	// piling up unattributed because the physical carrier at the line was never
	// bound (the SNF3 parked-ticks class). It is a GROWTH condition, not a fixed
	// threshold: an idle line (pending flat) never fires. The alarm names the
	// carrier + node and carries the operator's front-door fix. Rendered on the
	// operator-station tile (P2-C8) and greppable via its "parked ticks:" log line.
	EventUOPStranded

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
	// changeover_auto_arm mode is `prompt`; `auto` processes auto-start instead
	// (EventCATIDAutoArmed) and `off` processes do neither.
	EventCATIDChangePrompt

	// EventCATIDAutoArmed fires when the CATID monitor AUTO-STARTS a changeover on
	// a stable, confirmed part change (B1 auto-arm, changeover_auto_arm=auto).
	// Sibling of EventCATIDChangePrompt: where the prompt asks the operator to act,
	// this announces the system already started the changeover to the mapped style,
	// so the HMI can raise a station notification. It never cancels or re-plans —
	// it is the notification for an authorized auto-START.
	EventCATIDAutoArmed

	// EventChangeoverVerifyMismatch fires when, within the short window after a
	// cutover completed, the press's live part id still disagrees with the new
	// active style's expected_catid. It flags that changeover for operator
	// confirmation on the station (the changeover was set to style A, but the
	// press reports a part matching style B or nothing). It never blocks beyond
	// the existing request-path mismatch guard — it is a confirmation prompt.
	EventChangeoverVerifyMismatch
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
type CounterDeltaEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	ProcessID        int64  `json:"process_id"`
	StyleID          int64  `json:"style_id"`
	Delta            int64  `json:"delta"`
	NewCount         int64  `json:"new_count"`
	Anomaly          string `json:"anomaly"` // "reset" if from a PLC counter reset, "" for normal
}

// ProducedReportEvent is emitted once per produce-node tick. PayloadCode is
// the produce node's active-claim payload — the catalog part code (cat_id) —
// resolved at the tick site where the node, and therefore the part, is
// unambiguous even for multi-part styles. The production reporter keys
// counts by this instead of the style name so they match demands.cat_id on
// Core. Mirrors the per-produce-node inventory delta emitted alongside it.
type ProducedReportEvent struct {
	eventbus.PayloadBase
	PayloadCode string `json:"payload_code"`
	Delta       int64  `json:"delta"`
}

// CounterAnomalyEvent is emitted for counter resets or jumps.
type CounterAnomalyEvent struct {
	eventbus.PayloadBase
	ReportingPointID int64  `json:"reporting_point_id"`
	SnapshotID       int64  `json:"snapshot_id"`
	PLCName          string `json:"plc_name"`
	TagName          string `json:"tag_name"`
	OldValue         int64  `json:"old_value"`
	NewValue         int64  `json:"new_value"`
	AnomalyType      string `json:"anomaly_type"` // "reset" or "jump"
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
	// nil = older Core didn't send it; fall back to the role default.
	BinUOP   *int  `json:"bin_uop,omitempty"`
	BinEpoch int64 `json:"bin_epoch,omitempty"`
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
type CATIDMismatchEvent struct {
	eventbus.PayloadBase
	ProcessID     int64  `json:"process_id"`
	ProcessName   string `json:"process_name"`
	PLCName       string `json:"plc_name"`
	StyleID       int64  `json:"style_id"`
	StyleName     string `json:"style_name"`
	LiveCATID     string `json:"live_catid"`
	ExpectedCATID string `json:"expected_catid"`
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

// CATIDAutoArmedEvent announces an auto-started changeover: the press's part
// changed to NewCATID, which maps to TargetStyle, and the monitor started the
// changeover automatically (changeover_auto_arm=auto). Purely a notification for
// the operator station — the changeover itself is already under way.
type CATIDAutoArmedEvent struct {
	eventbus.PayloadBase
	ProcessID       int64  `json:"process_id"`
	ProcessName     string `json:"process_name"`
	TargetStyleID   int64  `json:"target_style_id"`
	TargetStyleName string `json:"target_style_name"`
	NewCATID        string `json:"new_catid"`
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

// UOPStrandedEvent carries the parked-ticks alarm (P2-C7): a node whose
// pending_uop_delta kept growing across consecutive flush intervals while
// unbound. CoreNodeName names the node; Carrier is the physical bin's label
// (best-effort from Core, "" when unknown); StagedHours is how long the node has
// been stranded; PendingDelta is the held count that never landed on a bin.
// Detail is the fully-formatted operator sentence the tile renders verbatim.
type UOPStrandedEvent struct {
	eventbus.PayloadBase
	CoreNodeName string `json:"core_node_name"`
	Carrier      string `json:"carrier,omitempty"`
	StagedHours  int    `json:"staged_hours"`
	PendingDelta int    `json:"pending_delta"`
	Detail       string `json:"detail"`
}
