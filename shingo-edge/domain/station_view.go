package domain

import "time"

// NodeBinState is the bin information fetched from Core via telemetry
// and folded into a StationNodeView. Reflects the current bin (if any)
// at the corresponding Core node — what's loaded, how full, whether
// the manifest has been confirmed.
type NodeBinState struct {
	BinLabel          string  `json:"bin_label,omitempty"`
	BinTypeCode       string  `json:"bin_type_code,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	UOPRemaining      int     `json:"uop_remaining"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
}

// StationNodeView is the per-node section of an OperatorStationView —
// pairs the persisted process Node and its RuntimeState with the
// claim-and-orders context needed for the HMI's node tile, plus the
// most recent release-error string surfaced as a chip when applicable.
type StationNodeView struct {
	Node           Node          `json:"node"`
	Runtime        *RuntimeState `json:"runtime,omitempty"`
	ActiveClaim    *NodeClaim    `json:"active_claim,omitempty"`
	TargetClaim    *NodeClaim    `json:"target_claim,omitempty"`
	ChangeoverTask *NodeTask     `json:"changeover_task,omitempty"`
	Orders         []Order       `json:"orders"`
	BinState       *NodeBinState `json:"bin_state,omitempty"`
	// SwapReady is true when both tracked orders for a two-robot swap are
	// in "staged" status — i.e. both robots are holding at their wait
	// points and a single coordinated release can move both forward.
	// Non-two-robot nodes always report false.
	SwapReady bool `json:"swap_ready"`
	// ChildOfNode is set when this tile is rendered on a station only because
	// the node it EXTENDS lives here — a press-index seat with no
	// operator_station_id of its own, shown under its press. Carries the
	// owning node's display name.
	//
	// Load-bearing for the UI, not decoration: a child tile must NOT offer a
	// release button. The seat owns no task and no order, so there is nothing
	// to release; a button there would either no-op or, worse, release the
	// parent's work from a tile that does not represent it. Empty on ordinary
	// tiles.
	ChildOfNode string `json:"child_of_node,omitempty"`
	// LinesideActive is the set of buckets currently counting toward
	// remaining UOP on this node (one row per part for the active style).
	// Rendered as the "active lineside bar" beneath the node fill-bar.
	LinesideActive []LinesideBucket `json:"lineside_active,omitempty"`
	// LinesideInactive is the set of stranded buckets — parts that were
	// pulled to lineside under a prior style and haven't been drained or
	// recalled yet. Rendered as stacked chips that open a detail modal.
	LinesideInactive []LinesideBucket `json:"lineside_inactive,omitempty"`
	// LastReleaseError is set when one of the runtime's tracked orders has
	// been rolled back to StatusStaged after a Core-side release failure
	// (e.g. manifest_sync_failed). The operator UI surfaces this as a chip
	// on the node card with the detail string so the operator knows why
	// their release didn't take and can click release again to retry.
	// Empty when no recent release error is pending.
	LastReleaseError string `json:"last_release_error,omitempty"`
	// StrandedAlarm is the parked-ticks alarm sentence (P2-C7/C8) for this node:
	// consume ticks piling up in pending_uop_delta while no bin is bound. The
	// operator UI renders it as an amber chip on the node card, verbatim —
	// "CARRIER-XXXX staged Nh at <node>, not bound — Record Count on the bin tab."
	// Empty when the node has no active stranding alarm.
	StrandedAlarm string `json:"stranded_alarm,omitempty"`
	// ActiveStylePayloads / AllStylePayloads are the manual_swap loader-board
	// unions across EVERY active process sharing this node's CoreNodeName (not
	// just this station's process): active = payloads the running styles need,
	// all = every covered payload (the preload list). Populated only for
	// manual_swap nodes. This is what lets an operator at a loader shared by
	// two cells (SNF2 + SNF3) see both cells' payloads instead of one.
	ActiveStylePayloads []string `json:"active_style_payloads,omitempty"`
	AllStylePayloads    []string `json:"all_style_payloads,omitempty"`
	// OperatorDriven is true when the loader's replenishment mode is operator —
	// a person stages it, and the board defaults to preload. Read from the Core-
	// owned loader aggregate (Loader.IsOperatorDriven).
	//
	// It used to come from an Edge-only operator_driven_loaders table. That
	// table outlived its readers: the supply decision moved onto the loader row
	// and stopped consulting it, leaving a toggle on the claim editor that wrote
	// a row nobody read. Deleted.
	OperatorDriven bool `json:"operator_driven,omitempty"`
	// HomeLocationLoader is true when this loader's core node is in the
	// home_location_loaders set — the dedicated-position LAYOUT. The board then
	// renders one card per home (position × its payload) across the station's
	// loader nodes, instead of one window with a card per payload. Orthogonal to
	// OperatorDriven (this is layout; that is type).
	HomeLocationLoader bool `json:"home_location_loader,omitempty"`
	// HasBufferPartial is true when this is a dedicated home position with a
	// tracked bin (UOP > 0) AND the loader's buffer slot holds a partial with
	// the same payload. When set, the HMI shows the "Clear Bin" button so the
	// operator can zero the home bin and trigger the consolidation move sequence.
	HasBufferPartial bool `json:"has_buffer_partial,omitempty"`
	// WindowGroupAnchor is the LoaderID this node belongs to when it is one window
	// of a shared MULTI-window loader, resolved from the Core aggregate (not the
	// legacy per-node claim). Empty for a single-window/legacy loader or a non-loader
	// node. This is what lets the operator board show "Window N of <loader>" and know
	// the node is part of a shared demand budget — the membership that the
	// claim-derived fields above structurally cannot express (multi-window refactor
	// C4b, the view-path cutover). Populated only for manual_swap nodes.
	WindowGroupAnchor string `json:"window_group_anchor,omitempty"`
	// WindowNodes is the sibling window set of this node's shared loader — every
	// window's core_node_name — populated alongside WindowGroupAnchor. One
	// physical bin per window; the windows share the loader's single empty-in
	// budget (one demand of N → N empties across the set, never 2N).
	//
	// NOT "in loader order", which is what this used to say. Core's operator-defined
	// window order does not survive the trip: it has no field on the wire and no
	// column in the Edge cache, so the order here is whatever the cache read
	// produced, which is by node NAME. Nothing renders this field today (no
	// consumer in any page or template), so the wrong claim misled readers rather
	// than the board.
	WindowNodes []string `json:"window_nodes,omitempty"`
	// ActivePayloadLineside maps an active-style payload code to the current
	// lineside UOP for it — the bin at the consuming node plus parts pulled to
	// the line (active buckets), summed across ALL active consume nodes for that
	// payload in this process. Populated only for manual_swap loader nodes; the
	// transitional board shows it on ACTIVE cards in place of "no demand" so the
	// operator sees how much the running styles still have lineside.
	ActivePayloadLineside map[string]int `json:"active_payload_lineside,omitempty"`
	// StarvedPayloads marks active-style payloads whose lineside UOP has
	// dropped into the danger zone (service.linesideStarved). The operator
	// board renders these cards red so the operator preloads before the line
	// runs dry. Populated only for manual_swap loader nodes.
	StarvedPayloads map[string]bool `json:"starved_payloads,omitempty"`
	// SupplyRefusals maps a payload code to the loader operator's standing
	// statement that they cannot fill the call on that card, keyed the same way
	// the card is: (this node, that payload). Populated for manual_swap loader
	// nodes on BOTH layouts — a shared window's payload card and a dedicated
	// home's position card reach the same key.
	//
	// The card renders red-DORMANT when a refusal is present: same hue as the
	// unanswered QUEUED state, pulse removed. The pulse means "nobody has dealt
	// with this", so dropping it means "answered" — a channel the status palette
	// does not otherwise use, which matters because the guide records the palette
	// as being at capacity and the next status having to earn a weight rather
	// than a hue.
	SupplyRefusals map[string]SupplyRefusal `json:"supply_refusals,omitempty"`
	// SupplyRefusedForMe is set on a CELL node when a loader operator has said
	// they cannot fill a part this cell has an outstanding call for.
	//
	// THE FILTER IS "A CALL AND THE PART", and it is the same predicate the
	// supplier's endpoint enforces before accepting the refusal at all — one rule
	// applied at both ends of the channel rather than two that have to be kept in
	// step. It deliberately does NOT consult inbound_source: that column may hold
	// a node GROUP name, and matching on it would go silent exactly where it
	// cannot resolve.
	//
	// It also does not wait for the cell to be nearly empty. Material is called
	// BEFORE it runs out, so a cell with an outstanding call and forty minutes of
	// stock is the best possible moment to tell someone their part is not coming —
	// it is the only point where they still have room to change over on their own
	// terms. Gating on "lineside is low" would hold the warning until the line
	// stops, which is the failure this project exists to fix.
	SupplyRefusedForMe *CellSupplyRefusal `json:"supply_refused_for_me,omitempty"`
}

// CellSupplyRefusal is a refusal as the CUSTOMER sees it — the same fact as
// SupplyRefusal plus the window that said it, because the cell's sentence names
// a place the loader's does not need to.
type CellSupplyRefusal struct {
	LoaderNode  string    `json:"loader_node"`
	PayloadCode string    `json:"payload_code"`
	RefusedAt   time.Time `json:"refused_at"`
	RefusedBy   string    `json:"refused_by,omitempty"`
	// Answered false is what fires the modal. It is a STATE, not a schedule:
	// there is no interval to invent because the question stops being asked the
	// moment it is answered, and the answer is durable so it is never asked twice.
	Answered  bool   `json:"answered"`
	AckChoice string `json:"ack_choice,omitempty"`
}

// SupplyRefusal is one card's open refusal as the HMI sees it.
//
// Deliberately not the store row: the board needs what to render, not the
// storage shape. Answered is precomputed because "refused" and "refused and
// answered" are different card states and the render layer must not have to
// infer the second from a nullable timestamp.
type SupplyRefusal struct {
	RefusedAt time.Time `json:"refused_at"`
	// RefusedBy is the station name, not a person — the loader board carries no
	// operator identity. Rendered as attribution because a human said it, and
	// station-level is the honest granularity available.
	RefusedBy string `json:"refused_by,omitempty"`
	// Answered is false while the cell has been told and has not replied. That
	// state is the one worth surfacing: it is the original complaint — the
	// information exists and nobody has acted on it.
	Answered  bool   `json:"answered"`
	AckChoice string `json:"ack_choice,omitempty"`
}

// OperatorStationView is the top-level shape rendered by the operator
// HMI for a single Station. Composes the persisted Station + Process
// state with the active/target style, the in-flight Changeover (if
// any), and the StationNodeView per process node.
type OperatorStationView struct {
	Station          Station           `json:"station"`
	Process          Process           `json:"process"`
	CurrentStyle     *Style            `json:"current_style,omitempty"`
	TargetStyle      *Style            `json:"target_style,omitempty"`
	AvailableStyles  []Style           `json:"available_styles,omitempty"`
	ActiveChangeover *Changeover       `json:"active_changeover,omitempty"`
	StationTask      *StationTask      `json:"station_task,omitempty"`
	Nodes            []StationNodeView `json:"nodes"`
}
