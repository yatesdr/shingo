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
//
// Store and validation only for now; the UI and endpoints are a later unit.

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
// route through: anything outside its positions and the enabled members of
// its routing set.
var ErrFlowPresetUnknownNode = errors.New("not a position or a routing-set member of this process")

// ErrFlowPresetNameIsAnotherShape refuses a name a DIFFERENT shape already
// holds on this press (owner ruling F2, 2026-09-12).
//
// A repeated name is version n+1, which is right when the shape is the same
// one changing — that is what versions are for, and what a member claim's
// source_preset_version records. It is wrong when the shapes differ: the two
// lineages then share a name and cannot be pulled apart afterwards, because a
// rename moves every version of a name (R6). The suggested name being the mode
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

// flowPresetNodeKeys are the cell keys that name a single node. key_route is
// deliberately absent: a key route is validated against the vendor map, not
// the node list — 'waypoint' is not a routing role.
var flowPresetNodeKeys = []string{
	"core_node_name", "paired_core_node", "second_paired_core_node",
	"inbound_source", "outbound_destination", "changeover_evac_destination",
	"inbound_staging", "outbound_staging",
}

// flowPresetNodeListKeys are the cell keys that name a list of nodes.
var flowPresetNodeListKeys = []string{"changeover_evac_nodes"}

// ValidateFlowPreset checks a preset's flow_json against the nodes the
// process may route through (its positions plus the enabled members of its
// routing set): it must parse into at least one cell, no cell may carry a
// payload, and every node named must be in allowed. Pure — the store resolves
// allowed and calls this.
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
	for i, cell := range flow.Cells {
		label := fmt.Sprintf("cell %d", i+1)
		if name := rawString(cell["core_node_name"]); name != "" {
			label += " (" + name + ")"
		}
		for _, key := range flowPresetPayloadKeys {
			if rawPresent(cell[key]) {
				return fmt.Errorf("%s: %w", label, ErrFlowPresetHasPayload)
			}
		}
		for _, key := range flowPresetNodeKeys {
			name := strings.TrimSpace(rawString(cell[key]))
			if name != "" && !allowed[name] {
				return fmt.Errorf("%s: %s %q %w", label, key, name, ErrFlowPresetUnknownNode)
			}
		}
		for _, key := range flowPresetNodeListKeys {
			var names []string
			if raw := cell[key]; len(raw) > 0 {
				_ = json.Unmarshal(raw, &names)
			}
			for _, n := range names {
				name := strings.TrimSpace(n)
				if name != "" && !allowed[name] {
					return fmt.Errorf("%s: %s %q %w", label, key, name, ErrFlowPresetUnknownNode)
				}
			}
		}
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
