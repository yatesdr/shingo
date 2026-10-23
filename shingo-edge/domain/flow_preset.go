package domain

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ── FLOW PRESETS ─────────────────────────────────────────────────────────────
//
// A flow preset is a NAMED FLOW an engineer saves for a process: the cells of
// a flow — positions, choreography, sources, staging, destinations — with the
// parts left blank, so the floor can pick "the two-position press-index flow"
// for a new part instead of building it. A preset is a SHAPE; the part is
// chosen when the preset is applied, which is why a preset carrying a payload
// is refused outright.
//
// Presets are versioned and never edited in place: a claim expanded from one
// records (source_preset_id, source_preset_version) as provenance, and that
// reference has to keep meaning what it meant. Drift is computed from the
// claim rows themselves, never from the stored version — the preset is not
// a drift oracle.

// FlowPreset is one row of flow_presets.
type FlowPreset struct {
	ID         int64      `json:"id"`
	ProcessID  int64      `json:"process_id"`
	Name       string     `json:"name"`
	Version    int        `json:"version"`
	FlowJSON   string     `json:"flow_json"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
	ArchivedAt *time.Time `json:"archived_at,omitempty"`
}

// FlowPresetInput is the write shape. Version is assigned by the store (the
// next version of that name in that process); CreatedBy is stamped by the
// caller that owns the write.
type FlowPresetInput struct {
	ProcessID int64
	Name      string
	FlowJSON  string
	CreatedBy string
}

// ErrFlowPresetHasPayload refuses a preset whose cell carries a part. A
// preset is a shape; the part is chosen when it is applied.
var ErrFlowPresetHasPayload = errors.New("a preset carries no payload — the part is chosen when the preset is applied")

// ErrFlowPresetUnknownNode refuses a preset naming a node the process may not
// route through: anything outside its positions and the members of its
// routing set, switched on or off (RoutingSetNames, owner ruling R4).
var ErrFlowPresetUnknownNode = errors.New("not a position or a routing-set member of this process")

// ErrFlowPresetNameIsAnotherShape refuses a name a DIFFERENT recipe already
// holds on this press (owner ruling F2, 2026-09-12; re-keyed on the recipe's
// structure, not its nodes, 2026-10-22 — RecipeStructureKey).
//
// A repeated name is version n+1, which is right when the recipe is the same
// one changing — its nodes moved, its routing re-pointed — and that is what
// versions are for, and what a member claim's source_preset_version records.
// It is wrong when the structures differ: the two lineages then share a name
// and cannot be pulled apart afterwards, because a rename moves every version
// of a name (R6). The suggested name being the mode
// word alone (R5) makes the collision easy to walk into — two candidate shapes
// of one choreography suggest the same word — so the refusal is at naming time,
// where the engineer still has the field open.
var ErrFlowPresetNameIsAnotherShape = errors.New("is already a different shape — pick another name")

// ErrFlowPresetMalformed refuses a flow_json that does not parse into at least
// one cell.
var ErrFlowPresetMalformed = errors.New("flow_json must be an object with a non-empty cells array")

// flowPresetPayloadKeys are the cell keys a part could arrive under.
//
// BOTH, AND THE SECOND ONE IS A GUARD RATHER THAN A SHAPE. FlowCell's tag is
// payload_code and nothing on this tree emits `payload`, so the short form is
// not a spelling this accepts — it is one it REFUSES, which is a different
// thing and the reason it is listed. A preset is a shape; a body that smuggles
// a part in under a name the decoder would drop is exactly what this validator
// is for, and TestFlowPreset_RefusesAPayload is the case.
var flowPresetPayloadKeys = []string{"payload", "payload_code"}

// presetNodeFields are the cell fields that name a node, in the order they are
// checked, each read as the nodes it names (changeover_evac_nodes names a
// list). key_route is deliberately absent: a key route is validated against
// the vendor map, not the node list — 'waypoint' is not a routing role.
var presetNodeFields = []struct {
	key   string
	nodes func(FlowCell) []string
}{
	{"core_node_name", func(c FlowCell) []string { return []string{c.CoreNodeName} }},
	{"paired_core_node", func(c FlowCell) []string { return []string{c.PairedCoreNode} }},
	{"second_paired_core_node", func(c FlowCell) []string { return []string{c.SecondPairedCoreNode} }},
	{"inbound_source", func(c FlowCell) []string { return []string{c.InboundSource} }},
	{"outbound_destination", func(c FlowCell) []string { return []string{c.OutboundDestination} }},
	{"changeover_evac_destination", func(c FlowCell) []string { return []string{c.ChangeoverEvacDestination} }},
	{"inbound_staging", func(c FlowCell) []string { return []string{c.InboundStaging} }},
	{"outbound_staging", func(c FlowCell) []string { return []string{c.OutboundStaging} }},
	{"changeover_evac_nodes", func(c FlowCell) []string { return c.ChangeoverEvacNodes }},
}

// StaleNode is one node a recipe names that the process does not have: the
// cell it is in (1-based, in the recipe's order), the field and the node.
type StaleNode struct {
	Cell  int
	Field string
	Node  string
}

// StaleNodes is every node the cells name that is not in live (RoutingSetNames:
// the process's live positions and every member of its routing set), in cell
// then field order. It is the check ValidateFlowPreset makes when a recipe is
// saved, asked again wherever the recipe is read, because a node deleted since
// leaves a stored recipe naming a node the process no longer has (Springfield
// Press 6, 2026-10-21: "Two spot press index" still sends cell 2 to ULN_003).
func StaleNodes(cells []FlowCell, live map[string]bool) []StaleNode {
	var out []StaleNode
	for i, c := range cells {
		for _, f := range presetNodeFields {
			for _, n := range f.nodes(c) {
				if name := strings.TrimSpace(n); name != "" && !live[name] {
					out = append(out, StaleNode{Cell: i + 1, Field: f.key, Node: name})
				}
			}
		}
	}
	return out
}

// StaleSentence is a stale node in the words the floor and the desktop show,
// naming the next act: "Two spot press index sends cell 2 to ULN_003, which
// is no longer a position here — an engineer re-saves the recipe on the
// desktop". A source is a routing node, so its sentence says the routing set.
func StaleSentence(recipe string, s StaleNode) string {
	cell := fmt.Sprintf("cell %d", s.Cell)
	gone := "which is no longer a position here"
	var says string
	switch s.Field {
	case "outbound_destination", "changeover_evac_destination":
		says = "sends " + cell + " to " + s.Node
	case "inbound_source":
		says, gone = "fills "+cell+" from "+s.Node, "which is no longer in this process's routing set"
	case "core_node_name":
		says = "runs " + cell + " on " + s.Node
	default:
		says = "names " + s.Node + " in " + cell
	}
	return recipe + " " + says + ", " + gone + " — an engineer re-saves the recipe on the desktop"
}

// RoutingSetOf is RoutingSetNames from rows already read: the live positions
// and every member of the routing set, adopted or not (R4). The store's SQL
// and this answer the same question; TestRoutingSetOf_IsRoutingSetNames holds
// them together.
func RoutingSetOf(positions []Node, routing []RoutingNode) map[string]bool {
	out := map[string]bool{}
	for _, p := range positions {
		if p.DeletedAt != nil {
			continue
		}
		if n := strings.TrimSpace(p.CoreNodeName); n != "" {
			out[n] = true
		}
	}
	for _, r := range routing {
		if n := strings.TrimSpace(r.CoreNodeName); n != "" {
			out[n] = true
		}
	}
	return out
}

// ValidateFlowPreset checks a preset's flow_json against the nodes the
// process may route through (its positions plus every member of its routing
// set, switched on or off): it must parse into at least one cell, no cell may
// carry a payload, and every node named must be in allowed (StaleNodes). Pure
// — the store resolves allowed and calls this. Every cell's payload is checked
// before any cell's nodes.
func ValidateFlowPreset(flowJSON string, allowed map[string]bool) error {
	var flow struct {
		Cells []map[string]json.RawMessage `json:"cells"`
	}
	if err := json.Unmarshal([]byte(flowJSON), &flow); err != nil {
		return fmt.Errorf("%w: %v", ErrFlowPresetMalformed, err)
	}
	if len(flow.Cells) == 0 {
		return ErrFlowPresetMalformed
	}
	label := func(i int) string {
		l := fmt.Sprintf("cell %d", i+1)
		if name := rawString(flow.Cells[i]["core_node_name"]); name != "" {
			l += " (" + name + ")"
		}
		return l
	}
	for i, cell := range flow.Cells {
		for _, key := range flowPresetPayloadKeys {
			if rawPresent(cell[key]) {
				return fmt.Errorf("%s: %w", label(i), ErrFlowPresetHasPayload)
			}
		}
	}
	cells, err := ParsePresetShape(flowJSON)
	if err != nil {
		return err
	}
	if stale := StaleNodes(cells, allowed); len(stale) > 0 {
		s := stale[0]
		return fmt.Errorf("%s: %s %q %w", label(s.Cell-1), s.Field, s.Node, ErrFlowPresetUnknownNode)
	}
	return nil
}

// rawString returns a JSON string value, or "" for absent / non-string.
func rawString(raw json.RawMessage) string {
	var s string
	if len(raw) == 0 || json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return s
}

// rawPresent reports whether a JSON value is there and carries something:
// absent, null and "" are not a payload; anything else is.
func rawPresent(raw json.RawMessage) bool {
	t := strings.TrimSpace(string(raw))
	return t != "" && t != "null" && t != `""`
}
