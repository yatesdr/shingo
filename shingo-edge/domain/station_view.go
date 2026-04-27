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
}

// OperatorStationView is the top-level shape rendered by the operator
// HMI for a single Station. Composes the persisted Station + Process
// state with the active/target style, the in-flight Changeover (if
// any), and the StationNodeView per process node.
type OperatorStationView struct {
	Station          Station          `json:"station"`
	Process          Process          `json:"process"`
	CurrentStyle     *Style           `json:"current_style,omitempty"`
	TargetStyle      *Style           `json:"target_style,omitempty"`
	AvailableStyles  []Style          `json:"available_styles,omitempty"`
	ActiveChangeover *Changeover      `json:"active_changeover,omitempty"`
	StationTask      *StationTask     `json:"station_task,omitempty"`
	Nodes            []StationNodeView `json:"nodes"`
}
