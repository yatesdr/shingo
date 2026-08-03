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
	// PLC-driven cutover (the Changeover_Active subscription) was REMOVED.
	// It read a tag that was never wired at any plant, so the feature could
	// not fire — and its opt-in flag actively HID the operator station's
	// CUTOVER button (operator-render.js), which would have stranded a
	// changeover with neither an automatic nor a manual way to complete it.
	// CATID auto-arm below is the mechanism that replaced it. The
	// processes.auto_cutover_enabled COLUMN is retained and unread: dropping
	// it means a SQLite table rebuild, and a rebuild is precisely what
	// generates the dangling REFERENCES clauses the FK repair exists to fix.
	//
	// ChangeoverAutoArm controls the PLC CATID auto-arm behavior for this
	// process, a 3-value mode (auto | prompt | off):
	//   "auto"   — on a STABLE, confirmed CATID change that maps to a configured
	//              style's expected_catid (differs from the active style, with no
	//              changeover already in progress), automatically START the
	//              changeover to that style. The default everywhere; naturally
	//              inert where no style has an expected_catid (nothing to match ⇒
	//              nothing to arm), so default-auto is safe on unconfigured plants.
	//   "prompt" — only PROMPT the operator to start the changeover (the round-2
	//              behavior, raiseCATIDChangePrompt); never auto-start.
	//   "off"    — neither auto-arm nor prompt; silent.
	// Empty/unknown persisted values read as "auto" via NormalizeChangeoverAutoArm.
	ChangeoverAutoArm string    `json:"changeover_auto_arm"`
	CreatedAt         time.Time `json:"created_at"`
}

// Changeover auto-arm modes for Process.ChangeoverAutoArm.
const (
	// ChangeoverAutoArmAuto auto-starts a changeover on a stable, mapped CATID
	// change. The global default.
	ChangeoverAutoArmAuto = "auto"
	// ChangeoverAutoArmPrompt only prompts the operator (round-2 behavior).
	ChangeoverAutoArmPrompt = "prompt"
	// ChangeoverAutoArmOff is silent — neither arm nor prompt.
	ChangeoverAutoArmOff = "off"
)

// NormalizeChangeoverAutoArm coerces a persisted/submitted auto-arm value to one
// of the three valid modes. Empty or unknown ⇒ auto (the safe default that is
// inert without an expected_catid match).
func NormalizeChangeoverAutoArm(mode string) string {
	switch mode {
	case ChangeoverAutoArmPrompt:
		return ChangeoverAutoArmPrompt
	case ChangeoverAutoArmOff:
		return ChangeoverAutoArmOff
	default:
		return ChangeoverAutoArmAuto
	}
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
	// ExpectedCATID is the PLC part-identity value (WarLink CATID_01) that
	// this style's physical part stamps on the press. Empty = unconfigured,
	// which leaves the A5 CATID guard INERT for this style (never blocks on an
	// empty expected value). When set, the guard blocks outgoing-style relief
	// whenever the press's live CATID_01 diverges from this value — the
	// ground-truth "is the right part physically on the press" check
	// (Hopkinsville 2026-07-23).
	ExpectedCATID string `json:"expected_catid"`
	// DeletedAt marks a RETIRED style. Non-nil means the operator removed it;
	// the row survives so that changeover history, hourly counts and reporting
	// points keep resolving a name instead of rendering blank, and so the
	// removal can be undone. Pickers exclude these (see liveStyles in
	// store/processes/styles.go); display joins deliberately do not.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

// StyleVariant describes one style to scaffold by cloning a base style
// (copying its node-claim choreography verbatim) and then overriding only
// the per-payload fields on selected claims. Used by GenerateStyles to
// stamp out a family of near-identical styles — e.g. the 8 part numbers a
// press runs, which share node layout, swap mode, staging, and pairing and
// differ only in which payload each produce node carries.
type StyleVariant struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Overrides   []ClaimOverride `json:"overrides"`
}

// ClaimOverride sets the per-payload fields on one cloned claim, matched by
// CoreNodeName. Every other claim field (role, swap_mode, staging, pairing,
// flags) is inherited from the base clone and never touched here, so the
// override can't break a swap-mode invariant the base already satisfied.
// For manual_swap claims PayloadCode is empty and AllowedPayloadCodes holds
// the switchable set; for every other mode PayloadCode is the binding and
// AllowedPayloadCodes is usually just that one code.
type ClaimOverride struct {
	CoreNodeName        string   `json:"core_node_name"`
	PayloadCode         string   `json:"payload_code"`
	UOPCapacity         int      `json:"uop_capacity"`
	AllowedPayloadCodes []string `json:"allowed_payload_codes"`
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
	// DeletedAt marks a RETIRED node — see Style.DeletedAt. Deleting a
	// process_node CASCADEs into changeover_node_tasks, whose process_node_id
	// is NOT NULL, so a hard delete destroys per-node changeover detail with no
	// SET NULL alternative.
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
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
	ActiveBinEpoch     int64 `json:"active_bin_epoch"`
	RemainingUOPCached int   `json:"remaining_uop_cached"`
	// PendingUOPDelta holds count changes that arrived while no bin was
	// bound at this slot (active_bin_id nil — the pickup→delivery gap).
	// The next tick that finds a bound bin applies (current + pending) and
	// resets this to 0, so a swap or message-lag window can't lose or
	// misattribute consumption. Durable (column) so an Edge restart
	// mid-gap doesn't drop the held count.
	PendingUOPDelta int64     `json:"pending_uop_delta"`
	ActiveOrderID   *int64    `json:"active_order_id,omitempty"`
	StagedOrderID   *int64    `json:"staged_order_id,omitempty"`
	ActivePull      bool      `json:"active_pull"`
	UpdatedAt       time.Time `json:"updated_at"`
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
	// Produce-role manual_swap claim (bin loader): NOT READ. Loader
	// replenishment is Core-owned — operator push or UOP threshold — and
	// no bin-count floor survives. Setting this on a loader claim does
	// nothing.
	ReorderPoint int `json:"reorder_point"`
	// ReorderPointSource (UOP-threshold replenishment) records how
	// ReorderPoint was set. 'legacy' = default, never edited (the
	// silent-inert default); 'manual' = engineer typed a value;
	// 'calculated' = applied from the unified calculator. Surfaced in
	// the replenishment UI as a small badge per row.
	ReorderPointSource string `json:"reorder_point_source"`
	AutoReorder        bool   `json:"auto_reorder"`
	// BelowReorderSince is the FALLING EDGE of this claim's level: when
	// remaining UOP first went at-or-below ReorderPoint. Nil means not below.
	//
	// It is what turns a LEVEL into an EDGE. Without it the predicate can only
	// answer "is it below right now", which cannot tell one below-threshold
	// episode from the next — so an origin minted from it would be a fresh
	// "demand" per tick, i.e. an id per ORDER, which is the paperwork-counting
	// failure the demand grain exists to avoid.
	//
	// Written through ON TRANSITION ONLY; the level is read on every consume
	// tick. Nullable because "not below" is genuinely absent, not a zero time.
	BelowReorderSince    *time.Time `json:"below_reorder_since,omitempty"`
	InboundStaging       string     `json:"inbound_staging"`
	OutboundStaging      string     `json:"outbound_staging"`
	InboundSource        string     `json:"inbound_source"`
	OutboundDestination  string     `json:"outbound_destination"`
	AllowedPayloadCodes  []string   `json:"allowed_payload_codes"`
	AutoRequestPayload   string     `json:"auto_request_payload"`
	KeepStaged           bool       `json:"keep_staged"`
	EvacuateOnChangeover bool       `json:"evacuate_on_changeover"`
	PairedCoreNode       string     `json:"paired_core_node"`
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
	// OperatorDriven is a computed, display-only field — NOT a persisted
	// claim column. It mirrors the loader-wide operator_driven_loaders set
	// (Edge-only, keyed by core_node_name) and is populated by the API list
	// path only for produce manual_swap (bin loader) claims so the Edge
	// processes claim editor can reflect/toggle it. Every other reader sees
	// the zero value; they don't consult it.
	OperatorDriven bool `json:"operator_driven"`
	// HomeLocationLoader is the same kind of computed, display-only field for the
	// home_location_loaders set (the LAYOUT axis) — populated by the API list path
	// for produce manual_swap claims so the editor can reflect/toggle it.
	HomeLocationLoader bool      `json:"home_location_loader"`
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
	// OperatorDriven toggles the loader-wide operator_driven_loaders set
	// (Edge-only, keyed by core_node_name). It is NOT persisted on the claim
	// row — the upsert handler applies it to the set only for a produce
	// manual_swap claim. A nil pointer means "field absent, leave the set
	// untouched" so saves of unrelated claims can't clear a loader's flag.
	OperatorDriven *bool `json:"operator_driven,omitempty"`
	// HomeLocationLoader toggles the loader-wide home_location_loaders set
	// (Edge-only, keyed by core_node_name) — the dedicated-position LAYOUT.
	// Same rules as OperatorDriven: applied only for a produce manual_swap
	// claim; a nil pointer leaves the set untouched.
	HomeLocationLoader *bool `json:"home_location_loader,omitempty"`
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

// ParticipantRole distinguishes the two kinds of node a changeover touches.
//
// Deliberately TWO values. A richer evac/supply/both vocabulary was rejected in
// review because it duplicates data the task row already owns; participants
// answer "is this node part of the changeover", not "what work happens there".
const (
	// ParticipantRoleTask — the node owns a changeover_node_task: there is
	// releasable work here and the operator can act on it.
	ParticipantRoleTask = "task"
	// ParticipantRoleIndexedOver — the node is physically traversed by the
	// changeover (a press-index extension position) but mints NO order and
	// owns NO task. It exists so intake gating can refuse robot traffic to a
	// position an index motion is about to place a bin on. Giving these a task
	// instead would create a phantom with no reachable terminal state.
	ParticipantRoleIndexedOver = "indexed_over"
)

// ParticipantInput is the input shape for one changeover participant, written
// in the same transaction as the node tasks.
//
// NO ORDER POINTERS, by construction. A participant is a membership fact, not a
// unit of work; the no-phantom-orders rule is expressed here as absent columns
// rather than as a convention someone has to remember.
type ParticipantInput struct {
	CoreNodeName string
	Role         string
	// OwningTaskCoreNode is the task-role node this participant hangs off —
	// for an indexed_over position, the press it extends. Empty for task-role
	// participants (they own themselves). Resolved to owning_task_id at write
	// time, once the task rows exist and have ids.
	OwningTaskCoreNode string
}

// Participant is a persisted changeover participant row.
type Participant struct {
	ID                  int64
	ProcessChangeoverID int64
	CoreNodeName        string
	// ProcessNodeID is nullable: a press-index extension position may have no
	// process_nodes row at all, and the whole point of keying this table by
	// name is that such a position stays representable and REPORTABLE rather
	// than being silently dropped at write time.
	ProcessNodeID *int64
	Role          string
	OwningTaskID  *int64
	UpdatedAt     time.Time
}
