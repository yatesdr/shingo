package domain

import (
	"time"

	"shingo/protocol"
)

// Process is one production process at the edge — typically a line or
// cell that runs styles in sequence. Holds the production state
// machine, optional counter binding (PLC + tag) for automatic UOP
// tracking, and the active/target style pointers used by the
// changeover flow.
type Process struct {
	ID              int64  `json:"id"`
	Name            string `json:"name"`
	Description     string `json:"description"`
	ActiveStyleID   *int64 `json:"active_style_id"`
	TargetStyleID   *int64 `json:"target_style_id,omitempty"`
	ProductionState string `json:"production_state"`
	CounterPLCName  string `json:"counter_plc_name"`
	CounterTagName  string `json:"counter_tag_name"`
	CounterEnabled  bool   `json:"counter_enabled"`
	// AutoCutoverEnabled subscribes to the PLC's Changeover_Active tag
	// derived from CounterTagName's parent struct. Falling edge (with
	// 2s debounce) calls CompleteProcessProductionCutover so shingo's
	// active_style_id follows PLC reality without a separate operator
	// click. Default false; opt-in per process. Operator still clicks
	// Start Changeover in shingo first — auto-cutover only drives the
	// completion side. The Theme B canCompleteChangeover gate provides
	// the safety net for spurious PLC triggers (PLC restart, fault
	// recovery): a falling edge with non-terminal tasks is a no-op,
	// logged.
	AutoCutoverEnabled bool      `json:"auto_cutover_enabled"`
	CreatedAt          time.Time `json:"created_at"`
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
// NodeClaim (which Style is running here), the bin physically present
// at the slot, the live UOP count, and which Order rows are active or
// staged for the node's two-robot flow. Active/StagedOrderID are the
// linchpin of the swap-ready computation surfaced in the operator HMI.
//
// ActiveBinID is the canonical "what bin is physically at this slot"
// pointer. Set when a delivery completes (the new bin lands), cleared
// when the bin is picked up. Edge writes UOP changes against this bin
// directly via the inventory delta path; binAtNode reads from this
// field, not from the order pointer.
//
// RemainingUOPCached is the local write-through cache of the bin's
// uop_remaining. Edge owns this number while the bin is at the node:
// PLC ticks decrement here, deltas ship to Core, Core stays in sync.
// No reverse heal — a stale Core value never overwrites Edge's live
// number.
type RuntimeState struct {
	ID            int64  `json:"id"`
	ProcessNodeID int64  `json:"process_node_id"`
	ActiveClaimID *int64 `json:"active_claim_id,omitempty"`
	ActiveBinID   *int64 `json:"active_bin_id,omitempty"`
	// ActiveBinEpoch mirrors Core's bins.delta_epoch for ActiveBinID.
	// Edge stamps every outgoing BinUOPDelta with this value so Core's
	// epoch-aware dedup accepts the delta against the right load
	// generation. Populated by LoadBin response and FetchNodeBins
	// refresh; persists across Edge restarts via the column on
	// process_node_runtime_states.
	ActiveBinEpoch     int64     `json:"active_bin_epoch"`
	CachedBinID        *int64    `json:"cached_bin_id,omitempty"`
	RemainingUOPCached int       `json:"remaining_uop_cached"`
	ActiveOrderID      *int64    `json:"active_order_id,omitempty"`
	StagedOrderID      *int64    `json:"staged_order_id,omitempty"`
	ActivePull         bool      `json:"active_pull"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// NodeClaim is a per-Style binding to a process Node — declares the
// payload, capacity, reorder behaviour, two-robot swap mode, and
// staging conventions. The active NodeClaim drives material orders
// when the Process is running this Style.
type NodeClaim struct {
	ID           int64              `json:"id"`
	StyleID      int64              `json:"style_id"`
	CoreNodeName string             `json:"core_node_name"`
	Role         protocol.ClaimRole `json:"role"`
	SwapMode     protocol.SwapMode  `json:"swap_mode"`
	PayloadCode  string             `json:"payload_code"`
	UOPCapacity  int                `json:"uop_capacity"`
	// ReorderPoint has role-dependent semantics.
	//
	// Consume-role claim: UOP threshold for auto-reorder, "fire at or
	// below" (≤) — wiring_counter_delta fires RequestNodeMaterial when
	// remaining UOP drops to ≤ ReorderPoint.
	//
	// Produce-role manual_swap claim (bin loader): bin-count
	// minimum-stock floor, "fire when fewer than" (<) — operator types
	// the desired minimum number of bins of the loader's payload, and
	// MaybeCreateLoaderEmptyIn fires the L1 retrieve_empty whenever
	// the system count drops below that. Zero falls back to a magic-
	// number floor of 2: strict "queue when fewer than 2 bins in the
	// system." The future kanban calculator (see
	// shingo-kanban-calculator-design.md) writes its calculated value
	// into this same column rather than a parallel "loader threshold"
	// field.
	ReorderPoint int `json:"reorder_point"`
	// ReorderPointSource (UOP-threshold replenishment) records how
	// ReorderPoint was set. 'legacy' = default, never edited (the
	// silent-inert default); 'manual' = engineer typed a value;
	// 'calculated' = applied from the unified calculator. Surfaced in
	// the replenishment UI as a small badge per row.
	ReorderPointSource   string   `json:"reorder_point_source"`
	AutoReorder          bool     `json:"auto_reorder"`
	InboundStaging       string   `json:"inbound_staging"`
	OutboundStaging      string   `json:"outbound_staging"`
	InboundSource        string   `json:"inbound_source"`
	OutboundDestination  string   `json:"outbound_destination"`
	AllowedPayloadCodes  []string `json:"allowed_payload_codes"`
	AutoRequestPayload   string   `json:"auto_request_payload"`
	KeepStaged           bool     `json:"keep_staged"`
	EvacuateOnChangeover bool     `json:"evacuate_on_changeover"`
	PairedCoreNode       string   `json:"paired_core_node"`
	// SecondPairedCoreNode is the optional third (back-most) position for
	// two_robot_press_index. When set, the layout is C → B → A and R1's
	// final dropoff goes to C instead of B. Empty = legacy 2-position.
	SecondPairedCoreNode string `json:"second_paired_core_node"`
	AutoConfirm          bool   `json:"auto_confirm"`
	Sequence             int    `json:"sequence"`
	// LinesideSoftThreshold is the per-claim soft cap for the release
	// qty-override prompt. Zero means "off" (default). When >0, the HMI
	// warns — but doesn't block — if the operator enters a qty greater
	// than 2× this value, catching typos before they become stranded
	// inventory.
	LinesideSoftThreshold int `json:"lineside_soft_threshold"`
	// ReuseCompatibleBins opts a press-index node into the no-swap shortcut:
	// when the next style produces the same payload AND the physical bin
	// at the node is empty, the planner skips the swap entirely. Saves a
	// robot trip when the press-index hardware can keep the same bin.
	// Default false preserves always-swap.
	ReuseCompatibleBins bool `json:"reuse_compatible_bins"`
	// AutoPush opts a consume manual_swap (unloader) claim into push-driven
	// dispatch: when the unloader window is free and a full bin of an allowed
	// payload exists in InboundSource, Edge fires a U1 retrieve_full without
	// waiting for a kanban demand signal. Useful for finished-goods unloaders
	// that should drain the FG supermarket continuously rather than wait for
	// downstream consumption. Default false preserves the kanban-driven model
	// (DemandSignal-only). See engine/operator_demand.go MaybePushUnloader.
	AutoPush bool `json:"auto_push"`
	// TransitionalLoader is a computed, display-only field — NOT a persisted
	// claim column. It mirrors the loader-wide transitional_loaders set
	// (Edge-only, keyed by core_node_name) and is populated by the API list
	// path only for produce manual_swap (bin loader) claims so the Edge
	// processes claim editor can reflect/toggle it. Every other reader sees
	// the zero value; they don't consult it.
	TransitionalLoader bool      `json:"transitional_loader"`
	CreatedAt          time.Time `json:"created_at"`
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
	StyleID               int64              `json:"style_id"`
	CoreNodeName          string             `json:"core_node_name"`
	Role                  protocol.ClaimRole `json:"role"`
	SwapMode              protocol.SwapMode  `json:"swap_mode"`
	PayloadCode           string             `json:"payload_code"`
	UOPCapacity           int                `json:"uop_capacity"`
	ReorderPoint          int                `json:"reorder_point"`
	ReorderPointSource    string             `json:"reorder_point_source"`
	AutoReorder           bool               `json:"auto_reorder"`
	InboundStaging        string             `json:"inbound_staging"`
	OutboundStaging       string             `json:"outbound_staging"`
	InboundSource         string             `json:"inbound_source"`
	OutboundDestination   string             `json:"outbound_destination"`
	AllowedPayloadCodes   []string           `json:"allowed_payload_codes"`
	AutoRequestPayload    string             `json:"auto_request_payload"`
	KeepStaged            bool               `json:"keep_staged"`
	EvacuateOnChangeover  bool               `json:"evacuate_on_changeover"`
	PairedCoreNode        string             `json:"paired_core_node"`
	SecondPairedCoreNode  string             `json:"second_paired_core_node"`
	AutoConfirm           bool               `json:"auto_confirm"`
	Sequence              int                `json:"sequence"`
	LinesideSoftThreshold int                `json:"lineside_soft_threshold"`
	ReuseCompatibleBins   bool               `json:"reuse_compatible_bins"`
	AutoPush              bool               `json:"auto_push"`
	// TransitionalLoader toggles the loader-wide transitional_loaders set
	// (Edge-only, keyed by core_node_name). It is NOT persisted on the claim
	// row — the upsert handler applies it to the set only for a produce
	// manual_swap claim. A nil pointer means "field absent, leave the set
	// untouched" so saves of unrelated claims can't clear a loader's flag.
	TransitionalLoader *bool `json:"transitional_loader,omitempty"`
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
