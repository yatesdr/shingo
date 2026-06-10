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
}

// Payload is a part type with its bin capacity.
type Payload struct {
	Code        string `yaml:"code"`
	UOPCapacity int64  `yaml:"uop_capacity"`
	BinType     string `yaml:"bin_type"`
}

// Zone is an NGRP storage zone holding lanes. RetrieveAlgorithm (e.g. FIFO) and
// StoreAlgorithm (DPTH/LKND) control kanban lane selection.
type Zone struct {
	Name              string `yaml:"name"`
	RetrieveAlgorithm string `yaml:"retrieve_algorithm"`
	StoreAlgorithm    string `yaml:"store_algorithm"`
	Lanes             []Lane `yaml:"lanes"`
}

// Lane is a LANE node under a zone; its slots carry an explicit depth so buried
// bins (depth > 1) can be staged to exercise reshuffles.
type Lane struct {
	Name  string `yaml:"name"`
	Slots []Slot `yaml:"slots"`
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
	CoreNode            string   `yaml:"core_node"`
	Style               string   `yaml:"style"`
	Role                string   `yaml:"role"`      // produce|consume
	SwapMode            string   `yaml:"swap_mode"` // simple|sequential|single_robot|two_robot|two_robot_press_index|manual_swap
	Payload             string   `yaml:"payload"`
	UOPCapacity         int64    `yaml:"uop_capacity"`
	ReorderPoint        int64    `yaml:"reorder_point"`
	AutoReorder         bool     `yaml:"auto_reorder"`
	InboundSource       string   `yaml:"inbound_source,omitempty"`
	OutboundDestination string   `yaml:"outbound_destination,omitempty"`
	InboundStaging      string   `yaml:"inbound_staging,omitempty"`
	OutboundStaging     string   `yaml:"outbound_staging,omitempty"`
	AutoPush            bool     `yaml:"auto_push"`
	AutoConfirm         bool     `yaml:"auto_confirm"`
	PairedCoreNode      string   `yaml:"paired_core_node,omitempty"`
	AllowedPayloads     []string `yaml:"allowed_payloads,omitempty"`
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
