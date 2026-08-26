// Package plantspec is the declarative plant description the dev-env seed tool
// (cmd/seeddev) loads to populate a demo plant across the core (Postgres) and
// edge (SQLite) databases (brief T4.1, decision D2).
//
// The spec mirrors the seed inventory: a storage hierarchy (NGRP zones → LANE
// lanes → depth-ordered slots — load-bearing, kanban only sees nodes under that
// hierarchy), non-storage stations (line/press/weld/loader/unloader/staging),
// payloads + bin types, initial bin placement, the edge process/style/claim
// topology, demand registry, reporting points, cell configs, and lineside
// buckets. Validate() rejects the mistakes that silently break the demo
// (dangling node references, missing swap staging, no LANE/NGRP hierarchy,
// payloads with no bin type).
//
// This package is pure data + validation — it performs no I/O against the
// databases; seeddev does that through the store/domain layer.
package plantspec

import (
	"fmt"
	"os"

	"gopkg.in/yaml.v3"

	"shingo/protocol"
)

// Plant is the whole declarative spec for one demo plant.
type Plant struct {
	Namespace        string           `yaml:"namespace"`
	LineID           string           `yaml:"line_id"`
	BinTypes         []string         `yaml:"bin_types"`
	Payloads         []Payload        `yaml:"payloads"`
	Zones            []Zone           `yaml:"zones"`
	Stations         []Station        `yaml:"stations"`
	Bins             []Bin            `yaml:"bins"`
	Processes        []Process        `yaml:"processes"`
	Styles           []Style          `yaml:"styles"`
	OperatorStations []string         `yaml:"operator_stations"`
	Claims           []Claim          `yaml:"claims"`
	Demands          []Demand         `yaml:"demands"`
	ReportingPoints  []ReportingPoint `yaml:"reporting_points"`
	CellConfigs      []CellConfig     `yaml:"cell_configs"`
	LinesideBuckets  []LinesideBucket `yaml:"lineside_buckets"`
	// MaintainedGroups declares which zones Core holds an empty-carrier level in.
	MaintainedGroups []MaintainedGroup `yaml:"maintained_groups,omitempty"`
	// Headroom is the storage-slack rule the census at birth asserts (§R.78).
	Headroom Headroom `yaml:"headroom,omitempty"`
	// BaselineFrozenAt names the ruling that froze this spec as a MEASUREMENT
	// BASELINE, and it is provenance rather than a setting.
	//
	// A/B comparisons are only meaningful against an unchanged seed, so a spec
	// that a published number was measured on cannot be corrected without
	// invalidating the number. Setting this records that the spec is knowingly
	// preserved with whatever defects it has, and downgrades the census at birth
	// from a refusal to a loud report — the findings are still printed in full,
	// because the point is that they are KNOWN, not that they are acceptable.
	//
	// NOT A SETTING TO REACH FOR. A new spec that will not pass the census is a
	// spec to fix. This exists for seeds that already have numbers attached, and
	// the value must name the ruling so a reader can go and find out which.
	BaselineFrozenAt string `yaml:"baseline_frozen_at,omitempty"`
}

// Headroom is the rule that a storage group must always keep somewhere to dig
// INTO: a group filled to the brim cannot conduct an excavation at all, because
// there is nowhere to stand a blocker while the lane is opened.
//
// Owner's number (§R.78): one full lane's worth, always free. Eleven lanes of
// five is fifty-five slots and fifty bins, not fifty-five.
//
// It is config rather than a constant because the right slack is a property of a
// plant's traffic, not of the software, and because a number in a spec is a
// number somebody can argue with. The DEFAULT is one lane's worth — a spec that
// says nothing gets the rule, which is the polarity that matters: headroom has
// to be opted OUT of explicitly, never forgotten into.
type Headroom struct {
	// FreeLanes is how many of the group's deepest lane's worth of slots must be
	// left empty at birth. Nil means the default of one. Zero is legal and means
	// "no headroom guaranteed", which is a statement a spec author has to make on
	// purpose.
	FreeLanes *int `yaml:"free_lanes,omitempty"`
}

// LanesFree returns the configured headroom, defaulting to one lane's worth.
func (h Headroom) LanesFree() int {
	if h.FreeLanes == nil {
		return 1
	}
	return *h.FreeLanes
}

// Payload is a part type with its bin capacity.
type Payload struct {
	Code        string `yaml:"code"`
	UOPCapacity int64  `yaml:"uop_capacity"`
	BinType     string `yaml:"bin_type"`
	// RobotGroup names the SEER robot-dispatch group allowed to carry this part
	// (→ payloads.robot_group → rds.SetOrderRequest.Group). Empty = the vendor's
	// default assignment, i.e. any robot.
	//
	// It is a CAPABILITY, not a label: a heavy part restricted to a 1500 kg group
	// must not be handed to a 600 kg robot. The spec could not express it until
	// now, which meant no simulated plant could exercise group selection at all —
	// so the one thing that goes wrong when an order carries the wrong payload was
	// the one thing the sim could not show.
	RobotGroup string `yaml:"robot_group,omitempty"`
}

// Zone is an NGRP storage zone. RetrieveAlgorithm (e.g. FIFO) and
// StoreAlgorithm (DPTH/LKND) control kanban lane selection.
//
// A zone holds LANES (the deep-storage shape: aisles of depth-ordered slots), or
// POSITIONS (the flat shape: slots hanging directly off the group), or both. The
// flat shape exists because a MAINTAINED group is refused at save time unless it
// is depth-1 — no lanes, no nested groups — and until now a spec had no way to
// write one down, which would have left phase 2's soak unable to stage the very
// shape it is soaking.
type Zone struct {
	Name              string `yaml:"name"`
	RetrieveAlgorithm string `yaml:"retrieve_algorithm"`
	StoreAlgorithm    string `yaml:"store_algorithm"`
	Lanes             []Lane `yaml:"lanes"`
	// Positions are slots parented directly by the zone, with no lane between.
	// They carry a Depth like any other slot, and it should be 1 on all of them:
	// nothing can be buried behind anything when there is no lane to bury it in.
	Positions []Slot `yaml:"positions,omitempty"`
}

// Lane is a LANE node under a zone; its slots carry an explicit depth so buried
// bins (depth > 1) can be staged to exercise reshuffles.
type Lane struct {
	Name  string `yaml:"name"`
	Slots []Slot `yaml:"slots"`
	// GatePoint places the lane's waiting-point mark: the RDS map point a
	// lane-bound robot dwells at until Core says the lane is safe to enter. It
	// becomes the lane node's lane_gate_point property, and its EXISTENCE is the
	// enablement — a lane with a mark ships unsealed orders and gets its tail
	// appended at the gate, a lane without one is decided before dispatch and
	// parks. There is no mode to set alongside it.
	//
	// The value is handed to the fleet as a block location verbatim and is never
	// resolved against nodes, so on a real plant it must exist in the RDS map.
	// Under the simulator nothing resolves it either, which is what lets a seed
	// name points freely.
	//
	// Empty means an unmarked lane, and that is a POSITION rather than a default
	// worth avoiding: a rig that marks every lane cannot show the two
	// dispositions living side by side, which is most of what a lane-stress seed
	// is for.
	GatePoint string `yaml:"gate_point,omitempty"`
}

// Slot is a depth-ordered storage position (depth 1 = lane mouth).
type Slot struct {
	Name  string `yaml:"name"`
	Depth int    `yaml:"depth"`
}

// Station is a non-storage node: a line position, press, weld cell, loader,
// unloader, staging node, or outbound destination. Kind is advisory (for
// readability + the seeder's node-type mapping); Zone is the owning area name.
type Station struct {
	Name string `yaml:"name"`
	Kind string `yaml:"kind"` // line_in|line_out|press|weld|loader|unloader|staging|dest
	Zone string `yaml:"zone,omitempty"`
	// ChangeoverLoadDirective commandeers this station's card during a
	// changeover: rather than offering every payload it serves, the card names
	// the carrier the incoming style needs. loader/unloader only — it is a
	// property of the station, which is why it sits here and not on each
	// style's claim.
	ChangeoverLoadDirective bool `yaml:"changeover_load_directive,omitempty"`
}

// Bin is an initial bin placement. Empty Payload = an empty bin.
type Bin struct {
	Name    string `yaml:"name"`
	Slot    string `yaml:"slot"`
	Payload string `yaml:"payload,omitempty"`
	UOP     int64  `yaml:"uop,omitempty"`
	BinType string `yaml:"bin_type,omitempty"`
	// AgeS backdates the bin's loaded_at by this many seconds at seed time (the
	// seeder stamps loaded_at at second precision, so all seeded bins are otherwise
	// the same age). Use it to make a BURIED slot the globally-oldest bin so FIFO
	// retrieve must reshuffle to reach it — i.e. to exercise the ASRS reshuffle path
	// deterministically from t0. 0 = stamp now (default).
	AgeS int64 `yaml:"age_s,omitempty"`
}

// MaintainedGroup declares that Core holds a standing supply of EMPTY carriers
// in a zone, for the equipment that zone serves.
//
// "So many 45x58x32 and so many 45x48x24, unclaimed, at all times." The zone must
// be FLAT — positions, no lanes — because that is what the save-time rules
// require of a maintained group, and a spec that could declare a shape the UI
// refuses would seed a plant nobody could then edit.
type MaintainedGroup struct {
	// Group names the zone. It must be a declared zone with positions and no lanes.
	Group string `yaml:"group"`
	// Station is the Edge station the top-up orders are projected to. REQUIRED,
	// and required here for the same reason the UI refuses a blank one:
	// projectOrder no-ops on a blank StationID, so a seeded group without one
	// would produce orders that run on the floor and show on no board. A seed
	// that can only be discovered wrong at runtime is the kind this spec exists
	// to refuse.
	Station string `yaml:"station"`
	// Strict reserves the group's empties for the supported processes.
	Strict bool `yaml:"strict,omitempty"`
	// Overflow names a second zone to try when this one is at level. Optional;
	// blank means an arriving carrier waits instead.
	Overflow string `yaml:"overflow,omitempty"`
	// Levels is how many empty carriers of each type to hold.
	Levels []MaintainLevel `yaml:"levels"`
	// Supports names PROCESSES. What the seeder writes is the core nodes those
	// processes' claims name — the same editor-speaks-process, storage-holds-nodes
	// split the UI makes, and for the same reason: a claim is Edge-local and Core
	// cannot read one when it has to decide anything.
	Supports []string `yaml:"supports,omitempty"`
}

// MaintainLevel is one line of a maintained group's declared level.
//
// Want may be zero, which declares the type and asks for none of it — a
// different statement from leaving the line out, and the distinction the store
// preserves.
type MaintainLevel struct {
	BinType string `yaml:"bin_type"`
	Want    int    `yaml:"want"`
}

// Process is an edge process (one independently-counting cell or line). Each
// process runs exactly one ActiveStyle at a time; that style's claims are the
// live ones (findActiveClaim keys on process.active_style_id). Independently-
// counting nodes must be SEPARATE processes — a counter tick is applied to
// every node of the active (process, style), so two nodes sharing a process
// would double-count. A/B pairs are the exception: both sides share one
// process+style and active_pull arbitrates which one counts.
type Process struct {
	Name        string `yaml:"name"`
	ActiveStyle string `yaml:"active_style"` // the running style (drives active_style_id)
}

// Style is a per-payload configuration on a process.
type Style struct {
	Name    string `yaml:"name"`
	Process string `yaml:"process"`
	Payload string `yaml:"payload"`
}

// Claim is one style→core-node binding — the full claim row.
type Claim struct {
	CoreNode     string `yaml:"core_node"`
	Style        string `yaml:"style"`
	Role         string `yaml:"role"`      // produce|consume
	SwapMode     string `yaml:"swap_mode"` // sequential|single_robot|two_robot|two_robot_press_index|manual_swap (simple retired — runtime descriptor only)
	Payload      string `yaml:"payload"`
	UOPCapacity  int64  `yaml:"uop_capacity"`
	ReorderPoint int64  `yaml:"reorder_point"`
	AutoReorder  bool   `yaml:"auto_reorder"`
	// InboundSource names the node group empty carriers are RETRIEVED FROM, and
	// OutboundDestination the group full ones are SENT TO. Between them they are
	// the whole of a loader's flow configuration (ledger §J).
	//
	// Partials are a different mechanism and do not appear here. A carrier
	// orphaned by a changeover parks in a MEMBER SLOT of the loader — a home row
	// with home_kind='buffer' (`home_of` below, kind chosen on the loaders
	// screen) — not in a group named by this claim. The two were once documented
	// as one field doing two jobs; they were always two representations, and the
	// group-naming half is retired.
	InboundSource       string `yaml:"inbound_source,omitempty"`
	OutboundDestination string `yaml:"outbound_destination,omitempty"`
	InboundStaging      string `yaml:"inbound_staging,omitempty"`
	OutboundStaging     string `yaml:"outbound_staging,omitempty"`
	AutoPush            bool   `yaml:"auto_push"`
	AutoConfirm         bool   `yaml:"auto_confirm"`
	PairedCoreNode      string `yaml:"paired_core_node,omitempty"`
	// SecondPairedCoreNode is the THIRD position of a press-index cell (bins
	// index C → B → A). Empty is the ordinary 2-position layout.
	SecondPairedCoreNode string `yaml:"second_paired_core_node,omitempty"`
	// IndexRobotSupplies flips a press-index cell's choreography: R1 clears the
	// press and leaves, R2 indexes forward AND fetches the replacement, instead
	// of R1 backfilling on its way out.
	//
	// SEEDABLE BECAUSE THE SIM IS WHERE IT GETS PROVEN. It describes the cell's
	// hardware — which robot can reach the supermarket from that press — so a
	// plant either has it or does not, and a scenario that cannot express it
	// can only test the shape half the presses run.
	IndexRobotSupplies bool     `yaml:"index_robot_supplies,omitempty"`
	AllowedPayloads    []string `yaml:"allowed_payloads,omitempty"`
	// ── THE ROUND-3/4 CLAIM CONFIG, SEEDABLE FOR THE SAME REASON THE FLIP IS ──
	//
	// These five were added to style_node_claims and to the claim editor, and
	// left out of here — so staged tooling evacuation, evacuation destination
	// precedence, the loader card and key routes could not be expressed by any
	// scenario. Not "were not covered by a test": could not be SET, anywhere but
	// by hand in a browser, which means those features have never executed on a
	// sim and there was no way to make them.
	//
	// ChangeoverEvacNodes names the CORE NODES whose bins block the tooling
	// change — e.g. ["PLN_001", "PLN_002"]. Validation refuses a node the claim
	// does not hold, so a re-pairing cannot silently re-target a clearance.
	// ChangeoverEvacDestination is where those bins go (blank falls back to
	// outbound_destination).
	ChangeoverEvacNodes       []string `yaml:"changeover_evac_nodes,omitempty"`
	ChangeoverEvacDestination string   `yaml:"changeover_evac_destination,omitempty"`
	// ChangeoverCarryoverDisposition decides what happens to a marked position's bin
	// when the SAME part runs on that position in both styles: "replace" (default —
	// clear it and bring a fresh carrier through staging), "keep_lineside" (the
	// bin does not move), or "outbound_staging" (the same bin hops to
	// outbound_staging and returns on the tooling-done release). Ignored when
	// the payloads differ. Seedable from day one, deliberately: a feature that
	// cannot be put in a plant file cannot be exercised on a sim.
	ChangeoverCarryoverDisposition string `yaml:"changeover_carryover_disposition,omitempty"`
	// KeyRoute is the ordered list of map points a leg from this claim should be
	// routed through; KeyTask is SEER's sibling-selection hint ("load"/"unload").
	// ORDER IS MEANINGFUL in KeyRoute, so it is a list and not a set.
	KeyRoute []string `yaml:"key_route,omitempty"`
	KeyTask  string   `yaml:"key_task,omitempty"`
	// WindowOf, when set on a manual_swap loader claim, makes this node a WINDOW
	// of the named shared loader rather than its own loader: the seed groups it as
	// a window home of that loader (the multi-window model the grid editor authors
	// in production). The window still gets a Core node + Edge process_node (so
	// empties can deliver to it); only its loader-aggregate grouping changes.
	WindowOf string `yaml:"window_of,omitempty"`
	// HomeOf, when set on a manual_swap loader claim, makes this node a dedicated
	// POSITION of the named dedicated_positions loader (the home-location model —
	// the dual of WindowOf's shared windows). Each home_of claim is one position
	// carrying its OWN payload (NOT a shared set); the same payload on two positions
	// is legal (the O2 lead-time-buffer case). Like WindowOf, the node still gets a
	// Core node + Edge process_node (so empties can deliver to it); only its
	// loader-aggregate grouping changes. The named loader id may be synthetic (no
	// declared node of its own — the clean shape) or an anchor node.
	HomeOf string `yaml:"home_of,omitempty"`
	// OperatorStation, when set, assigns this node's Edge process_node to its OWN
	// operator station (created on the claim's process) instead of the process's
	// default station. The per-window-HMI model gives each window of a shared loader
	// its own physical screen — one window per station — so an operator loads the bin
	// in front of them with no window-picker and no misload. Empty = the process's
	// default station (the normal single-station-per-cell case).
	OperatorStation string `yaml:"operator_station,omitempty"`
	// ActivePull marks an A/B pair's live side. nil = the node is the active
	// pull point (default true); set false on the parked (inactive) side so the
	// seeder writes active_pull=0 and counter ticks skip it (review I4).
	ActivePull *bool `yaml:"active_pull,omitempty"`
}

// IsActivePull reports the node's seeded active-pull state (default true).
func (c Claim) IsActivePull() bool { return c.ActivePull == nil || *c.ActivePull }

// IsManualSwap reports whether this is a forklift-managed loader/unloader claim
// (operator-driven; counter ticks skip it).
func (c Claim) IsManualSwap() bool {
	return protocol.SwapMode(c.SwapMode) == protocol.SwapModeManualSwap
}

// IsMultiStepSwap reports whether the swap mode needs inbound/outbound staging
// nodes (the swap choreography validates them).
func (c Claim) IsMultiStepSwap() bool {
	switch protocol.SwapMode(c.SwapMode) {
	case protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential:
		return true
	}
	return false
}

// Demand is a demand-registry entry: a payload wanted at a core node.
// ReplenishUOPThreshold is the C-push trigger (when the market's total UOP for
// this payload drops below this threshold, the loader fires). nil = infer from
// the claim's reorder_point (backward-compatible default); explicitly set 0 =
// no C-push (informational only).
type Demand struct {
	Payload               string `yaml:"payload"`
	Node                  string `yaml:"node"`
	ReplenishUOPThreshold *int   `yaml:"replenish_uop_threshold,omitempty"`
}

// ReportingPoint ties a PLC counter tag to a core node. The plc_name/tag_name
// MUST match the edge sim process entries in shingoedge.dev.yaml.
type ReportingPoint struct {
	PLCName string `yaml:"plc_name"`
	TagName string `yaml:"tag_name"`
	Node    string `yaml:"node"`
	Style   string `yaml:"style,omitempty"`
}

// CellConfig maps an edge process to its operator station.
type CellConfig struct {
	Process string `yaml:"process"`
	Station string `yaml:"station"`
}

// LinesideBucket pre-stages lineside inventory at a consume node so consume
// ticks drain the bucket (exercises DrainLinesideBucket) before bin UOP drops.
type LinesideBucket struct {
	Node    string `yaml:"node"`
	Payload string `yaml:"payload"`
	Qty     int64  `yaml:"qty"`
}

// Load reads and parses a plant spec YAML file. It does NOT validate — call
// Validate separately so callers can choose how to surface errors.
func Load(path string) (*Plant, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("plantspec: read %s: %w", path, err)
	}
	var p Plant
	if err := yaml.Unmarshal(data, &p); err != nil {
		return nil, fmt.Errorf("plantspec: parse %s: %w", path, err)
	}
	return &p, nil
}
