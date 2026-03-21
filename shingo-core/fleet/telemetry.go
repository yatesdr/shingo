package fleet

// OrderSnapshot captures the full state of a vendor order at a point in time.
// Used for telemetry recording. Vendor adapters populate this from their
// native detail types.
type OrderSnapshot struct {
	VendorOrderID string          `json:"vendor_order_id"`
	State         string          `json:"state"`
	Vehicle       string          `json:"vehicle"`
	CreateTime    int64           `json:"create_time"`   // ms epoch from vendor
	TerminalTime  int64           `json:"terminal_time"` // ms epoch, 0 if not terminal
	Blocks        []BlockSnapshot `json:"blocks,omitempty"`
	Errors        []OrderMessage  `json:"errors,omitempty"`
	Warnings      []OrderMessage  `json:"warnings,omitempty"`
	Notices       []OrderMessage  `json:"notices,omitempty"`
}

// BlockSnapshot captures block-level state at a point in time.
type BlockSnapshot struct {
	BlockID  string `json:"block_id"`
	Location string `json:"location"`
	State    string `json:"state"`
}

// OrderMessage represents a structured notice, warning, or error from the fleet backend.
type OrderMessage struct {
	Code      int    `json:"code"`
	Desc      string `json:"desc"`
	Times     int    `json:"times"`
	Timestamp int64  `json:"timestamp"`
}
