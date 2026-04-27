package domain

import "time"

// Process is one production process at the edge — typically a line or
// cell that runs styles in sequence. Holds the production state
// machine, optional counter binding (PLC + tag) for automatic UOP
// tracking, and the active/target style pointers used by the
// changeover flow.
type Process struct {
	ID              int64     `json:"id"`
	Name            string    `json:"name"`
	Description     string    `json:"description"`
	ActiveStyleID   *int64    `json:"active_style_id"`
	TargetStyleID   *int64    `json:"target_style_id,omitempty"`
	ProductionState string    `json:"production_state"`
	CounterPLCName  string    `json:"counter_plc_name"`
	CounterTagName  string    `json:"counter_tag_name"`
	CounterEnabled  bool      `json:"counter_enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

// Style is a build configuration that a Process can run — typically a
// part variant that determines which lineside parts apply, what the
// counter delta means, and which NodeClaims are active.
type Style struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	ProcessID   int64     `json:"process_id"`
	CreatedAt   time.Time `json:"created_at"`
}

// Node is a process node — one slot in a Process at which material
// is consumed or produced. ProcessNodeID in other tables refers to
// this row's ID. Joined fields (StationName, ProcessName) ride along
// from the read-path JOIN so callers don't have to look them up.
type Node struct {
	ID                int64     `json:"id"`
	ProcessID         int64     `json:"process_id"`
	OperatorStationID *int64    `json:"operator_station_id,omitempty"`
	CoreNodeName      string    `json:"core_node_name"`
	Code              string    `json:"code"`
	Name              string    `json:"name"`
	Sequence          int       `json:"sequence"`
	Enabled           bool      `json:"enabled"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
	// Joined fields
	StationName string `json:"station_name"`
	ProcessName string `json:"process_name"`
}

// NodeInput is the request shape for creating or updating a process
// Node — the persisted Node fields minus the server-controlled
// timestamps and joined columns.
type NodeInput struct {
	ProcessID         int64  `json:"process_id"`
	OperatorStationID *int64 `json:"operator_station_id,omitempty"`
	CoreNodeName      string `json:"core_node_name"`
	Code              string `json:"code"`
	Name              string `json:"name"`
	Sequence          int    `json:"sequence"`
	Enabled           bool   `json:"enabled"`
}

// RuntimeState is the per-process-node runtime row. Tracks the active
// NodeClaim (which Style is running here), the remaining UOP budget
// before reorder, and which Order rows are active or staged for the
// node's two-robot flow. Active/StagedOrderID are the linchpin of the
// swap-ready computation surfaced in the operator HMI.
type RuntimeState struct {
	ID            int64     `json:"id"`
	ProcessNodeID int64     `json:"process_node_id"`
	ActiveClaimID *int64    `json:"active_claim_id,omitempty"`
	RemainingUOP  int       `json:"remaining_uop"`
	ActiveOrderID *int64    `json:"active_order_id,omitempty"`
	StagedOrderID *int64    `json:"staged_order_id,omitempty"`
	ActivePull    bool      `json:"active_pull"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// NodeClaim is a per-Style binding to a process Node — declares the
// payload, capacity, reorder behaviour, two-robot swap mode, and
// staging conventions. The active NodeClaim drives material orders
// when the Process is running this Style.
type NodeClaim struct {
	ID                   int64     `json:"id"`
	StyleID              int64     `json:"style_id"`
	CoreNodeName         string    `json:"core_node_name"`
	Role                 string    `json:"role"`
	SwapMode             string    `json:"swap_mode"`
	PayloadCode          string    `json:"payload_code"`
	UOPCapacity          int       `json:"uop_capacity"`
	ReorderPoint         int       `json:"reorder_point"`
	AutoReorder          bool      `json:"auto_reorder"`
	InboundStaging       string    `json:"inbound_staging"`
	OutboundStaging      string    `json:"outbound_staging"`
	InboundSource        string    `json:"inbound_source"`
	OutboundDestination  string    `json:"outbound_destination"`
	AllowedPayloadCodes  []string  `json:"allowed_payload_codes"`
	AutoRequestPayload   string    `json:"auto_request_payload"`
	KeepStaged           bool      `json:"keep_staged"`
	EvacuateOnChangeover bool      `json:"evacuate_on_changeover"`
	PairedCoreNode       string    `json:"paired_core_node"`
	AutoConfirm          bool      `json:"auto_confirm"`
	Sequence             int       `json:"sequence"`
	// LinesideSoftThreshold is the per-claim soft cap for the release
	// qty-override prompt. Zero means "off" (default). When >0, the HMI
	// warns — but doesn't block — if the operator enters a qty greater
	// than 2× this value, catching typos before they become stranded
	// inventory.
	LinesideSoftThreshold int       `json:"lineside_soft_threshold"`
	CreatedAt             time.Time `json:"created_at"`
}

// AllowedPayloads returns the effective set of payload codes this claim
// accepts. For source nodes with an allowed list, returns that list.
// Otherwise returns a single-element list with the primary payload code.
//
// Method lives on domain.NodeClaim because it reads only the claim's
// own fields and uses no external state — pure data access, not
// persistence.
func (c *NodeClaim) AllowedPayloads() []string {
	if len(c.AllowedPayloadCodes) > 0 {
		return c.AllowedPayloadCodes
	}
	if c.PayloadCode != "" {
		return []string{c.PayloadCode}
	}
	return nil
}

// NodeClaimInput is the request shape for creating or updating a
// NodeClaim — the persisted NodeClaim fields minus ID and CreatedAt.
type NodeClaimInput struct {
	StyleID               int64    `json:"style_id"`
	CoreNodeName          string   `json:"core_node_name"`
	Role                  string   `json:"role"`
	SwapMode              string   `json:"swap_mode"`
	PayloadCode           string   `json:"payload_code"`
	UOPCapacity           int      `json:"uop_capacity"`
	ReorderPoint          int      `json:"reorder_point"`
	AutoReorder           bool     `json:"auto_reorder"`
	InboundStaging        string   `json:"inbound_staging"`
	OutboundStaging       string   `json:"outbound_staging"`
	InboundSource         string   `json:"inbound_source"`
	OutboundDestination   string   `json:"outbound_destination"`
	AllowedPayloadCodes   []string `json:"allowed_payload_codes"`
	AutoRequestPayload    string   `json:"auto_request_payload"`
	KeepStaged            bool     `json:"keep_staged"`
	EvacuateOnChangeover  bool     `json:"evacuate_on_changeover"`
	PairedCoreNode        string   `json:"paired_core_node"`
	AutoConfirm           bool     `json:"auto_confirm"`
	Sequence              int      `json:"sequence"`
	LinesideSoftThreshold int      `json:"lineside_soft_threshold"`
}

// NodeTaskInput is the input shape for creating a per-node changeover
// task. Used by the changeover-orchestration code internally; not
// directly exposed to handler request bodies but lives here so the
// service contract is persistence-free.
type NodeTaskInput struct {
	ProcessID    int64  // used for auto-creating process node
	CoreNodeName string // matched against existing nodes or used for auto-create
	FromClaimID  *int64
	ToClaimID    *int64
	Situation    string
	State        string
}
