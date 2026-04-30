package protocol

import (
	"encoding/json"
	"time"
)

// Data is the payload for TypeData messages.
// Subject selects the sub-schema; Body carries the subject-specific data.
type Data struct {
	Subject string          `json:"subject"`
	Body    json.RawMessage `json:"data"`
}

// --- Edge lifecycle data schemas ---

// EdgeRegister is sent by an edge on startup.
type EdgeRegister struct {
	StationID string   `json:"station_id"`
	Hostname  string   `json:"hostname"`
	Version   string   `json:"version"`
	LineIDs   []string `json:"line_ids"`
}

// EdgeHeartbeat is sent periodically by an edge.
type EdgeHeartbeat struct {
	StationID string `json:"station_id"`
	Uptime    int64  `json:"uptime_s"`
	Orders    int    `json:"active_orders"`
}

// EdgeRegistered acknowledges edge registration.
type EdgeRegistered struct {
	StationID string `json:"station_id"`
	Message   string `json:"message,omitempty"`
}

// EdgeHeartbeatAck acknowledges a heartbeat.
type EdgeHeartbeatAck struct {
	StationID string    `json:"station_id"`
	ServerTS  time.Time `json:"server_ts"`
}

// --- Order payloads: Edge -> Core ---

// OrderRequest is a new transport order from edge.
type OrderRequest struct {
	OrderUUID     string `json:"order_uuid"`
	OrderType     string `json:"order_type"`
	PayloadCode   string `json:"payload_code,omitempty"`
	PayloadDesc   string `json:"payload_desc,omitempty"`
	Quantity      int64  `json:"quantity"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	SourceNode    string `json:"source_node,omitempty"`
	StagingNode   string `json:"staging_node,omitempty"`
	LoadType      string `json:"load_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	RetrieveEmpty bool   `json:"retrieve_empty,omitempty"`
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
}

// OrderCancel cancels an existing order.
type OrderCancel struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// OrderReceipt confirms delivery acceptance.
type OrderReceipt struct {
	OrderUUID   string `json:"order_uuid"`
	ReceiptType string `json:"receipt_type"`
	FinalCount  int64  `json:"final_count"`
}

// OrderRedirect changes the delivery destination.
type OrderRedirect struct {
	OrderUUID       string `json:"order_uuid"`
	NewDeliveryNode string `json:"new_delivery_node"`
}

// OrderStorageWaybill submits a store order.
type OrderStorageWaybill struct {
	OrderUUID   string `json:"order_uuid"`
	OrderType   string `json:"order_type"`
	PayloadDesc string `json:"payload_desc,omitempty"`
	SourceNode  string `json:"source_node"`
	FinalCount  int64  `json:"final_count"`
}

// --- Order payloads: Core -> Edge ---

// OrderAck confirms order acceptance.
type OrderAck struct {
	OrderUUID     string `json:"order_uuid"`
	ShingoOrderID int64  `json:"shingo_order_id"`
	SourceNode    string `json:"source_node,omitempty"`
}

// OrderWaybill assigns a robot.
type OrderWaybill struct {
	OrderUUID string `json:"order_uuid"`
	WaybillID string `json:"waybill_id"`
	RobotID   string `json:"robot_id,omitempty"`
	ETA       string `json:"eta,omitempty"`
}

// OrderUpdate provides a status change.
type OrderUpdate struct {
	OrderUUID string `json:"order_uuid"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	ETA       string `json:"eta,omitempty"`
}

// OrderDelivered signals fleet delivery complete.
//
// BinUOPRemaining snapshots bins.uop_remaining at the moment Core moved
// the bin to the destination node (after applyBinArrivalForOrder). Edge
// stores it on the order and uses it in handleNormalReplenishment to
// reset the lineside counter from the bin's actual contents — partial
// returns from operator-released runouts, fresh fills from produce, etc.
// Routing the value through this envelope avoids the
// arrivedBinUOP/FetchNodeBins HTTP round-trip and the location-telemetry
// race against AutoConfirm orders.
//
// Single-bin orders only. Nil for multi-bin orders (those don't drive a
// single lineside-UOP reset) and nil from older Core builds.
type OrderDelivered struct {
	OrderUUID       string     `json:"order_uuid"`
	DeliveredAt     time.Time  `json:"delivered_at"`
	StagedExpireAt  *time.Time `json:"staged_expire_at,omitempty"`
	BinUOPRemaining *int       `json:"bin_uop_remaining,omitempty"`
}

// OrderError signals order failure.
type OrderError struct {
	OrderUUID string `json:"order_uuid"`
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail"`
}

// OrderCancelled confirms order cancellation.
type OrderCancelled struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

// --- Complex order payloads ---

// ComplexOrderStep describes a single step in a complex (multi-leg) order.
// Node can be a concrete node name or a group node name — Core auto-detects
// and resolves groups via the group resolver.
type ComplexOrderStep struct {
	Action string `json:"action"`          // "pickup", "dropoff", "wait"
	Node   string `json:"node,omitempty"`  // node or group name (Core auto-resolves groups)
}

// ComplexOrderRequest is a multi-step transport order from edge.
type ComplexOrderRequest struct {
	OrderUUID   string             `json:"order_uuid"`
	PayloadCode string             `json:"payload_code,omitempty"`
	PayloadDesc string             `json:"payload_desc,omitempty"`
	Quantity    int64              `json:"quantity"`
	Priority    int                `json:"priority,omitempty"`
	Steps       []ComplexOrderStep `json:"steps"`
	// RemainingUOP: nil = no sync, 0 = clear manifest, >0 = partial consumption.
	RemainingUOP *int `json:"remaining_uop,omitempty"`
}

// OrderRelease signals that a staged (dwelling) order should resume.
//
// RemainingUOP late-binds the bin's manifest at the operator's release click:
//
//   - nil = no manifest change (legacy/unspecified — preserves pre-release behavior)
//   - 0   = clear manifest (bin is empty, e.g. NOTHING PULLED disposition)
//   - >0  = sync UOP, preserve manifest (bin returns as partial, e.g. SEND PARTIAL BACK)
//
// Routing on Core mirrors ClaimForDispatch but operates on the already-claimed
// bin via BinManifestService.SyncOrClearForReleased. See docs on that method.
//
// CalledBy carries the operator identity (station name, badge id, etc.) from
// the HTTP body all the way through to Core's bin audit. Empty when the
// caller is a system-internal path (wiring completion fallbacks, restore,
// etc.); Core defaults to "system" in that case.
type OrderRelease struct {
	OrderUUID    string `json:"order_uuid"`
	RemainingUOP *int   `json:"remaining_uop,omitempty"`
	CalledBy     string `json:"called_by,omitempty"`
}

// OrderStaged notifies edge that an order is dwelling at a staging node.
type OrderStaged struct {
	OrderUUID string `json:"order_uuid"`
	Detail    string `json:"detail,omitempty"`
}

// --- Origination payloads: Edge -> Core ---

// OrderIngestRequest submits a newly filled bin for storage.
// Core sets the bin manifest and dispatches a store order.
type OrderIngestRequest struct {
	OrderUUID   string               `json:"order_uuid"`
	PayloadCode string               `json:"payload_code"`
	BinLabel    string               `json:"bin_label"`
	SourceNode  string               `json:"source_node"`
	Quantity    int64                `json:"quantity"`
	Manifest    []IngestManifestItem `json:"manifest,omitempty"`
	ProducedAt  string               `json:"produced_at,omitempty"` // RFC3339 timestamp from Edge at cell completion
}

// IngestManifestItem describes a single item in an ingest manifest.
type IngestManifestItem struct {
	PartNumber  string `json:"part_number"`
	Quantity    int64  `json:"quantity"`
	Description string `json:"description,omitempty"`
}

// --- Node list data schemas ---

// NodeListRequest is sent by edge to request the core's node list.
type NodeListRequest struct{}

// NodeInfo describes a single node in the core's node list.
//
// ParentNodeType is the node type of the immediate parent (e.g. "LANE",
// "NGRP", empty for top-level nodes). Edge uses it to validate that
// consume-role style claims land on LANE-parented storage slots — the
// only nodes handleKanbanDemand will actually fire "consume" signals
// for (see shingo-core/engine/wiring_kanban.go's isStorageSlot check).
type NodeInfo struct {
	Name           string `json:"name"`
	NodeType       string `json:"node_type"`
	ParentNodeType string `json:"parent_node_type,omitempty"`
}

// NodeListResponse carries the core's authoritative node list.
type NodeListResponse struct {
	Nodes []NodeInfo `json:"nodes"`
}

// --- Production data schemas ---

// ProductionReportEntry is a single cat_id production count.
type ProductionReportEntry struct {
	CatID string `json:"cat_id"`
	Count int64  `json:"count"`
}

// ProductionReport carries production counts from an edge station.
type ProductionReport struct {
	StationID string                  `json:"station_id"`
	Reports   []ProductionReportEntry `json:"reports"`
}

// ProductionReportAck acknowledges processing of a production report.
type ProductionReportAck struct {
	StationID string `json:"station_id"`
	Accepted  int    `json:"accepted"`
}

// EdgeStale is sent by core to notify an edge that it has been marked stale.
type EdgeStale struct {
	StationID string `json:"station_id"`
	Message   string `json:"message"`
}

// EdgeRegisterRequest is sent by core to ask an edge to re-register.
type EdgeRegisterRequest struct {
	StationID string `json:"station_id"`
	Reason    string `json:"reason"`
}

// --- QR Tag Verification ---

// TagVerifyRequest is sent by edge to verify a scanned QR tag against an order's payload bin.
type TagVerifyRequest struct {
	OrderUUID string `json:"order_uuid"`
	TagID     string `json:"tag_id"`
	Location  string `json:"location,omitempty"`
}

// TagVerifyResponse is the core's response to a tag verification request.
type TagVerifyResponse struct {
	OrderUUID string `json:"order_uuid"`
	Match     bool   `json:"match"`
	Expected  string `json:"expected,omitempty"`
	Detail    string `json:"detail,omitempty"`
}

// --- Node State ---

// NodeStateRequest is sent by edge to query the occupancy state of specific nodes.
type NodeStateRequest struct {
	Nodes []string `json:"nodes"` // node names to query
}

// NodeStateEntry describes the occupancy state of a single node.
type NodeStateEntry struct {
	Name        string `json:"name"`
	Occupied    bool   `json:"occupied"`     // has at least one bin
	BinCount    int    `json:"bin_count"`     // number of bins at node
	Claimed     bool   `json:"claimed"`       // any bin claimed by an active order
	PayloadCode string `json:"payload_code,omitempty"` // payload of first bin (if occupied)
}

// NodeStateResponse carries the occupancy state for requested nodes.
type NodeStateResponse struct {
	Nodes []NodeStateEntry `json:"nodes"`
}

// --- Payload Catalog ---

// CatalogPayloadsRequest is sent by edge to request the payload catalog.
type CatalogPayloadsRequest struct{}

// CatalogPayloadInfo describes a single payload template in the catalog.
type CatalogPayloadInfo struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	Description string `json:"description"`
	UOPCapacity int    `json:"uop_capacity"`
}

// CatalogPayloadsResponse carries the core's payload catalog.
type CatalogPayloadsResponse struct {
	Payloads []CatalogPayloadInfo `json:"payloads"`
}

// OrderStatusRequest asks Core for the current authoritative status of a set of orders.
type OrderStatusRequest struct {
	OrderUUIDs []string `json:"order_uuids"`
}

// OrderStatusSnapshot is the current Core-side view of an order.
type OrderStatusSnapshot struct {
	OrderUUID     string `json:"order_uuid"`
	Found         bool   `json:"found"`
	Status        string `json:"status,omitempty"`
	StationID     string `json:"station_id,omitempty"`
	SourceNode    string `json:"source_node,omitempty"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	VendorOrderID string `json:"vendor_order_id,omitempty"`
	ErrorDetail   string `json:"error_detail,omitempty"`
}

// OrderStatusResponse carries the authoritative Core-side state for requested orders.
type OrderStatusResponse struct {
	Orders []OrderStatusSnapshot `json:"orders"`
}

// NodeStructureChanged is sent Core→Edge when a node group's structure changes
// (reparent or group deletion). Edge uses this to refresh its node cache.
type NodeStructureChanged struct {
	NodeID      int64  `json:"node_id"`
	NodeName    string `json:"node_name"`
	OldParentID *int64 `json:"old_parent_id,omitempty"`
	NewParentID *int64 `json:"new_parent_id,omitempty"`
	Action      string `json:"action"` // "reparented" or "group_deleted"
}

// ClaimSyncEntry represents a single manual_swap claim's config for the demand registry.
type ClaimSyncEntry struct {
	CoreNodeName        string   `json:"core_node_name"`
	Role                string   `json:"role"`                  // "produce" or "consume"
	AllowedPayloadCodes []string `json:"allowed_payload_codes"` // payloads this node accepts
	OutboundDestination string   `json:"outbound_destination"`
}

// ClaimSync is sent by Edge to Core on startup and claim changes.
// Core uses this to build its demand registry for kanban wiring.
type ClaimSync struct {
	StationID string           `json:"station_id"`
	Claims    []ClaimSyncEntry `json:"claims"`
}

// DemandSignal is sent by Core to Edge when a kanban event fires.
// Edge creates an order for the specified payload at the specified node.
type DemandSignal struct {
	CoreNodeName string `json:"core_node_name"` // delivery node for the order
	PayloadCode  string `json:"payload_code"`   // which payload to request
	Role         string `json:"role"`            // "produce" or "consume" — determines order type
	Reason       string `json:"reason"`          // human-readable trigger (e.g., "empty bin returned to storage")
}

// CountGroupCommand is sent by Core to Edge when an advanced zone's occupancy state changes.
// Edge translates this into a request/ack handshake against a PLC tag via WarLink.
//
// Subject: protocol.SubjectCountGroupCommand.
type CountGroupCommand struct {
	CorrelationID    string    `json:"corr_id"`             // for matching the eventual CountGroupAck
	Group            string    `json:"group"`               // RDS advanced-group name
	Desired          string    `json:"desired"`             // "on" | "off"
	Robots           []string  `json:"robots"`              // robot IDs in the zone (for audit)
	RobotCount       int       `json:"robot_count"`         // len(Robots) — cheap log without decoding the slice
	FailSafeTriggered bool     `json:"fail_safe_triggered"` // true if this command came from RDS-down fail-safe
	Timestamp        time.Time `json:"ts"`
}

// CountGroupAck is sent by Edge to Core after a CountGroupCommand has been
// processed (or abandoned) by the PLC.
//
// Subject: protocol.SubjectCountGroupAck. Outcome ∈ {AckOutcomeAcked,
// AckOutcomeTimeout, AckOutcomeWarlinkErr}.
type CountGroupAck struct {
	CorrelationID string    `json:"corr_id"`
	Group         string    `json:"group"`
	Outcome       string    `json:"outcome"`
	AckLatencyMs  int64     `json:"ack_latency_ms"`
	Timestamp     time.Time `json:"ts"`
}
