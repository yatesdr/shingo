// Package scenefixtures holds two synthetic plants — A and B — for tests on
// either side of the wire that need a realistic scene.
//
// SYNTHETIC, NOT HAND-BUILT, AND NOT A PULL. Owner ruling 2026-09-13: no plant
// data or information in the repo. But a hand-typed fixture gets the join
// right by accident. The node→map join is scene_points.instance_name WHERE
// class_name = 'GeneralLocation', and a join on `label` looks right at one
// plant (A: 60 of 383 points labelled) while silently rendering the other
// empty (B: 12 of 350). A scene small enough to type is one in which both
// joins work, and a test on it cannot tell them apart. Likewise the vendor's
// class string does not partition the geometry — 494 of B's 588 segments say
// DegenerateBezier and only the handles say which of them bow — so a test that
// draws the scene needs the handles a vendor actually stores.
//
// So these are DERIVED: scripts/anonymise-plant-pull.py reads a real pull held
// outside the repo and emits the fixture, keeping row counts, every id and
// structural relation, which points carry a label, the vendor's class strings
// and handles, the dirty rows (orphan claims, claims on soft-deleted styles,
// the claim with no process row) and relative timestamp order — and replacing
// part numbers, payload codes, style/process/station names, PLC and tag names,
// CATIDs, every timestamp, and the floor plan itself, which moves under one
// rigid motion per plant so distances survive and the map is not the map.
// scene_points.properties_json is dropped rather than rewritten; the script
// says why. The script is deterministic, so a re-derivation is a no-op diff.
//
// PROVENANCE is the script and nothing else. No hosts, no pull dates, no
// plant names: A is not Hopkinsville and B is not Springfield, they are the
// two SHAPES those plants have. Row counts: A 383 points / 635 edges, B 350 /
// 588, full-table. Column names are verbatim from the source schema and every
// column is carried, so the store's verbatim-insert tests can use the same
// bytes the typed decode ignores.
//
// It lives in shared/ because both shingo-core and shingo-edge tests read it
// and neither module can import the other — the same reason loadervectors is
// here. Embedded rather than read by path so a test is independent of its
// working directory and a missing file is a compile error.
package scenefixtures

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
)

//go:embed a-press-setups.json
var rawA []byte

//go:embed b-press-setups.json
var rawB []byte

// Plant is one synthetic plant. Field names mirror the source tables, and the
// decode ignores the columns it does not name — the file carries all of them.
type Plant struct {
	Plant            string            `json:"plant"`
	Note             string            `json:"note"`
	Processes        []Process         `json:"processes"`
	OperatorStations []OperatorStation `json:"operator_stations"`
	ProcessNodes     []ProcessNode     `json:"process_nodes"`
	Styles           []Style           `json:"styles"`
	StyleNodeClaims  []StyleNodeClaim  `json:"style_node_claims"`
	ScenePoints      []ScenePoint      `json:"scene_points"`
	SceneEdges       []SceneEdge       `json:"scene_edges"`
	Nodes            []Node            `json:"nodes"`
	PayloadCatalog   []CatalogEntry    `json:"payload_catalog"`
	Payloads         []Payload         `json:"payloads"`
	PayloadBinTypes  []PayloadBinType  `json:"payload_bin_types"`
}

// Process is an Edge processes row.
type Process struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	ActiveStyleID *int64 `json:"active_style_id"`
}

// OperatorStation is an Edge operator_stations row.
type OperatorStation struct {
	ID        int64  `json:"id"`
	ProcessID int64  `json:"process_id"`
	Code      string `json:"code"`
	Name      string `json:"name"`
}

// ProcessNode is an Edge process_nodes row. OperatorStationID is nil on a
// press-index back position that was auto-created without a station.
type ProcessNode struct {
	ID                int64   `json:"id"`
	ProcessID         int64   `json:"process_id"`
	OperatorStationID *int64  `json:"operator_station_id"`
	CoreNodeName      string  `json:"core_node_name"`
	Code              string  `json:"code"`
	Name              string  `json:"name"`
	Sequence          int     `json:"sequence"`
	Enabled           int     `json:"enabled"`
	DeletedAt         *string `json:"deleted_at"`
}

// Style is an Edge styles row.
type Style struct {
	ID        int64   `json:"id"`
	Name      string  `json:"name"`
	ProcessID int64   `json:"process_id"`
	DeletedAt *string `json:"deleted_at"`
}

// StyleNodeClaim is an Edge style_node_claims row, the columns the picture
// reads. JSON-text columns are left as the strings the database holds.
type StyleNodeClaim struct {
	ID                        int64  `json:"id"`
	StyleID                   int64  `json:"style_id"`
	CoreNodeName              string `json:"core_node_name"`
	Role                      string `json:"role"`
	SwapMode                  string `json:"swap_mode"`
	PayloadCode               string `json:"payload_code"`
	UOPCapacity               int    `json:"uop_capacity"`
	InboundStaging            string `json:"inbound_staging"`
	OutboundStaging           string `json:"outbound_staging"`
	InboundSource             string `json:"inbound_source"`
	OutboundDestination       string `json:"outbound_destination"`
	PairedCoreNode            string `json:"paired_core_node"`
	SecondPairedCoreNode      string `json:"second_paired_core_node"`
	AllowedPayloadCodes       string `json:"allowed_payload_codes"`
	KeyRoute                  string `json:"key_route"`
	ChangeoverEvacDestination string `json:"changeover_evac_destination"`
}

// ScenePoint is a Core scene_points row.
type ScenePoint struct {
	ID           int64   `json:"id"`
	AreaName     string  `json:"area_name"`
	InstanceName string  `json:"instance_name"`
	ClassName    string  `json:"class_name"`
	PointName    string  `json:"point_name"`
	Label        string  `json:"label"`
	PosX         float64 `json:"pos_x"`
	PosY         float64 `json:"pos_y"`
	Dir          float64 `json:"dir"`
	SyncedAt     string  `json:"synced_at"`
}

// SceneEdge is a Core scene_edges row. The control handles are nullable and
// null on every StraightPath row, exactly as the database holds them.
type SceneEdge struct {
	ID           int64    `json:"id"`
	AreaName     string   `json:"area_name"`
	InstanceName string   `json:"instance_name"`
	ClassName    string   `json:"class_name"`
	FromName     string   `json:"from_name"`
	ToName       string   `json:"to_name"`
	FromX        float64  `json:"from_x"`
	FromY        float64  `json:"from_y"`
	ToX          float64  `json:"to_x"`
	ToY          float64  `json:"to_y"`
	Ctrl1X       *float64 `json:"ctrl1_x"`
	Ctrl1Y       *float64 `json:"ctrl1_y"`
	Ctrl2X       *float64 `json:"ctrl2_x"`
	Ctrl2Y       *float64 `json:"ctrl2_y"`
	SyncedAt     string   `json:"synced_at"`
}

// Node is a Core nodes row with its type code and parent name joined in.
type Node struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	NodeTypeCode *string `json:"node_type_code"`
	ParentName   *string `json:"parent_name"`
}

// CatalogEntry is an Edge payload_catalog row. A claim's UOP capacity is
// resolved from this table rather than stored on the claim, so a plant seeded
// without it reads every capacity as 0 — which is why SeedPlant loads it.
type CatalogEntry struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Code        string `json:"code"`
	UOPCapacity int    `json:"uop_capacity"`
}

// Payload is a Core payloads row, id and code only.
type Payload struct {
	ID   int64  `json:"id"`
	Code string `json:"code"`
}

// PayloadBinType is a Core payload_bin_types row with the bin-type code joined.
type PayloadBinType struct {
	PayloadID   int64  `json:"payload_id"`
	BinTypeID   int64  `json:"bin_type_id"`
	BinTypeCode string `json:"bin_type_code"`
}

// A returns plant A: three processes, 12 styles, 383 scene points of which 60
// carry a label. The plant whose label join looks like it works.
func A() Plant { return mustParse("a", rawA) }

// B returns plant B: fourteen processes, 125 styles, 350 scene points of which
// 12 carry a label. The plant that catches a join on `label`, and the one with
// the dirty claim rows the routing-set backfill exists for.
func B() Plant { return mustParse("b", rawB) }

// RawA and RawB are the fixture bytes, for the tests that insert rows VERBATIM
// rather than through the typed decode above — the routing-set backfill needs
// columns the picture never reads, and needs them unvalidated, because the
// point is the rows as a plant holds them. A copy, so a caller that rewrites
// one cannot reach the next test through the embedded slice.
func RawA() []byte { return bytes.Clone(rawA) }

// RawB is RawA for plant B.
func RawB() []byte { return bytes.Clone(rawB) }

func mustParse(name string, raw []byte) Plant {
	var p Plant
	if err := json.Unmarshal(raw, &p); err != nil {
		panic(fmt.Sprintf("scenefixtures: %s fixture does not parse: %v", name, err))
	}
	return p
}

// PayloadBinTypeCodes joins payload_bin_types to payloads and returns
// payload code → bin-type codes, the shape the wire carries.
func (p Plant) PayloadBinTypeCodes() map[string][]string {
	byID := make(map[int64]string, len(p.Payloads))
	for _, pl := range p.Payloads {
		byID[pl.ID] = pl.Code
	}
	out := map[string][]string{}
	for _, r := range p.PayloadBinTypes {
		if code, ok := byID[r.PayloadID]; ok {
			out[code] = append(out[code], r.BinTypeCode)
		}
	}
	return out
}
