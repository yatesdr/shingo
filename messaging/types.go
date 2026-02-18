package messaging

import "time"

// Envelope is the typed message wrapper for all WarDrop <-> WarPath messages.
type Envelope struct {
	MsgType   string    `json:"msg_type"`
	MsgID     string    `json:"msg_id"`
	ClientID  string    `json:"client_id"`
	FactoryID string    `json:"factory_id"`
	Timestamp time.Time `json:"timestamp"`
	Payload   any       `json:"payload"`
}

// --- Inbound payloads (WarDrop -> WarPath) ---

type OrderRequest struct {
	OrderUUID     string  `json:"order_uuid"`
	OrderType     string  `json:"order_type"` // retrieve, move, store
	MaterialCode  string  `json:"material_code"`
	Quantity      float64 `json:"quantity"`
	DeliveryNode  string  `json:"delivery_node"`
	PickupNode    string  `json:"pickup_node"`
	Priority      int     `json:"priority"`
	RetrieveEmpty bool    `json:"retrieve_empty"`
	PayloadDesc   string  `json:"payload_desc"`
}

type OrderCancel struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}

type DeliveryReceipt struct {
	OrderUUID   string  `json:"order_uuid"`
	ReceiptType string  `json:"receipt_type"` // confirmed
	FinalCount  float64 `json:"final_count"`
}

type RedirectRequest struct {
	OrderUUID       string `json:"order_uuid"`
	NewDeliveryNode string `json:"new_delivery_node"`
}

// --- Outbound payloads (WarPath -> WarDrop) ---

type AckReply struct {
	OrderUUID     string `json:"order_uuid"`
	WarpathOrderID int64 `json:"warpath_order_id"`
	SourceNode    string `json:"source_node"`
}

type WaybillReply struct {
	OrderUUID  string `json:"order_uuid"`
	WaybillID  string `json:"waybill_id"`
	RobotID    string `json:"robot_id"`
	ETA        string `json:"eta,omitempty"`
}

type UpdateReply struct {
	OrderUUID string `json:"order_uuid"`
	Status    string `json:"status"`
	Detail    string `json:"detail,omitempty"`
	ETA       string `json:"eta,omitempty"`
}

type DeliveredReply struct {
	OrderUUID   string `json:"order_uuid"`
	DeliveredAt string `json:"delivered_at"`
}

type ErrorReply struct {
	OrderUUID string `json:"order_uuid"`
	ErrorCode string `json:"error_code"`
	Detail    string `json:"detail"`
}

type CancelledReply struct {
	OrderUUID string `json:"order_uuid"`
	Reason    string `json:"reason"`
}
