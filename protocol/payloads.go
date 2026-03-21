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
	PickupNode    string `json:"pickup_node,omitempty"`
	StagingNode   string `json:"staging_node,omitempty"`
	LoadType      string `json:"load_type,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	RetrieveEmpty bool   `json:"retrieve_empty,omitempty"`
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
	PickupNode  string `json:"pickup_node"`
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
type OrderDelivered struct {
	OrderUUID      string     `json:"order_uuid"`
	DeliveredAt    time.Time  `json:"delivered_at"`
	StagedExpireAt *time.Time `json:"staged_expire_at,omitempty"`
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
type ComplexOrderStep struct {
	Action    string `json:"action"`               // "pickup", "dropoff", "wait"
	Node      string `json:"node,omitempty"`       // exact node (for dropoff to staging/production)
	NodeGroup string `json:"node_group,omitempty"` // synthetic parent (for pickup from storage)
}

// ComplexOrderRequest is a multi-step transport order from edge.
type ComplexOrderRequest struct {
	OrderUUID   string             `json:"order_uuid"`
	PayloadCode string             `json:"payload_code,omitempty"`
	PayloadDesc string             `json:"payload_desc,omitempty"`
	Quantity    int64              `json:"quantity"`
	Priority    int                `json:"priority,omitempty"`
	Steps       []ComplexOrderStep `json:"steps"`
}

// OrderRelease signals that a staged (dwelling) order should resume.
type OrderRelease struct {
	OrderUUID string `json:"order_uuid"`
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
	PickupNode  string               `json:"pickup_node"`
	Quantity    int64                `json:"quantity"`
	Manifest    []IngestManifestItem `json:"manifest,omitempty"`
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
type NodeInfo struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
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
	PickupNode    string `json:"pickup_node,omitempty"`
	DeliveryNode  string `json:"delivery_node,omitempty"`
	VendorOrderID string `json:"vendor_order_id,omitempty"`
	ErrorDetail   string `json:"error_detail,omitempty"`
}

// OrderStatusResponse carries the authoritative Core-side state for requested orders.
type OrderStatusResponse struct {
	Orders []OrderStatusSnapshot `json:"orders"`
}
