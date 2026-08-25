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
	// ChangeoverEvacSeats names which of a press-index cell's seats hold bins
	// that BLOCK THE TOOLING CHANGE and must therefore leave the press before
	// the tool can be swapped. Values are ChangeoverEvacSeat constants.
	//
	// A SET ON THE FRONT CLAIM, not a flag per seat, because the back seats
	// have no claim rows to carry one: only the front position of a
	// press-index cell is a style_node_claims row, and UpsertClaim rejects
	// SwapModePressPosition outright so per-seat rows cannot be created.
	//
	// Empty is the standing default and means what it has always meant: no
	// seat is marked, so nothing is evacuated for tooling and the cell takes
	// the ordinary index choreography. Unmarked seats stay put.
	//
	// The scalar EvacuateOnChangeover above remains the whole answer for
	// single-seat consume/process nodes; this is the press-index shape of the
	// same question, which is why it lives beside it and not in a new struct.
	ChangeoverEvacSeats []string `json:"changeover_evac_seats,omitempty"`
	// ChangeoverEvacDestination is where a tooling evacuation sends the bins
	// it lifts off the press. Free-form: a node name or a group name, exactly
	// like InboundSource — Core resolves either.
	//
	// Blank means today's behaviour: the evacuation falls back to
	// OutboundDestination. There is deliberately no enum and no unloader
	// special case; an unloader is reached by naming a group it projects over,
	// which keeps this field ignorant of what is on the other end.
	ChangeoverEvacDestination string `json:"changeover_evac_destination"`
	// ChangeoverLoadDirective turns this loader's card into a LOADING
	// INSTRUCTION during a changeover: instead of the operator choosing from
	// the loader's whole payload list, the card names the empty bin type the
	// changing-over cells are actually waiting for.
	//
	// ON THE CLAIM, not on the Core loader aggregate. The behaviour is an Edge
	// station's rendering of Edge's own changeover state — Core has no
	// changeover concept for loaders and could only be told about this, never
	// decide it. It also follows where every other per-station behaviour fact
	// already lives; domain.Loader carries no config at all.
	//
	// Off by default: a plant that has never wanted this sees no change.
	ChangeoverLoadDirective bool `json:"changeover_load_directive"`
	// IndexRobotSupplies flips which robot of a press-index pair fetches the
	// replacement carrier.
	//
	// Unflipped (false, today): R1 evacuates the full tote AND fetches the
	// replacement from InboundSource, dropping it on the back position; R2 just
	// indexes the on-deck carrier forward onto the press.
	//
	// Flipped (true): R1 does the evacuation and stops. R2 indexes forward and
	// then goes for the replacement itself. One robot leaves the cell as soon
	// as the press is clear; the other owns the whole refill.
	//
	// IT DESCRIBES HARDWARE, not a style. Which robot can reach the
	// supermarket is a fact about the cell's layout and reach, so two styles on
	// one press disagreeing about it is operator confusion rather than
	// configuration — UpsertClaim warns on the drift.
	IndexRobotSupplies bool `json:"index_robot_supplies"`
	// KeyRoute names map points a leg from this claim should be routed
	// through, in order. It is SEER's keyRoute: per the vendor manual
	// (RDSCore HTTP API 2026-04-30, setOrder field table) a robot-SELECTION
	// assist, not a route constraint — the fleet still plans its own path, it
	// just prefers a robot for which these points are convenient.
	//
	// Empty is today's behaviour on every order in the plant and stays the
	// default: an unset keyRoute means SEER auto-picks. That matters more than
	// usual here, because a point that does not exist or is unreachable does
	// not degrade — it TERMINATES THE WAYBILL IMMEDIATELY ON ISSUE. Hence the
	// save-time resolution check in the handler: an unresolvable point stored
	// quietly is an order that dies at dispatch with no obvious cause.
	//
	// Per CLAIM rather than per order because the reason to want one is
	// geometry — a cell reached through a particular aisle — and geometry is
	// what a claim already describes.
	KeyRoute []string `json:"key_route,omitempty"`
	// KeyTask is SEER's sibling selection hint: "load" or "unload" (the
	// manual's literal values), preferring a robot already carrying out that
	// kind of task. Empty = auto-pick, which is every order in the plant
	// today.
	KeyTask     string `json:"key_task,omitempty"`
	AutoConfirm bool   `json:"auto_confirm"`
	Sequence    int    `json:"sequence"`
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
	// payload exists in InboundSource, Edge fires a U1 retrieve_full on its
	// own. Useful for finished-goods unloaders that should drain the FG
	// supermarket continuously. Default false keeps the unloader event-driven
	// (fires only on a produce-role lineside release). See
	// engine/operator_demand.go MaybePushUnloader. The kanban demand signal
	// this comment used to name was deleted 2026-08.
	AutoPush bool `json:"auto_push"`
	// HomeLocationLoader is the same kind of computed, display-only field for the
	// home_location_loaders set (the LAYOUT axis) — populated by the API list path
	// for produce manual_swap claims so the editor can reflect/toggle it.
	HomeLocationLoader bool      `json:"home_location_loader"`
	CreatedAt          time.Time `json:"created_at"`
}

// SwapModePressPosition marks a per-position claim synthesized from a
// press-index parent — one physical seat of the press (front, paired, or
// second-paired) treated as an independent slot. The parent's SwapMode
// (two_robot_press_index) is replaced with this value so the planner routes
// it to the simple per-position builder rather than back into the press-index
// branch, and so protocol.SwapMode.IsTwoRobot reports false for it.
//
// NEVER PERSISTED. store/processes.UpsertClaim rejects it (it is absent from
// protocol.ConfigurableSwapModes), so style_node_claims never holds this
// value. A seat carrying it exists only for the life of one changeover.
const SwapModePressPosition protocol.SwapMode = "press_position"

// SynthesizePressPositionClaim builds a per-position claim from a parent
// press-index claim. CoreNodeName becomes the position's own name; SwapMode
// becomes the press_position marker; the press-index-only geometry fields
// (PairedCoreNode, SecondPairedCoreNode) and the staging fields are zeroed
// (a single position uses direct trips, no staging hop, no A/B partner).
// Everything else — PayloadCode, InboundSource, OutboundDestination, Role,
// UOPCapacity, EvacuateOnChangeover — is copied from the parent.
//
// The synthesized claim keeps the PARENT's ID: per-position node tasks
// reference that ID so wiring lookups resolve back to the real persisted
// parent row. That is LOAD-BEARING and it is the OPPOSITE of the other
// synthesized claim in this package — Loader.SynthClaim leaves ID at 0 because
// a Core-owned loader window has no persisted row at all, and its callers must
// guard on that zero. A press seat does have a row: its parent's. Zeroing this
// ID would strand every seat task, because the task's FromClaimID/ToClaimID is
// how the seat is resolved back. See the note at Loader.SynthClaim.
//
// Lives in domain, not engine, because BOTH the planner and the station view
// must derive this claim the same way. The planner synthesizes it to build
// the per-position orders; the view re-synthesizes it so a fanned-out seat
// renders as a claimed node. Hopkinsville 2026-08-05 (P400, changeover 51,
// tote → bin): the planner had it, the view did not, and every claim-keyed UI
// gate failed closed on PLN_02/PLN_05 — the tile lit up release-ready while
// its modal rendered no buttons at all. Two robots sat at a staged wait for
// 19 minutes and the operator cancelled both orders to free them. One
// definition, two callers, so the two can never drift again.
func SynthesizePressPositionClaim(parent *NodeClaim, coreNodeName string) *NodeClaim {
	if parent == nil {
		return nil
	}
	c := *parent
	c.CoreNodeName = coreNodeName
	c.SwapMode = SwapModePressPosition
	c.PairedCoreNode = ""
	c.SecondPairedCoreNode = ""
	c.InboundStaging = ""
	c.OutboundStaging = ""
	// ReuseCompatibleBins is press-index-only; clear it so the
	// reuse-compatible-bins shortcut doesn't try to apply per-position.
	c.ReuseCompatibleBins = false
	// KeepStaged shouldn't trigger inside per-position routing.
	c.KeepStaged = false
	// ChangeoverLoadDirective is an instruction to a LOADER — "go and fetch
	// these carriers for the cells that are changing over". A press seat loads
	// nothing, so the instruction is not its to give, and it only ever had the
	// flag because this is a whole-struct copy. Left set, every seat of a
	// flagged press rendered the loader's card on a tile that cannot act on it.
	//
	// Safe to clear, unlike the ID above: nothing resolves a seat back to its
	// parent through this field.
	c.ChangeoverLoadDirective = false
	return &c
}

// SeatClaimFromParent resolves the claim for a press seat that owns changeover
// work but has no style_node_claims row of its own, given the PARENT claim the
// seat's node task was planned against.
//
// This is the ONE derivation. Three consumers need it and they must agree:
//
//   - the planner, which synthesizes the seat claims to build the per-seat
//     orders (FanOutPressIndexDifferentBinType),
//   - the station view, which re-derives them so a fanned-out seat renders as
//     claimed rather than as an unclaimed node with no buttons,
//   - the per-node changeover actions and the cutover gate, which have to be
//     able to ADVANCE that seat's task — the front seat resolves by node name
//     and the back seats cannot, so before this they refused "target style
//     claim not found for node" and the task never left its live state. The
//     cutover gate blocks on any live task, so the whole changeover deadlocked
//     with cancel as the only exit.
//
// Returns nil for anything that is not a seat of a press-index parent. A wrong
// claim is worse than none: the caller then reports its own honest refusal
// rather than acting on an invented configuration.
//
// The parent is found through the node task's FromClaimID / ToClaimID, which
// point at the real persisted parent row precisely because
// SynthesizePressPositionClaim keeps the parent's ID. See the contract note
// there.
func SeatClaimFromParent(parent *NodeClaim, coreNodeName string) *NodeClaim {
	if parent == nil || coreNodeName == "" {
		return nil
	}
	if parent.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return nil
	}
	// The seat must be one this parent actually names. Any other claimless node
	// holding a task is a different problem and stays claimless.
	if parent.PairedCoreNode != coreNodeName && parent.SecondPairedCoreNode != coreNodeName {
		return nil
	}
	return SynthesizePressPositionClaim(parent, coreNodeName)
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

// Ptr returns a pointer to v. It exists for the absent-means-untouched fields
// on NodeClaimInput below, which cannot be written as &true / &1 inline.
func Ptr[T any](v T) *T { return &v }

// ChangeoverEvacSeat identifies one seat of a press-index cell in the per-seat
// tooling-relevance selection.
//
// POSITIONAL, NOT BY NODE NAME. A seat is "the front position", not
// "PLN_002_B" — the names live on PairedCoreNode / SecondPairedCoreNode and
// change when a press is re-cabled or a style re-pairs it. Storing the
// position means a selection survives a rename; storing the name would leave
// a set pointing at a seat that is no longer part of the cell.
const (
	EvacSeatFront  = "front"  // CoreNodeName
	EvacSeatPaired = "paired" // PairedCoreNode
	EvacSeatSecond = "second" // SecondPairedCoreNode
)

// ChangeoverEvacSeats is the ordered, canonical seat vocabulary — front to
// back, the direction bins index. Order matters for rendering and for the
// deterministic plan the builder emits.
func ChangeoverEvacSeatKeys() []string {
	return []string{EvacSeatFront, EvacSeatPaired, EvacSeatSecond}
}

// SeatCoreNode resolves a seat key to the core node holding it on this claim.
// Returns "" for a seat the claim's layout does not have — a 2-position press
// asked for its third seat.
func SeatCoreNode(c *NodeClaim, seat string) string {
	if c == nil {
		return ""
	}
	switch seat {
	case EvacSeatFront:
		return c.CoreNodeName
	case EvacSeatPaired:
		return c.PairedCoreNode
	case EvacSeatSecond:
		return c.SecondPairedCoreNode
	}
	return ""
}

// EvacSeatMarked reports whether this claim marks the given seat as holding
// bins that block the tooling change.
func EvacSeatMarked(c *NodeClaim, seat string) bool {
	if c == nil {
		return false
	}
	for _, s := range c.ChangeoverEvacSeats {
		if s == seat {
			return true
		}
	}
	return false
}

// MarkedEvacSeatNodes returns the core nodes of every marked seat that this
// claim's layout actually has, front to back.
//
// A marked seat the layout does not have contributes nothing rather than an
// empty string: a 2-position press whose claim still carries "second" from a
// 3-position past must plan two evacuations, not two and a phantom.
func MarkedEvacSeatNodes(c *NodeClaim) []string {
	var out []string
	for _, seat := range ChangeoverEvacSeatKeys() {
		if !EvacSeatMarked(c, seat) {
			continue
		}
		if node := SeatCoreNode(c, seat); node != "" {
			out = append(out, node)
		}
	}
	return out
}

// StagedToolingChangeover reports whether this OUTGOING claim puts the cell
// into the staged tooling-evacuation mode: a press-index cell with at least
// one seat marked as holding bins that block the tool change.
//
// ── THE OUTGOING CLAIM OWNS THE ANSWER ────────────────────────────────────
//
// The question is "which bins are physically in the way of the tool change".
// The bins on the press at that moment were put there by the OUTGOING setup
// and belong to the outgoing style; the incoming style has not placed
// anything yet. So the claim that knows is the one being replaced.
//
// It also settles an inconsistency rather than adding one. The Drop branch of
// the planner already reads fromClaim.EvacuateOnChangeover to decide whether
// cutover waits for a bin to leave, while DiffStyleClaims read
// to.EvacuateOnChangeover for the same concept — two readings of one flag,
// three files apart. Both now read the outgoing claim.
//
// CONSEQUENCE, and it is a real one: a plant that set EvacuateOnChangeover on
// the INCOMING style's claim and relied on the diff reading it will stop
// evacuating. changeoverEvacConfigOnWrongSide below exists to make that
// visible instead of silent.
func StagedToolingChangeover(from *NodeClaim) bool {
	if from == nil || from.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		return false
	}
	return len(MarkedEvacSeatNodes(from)) > 0
}

// ChangeoverNeedsEvacuation reports whether this transition has to take bins
// off the line before the tool can change, reading the OUTGOING claim per the
// rule above. Either the whole-node scalar or a per-seat selection says yes.
func ChangeoverNeedsEvacuation(from *NodeClaim) bool {
	if from == nil {
		return false
	}
	return from.EvacuateOnChangeover || StagedToolingChangeover(from)
}

// EvacConfigOnWrongSide reports the case worth a log line: the INCOMING claim
// asks for evacuation and the outgoing one does not, so this transition will
// not evacuate where the old read would have.
//
// Not a refusal and not a fallback — a diagnostic. Honouring it would be a
// second ownership rule, and the whole point of the decision above is that
// there is one. Naming it is what turns "the config is on the wrong claim"
// from a silent behaviour change into a line an engineer can act on.
func EvacConfigOnWrongSide(from, to *NodeClaim) bool {
	if from == nil || to == nil {
		return false
	}
	return to.EvacuateOnChangeover && !ChangeoverNeedsEvacuation(from)
}

// EvacDestinationFor is where a tooling evacuation sends this claim's bins.
// ChangeoverEvacDestination when set, else the ordinary OutboundDestination —
// blank means "unchanged from today", which is the whole compatibility story
// for the field.
func EvacDestinationFor(c *NodeClaim) string {
	if c == nil {
		return ""
	}
	if c.ChangeoverEvacDestination != "" {
		return c.ChangeoverEvacDestination
	}
	return c.OutboundDestination
}

// OptValue reads an absent-means-untouched field as the value its caller
// expressed, or the zero value when the caller said nothing.
//
// For VALIDATION and for reading intent. Writers must not use it: the whole
// point of the pointer is that "said nothing" and "said empty" are different
// instructions to updateClaim, and this collapses them.
func OptValue[T any](p *T) T {
	var zero T
	if p == nil {
		return zero
	}
	return *p
}

// NodeClaimInput is the request shape for creating or updating a
// NodeClaim — the persisted NodeClaim fields minus ID and CreatedAt.
type NodeClaimInput struct {
	StyleID              int64              `json:"style_id"`
	CoreNodeName         string             `json:"core_node_name"`
	Role                 protocol.ClaimRole `json:"role"`
	SwapMode             protocol.SwapMode  `json:"swap_mode"`
	PayloadCode          string             `json:"payload_code"`
	UOPCapacity          int                `json:"uop_capacity"`
	ReorderPoint         int                `json:"reorder_point"`
	InboundStaging       string             `json:"inbound_staging"`
	OutboundStaging      string             `json:"outbound_staging"`
	InboundSource        string             `json:"inbound_source"`
	OutboundDestination  string             `json:"outbound_destination"`
	AllowedPayloadCodes  []string           `json:"allowed_payload_codes"`
	AutoRequestPayload   string             `json:"auto_request_payload"`
	EvacuateOnChangeover bool               `json:"evacuate_on_changeover"`
	PairedCoreNode       string             `json:"paired_core_node"`
	SecondPairedCoreNode string             `json:"second_paired_core_node"`
	// ALL SIX ARE POINTER-TYPED — absent means leave the stored value alone.
	// See the contract block below, and the same-named fields on NodeClaim for
	// what each one means.
	//
	// The first version of this shipped only IndexRobotSupplies as a pointer,
	// on the argument that the claim editor owns controls for the other five
	// and therefore always has an opinion. The editor does. It is not the only
	// writer: the replenishment admin page reads a claim, re-sends a subset,
	// and changing a reorder point wiped the press's evacuation seats, its
	// evacuation destination, the loader card and the key route. "The one UI I
	// am thinking of always fills this in" is not a property of a struct, and
	// the pointer is what makes the class unrepresentable rather than fixing
	// each caller as it is discovered.
	ChangeoverEvacSeats       *[]string `json:"changeover_evac_seats,omitempty"`
	ChangeoverEvacDestination *string   `json:"changeover_evac_destination,omitempty"`
	ChangeoverLoadDirective   *bool     `json:"changeover_load_directive,omitempty"`
	IndexRobotSupplies        *bool     `json:"index_robot_supplies,omitempty"`
	KeyRoute                  *[]string `json:"key_route,omitempty"`
	KeyTask                   *string   `json:"key_task,omitempty"`
	AutoConfirm               bool      `json:"auto_confirm"`
	LinesideSoftThreshold     int       `json:"lineside_soft_threshold"`
	ReuseCompatibleBins       bool      `json:"reuse_compatible_bins"`
	AutoPush                  bool      `json:"auto_push"`
	// ── ABSENT MEANS LEAVE UNTOUCHED ────────────────────────────────────
	//
	// These are columns no single writer owns. updateClaim writes every
	// column it is given, and a value type cannot say "I have no opinion" —
	// it says 0, "" or false. So the claims editor, which has controls for
	// none of these, reset board order to 0, the reorder point's
	// provenance to "legacy", and both flags to false on EVERY save of an
	// unrelated field. The auto_reorder case had already been patched by
	// hand in the UI (read the claim back, echo the value); that is the same
	// disease treated one field at a time, and it is deleted with this.
	//
	// A pointer is the fix at the contract rather than at each caller: nil
	// means the writer is not speaking about this column. On INSERT they
	// take their documented defaults (sequence = next free, source =
	// "legacy", flags off). Same rules as HomeLocationLoader below.
	ReorderPointSource *string `json:"reorder_point_source,omitempty"`
	AutoReorder        *bool   `json:"auto_reorder,omitempty"`
	KeepStaged         *bool   `json:"keep_staged,omitempty"`
	Sequence           *int    `json:"sequence,omitempty"`

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
