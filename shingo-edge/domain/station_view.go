package domain

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
	// ActiveStylePayloads / AllStylePayloads are the manual_swap loader-board
	// unions across EVERY active process sharing this node's CoreNodeName (not
	// just this station's process): active = payloads the running styles need,
	// all = every covered payload (the preload list). Populated only for
	// manual_swap nodes. This is what lets an operator at a loader shared by
	// two cells (SNF2 + SNF3) see both cells' payloads instead of one.
	ActiveStylePayloads []string `json:"active_style_payloads,omitempty"`
	AllStylePayloads    []string `json:"all_style_payloads,omitempty"`
	// OperatorDriven is true when this loader's core node is in the
	// operator_driven_loaders set — operator-driven, board defaults to preload.
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
	// window's core_node_name in loader order — populated alongside WindowGroupAnchor.
	// One physical bin per window; the windows share the loader's single empty-in
	// budget (one demand of N → N empties across the set, never 2N).
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
