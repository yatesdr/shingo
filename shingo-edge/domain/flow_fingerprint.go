package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"shingo/protocol"
)

// ── THE FINGERPRINT ──────────────────────────────────────────────────────────
//
// A preview is a plan over the rows as they were when the operator looked. A
// save or a start that runs over different rows is running a plan nobody
// looked at, so both carry the fingerprint the preview returned and are
// refused when it no longer matches.
//
// It is a pure hash of STORED rows — the process, the active style and its
// claims, the target style and its claims — recomputed from the database on
// every check and never stored. That is what makes it survive an Edge
// restart, refuse the second of two stations composing the same style, and
// turn a from-side cutover between save and start into PREVIEW STALE rather
// than a start over the wrong outgoing claims. Both sides are in it because
// the planner switches on the OUTGOING claim's mode.
//
// Claims are serialised on the columns cloneClaimColumns names
// (store/processes/styles.go) plus id — content and provenance — never
// source, called_by, updated_at, retired_at or the level's falling edge: who
// wrote a row and when is not the flow, and an attribution-only rewrite must
// not invalidate a preview.
//
// THE ROUTING SET IS NOT IN IT (owner, 2026-09-13). It is an OFFER LIST: it
// says which nodes the position panel may put in front of an operator, and
// nothing on the server refuses a saved claim that names a node outside it.
// So adopting or dropping a lane changed no stored flow and could not change
// what a plan would do, while invalidating every open preview on the press —
// and it put the most expensive query in the feature inside the save's
// exclusive transaction, where it was 37% of the hold.
//
// key_task is not in it either: the branch removed its one runtime forward,
// so editing a column that drives nothing was invalidating every open
// preview.

// FlowFingerprintColumns is the claim column list the fingerprint serialises,
// in order. It must equal cloneClaimColumns plus id, and a test in
// store/processes holds the two together.
//
// DERIVED FROM fingerprintClaim, NOT WRITTEN BESIDE IT. This was a third
// hand-maintained claim column list next to claimSelect and
// cloneClaimColumns — the shape that started the whole review — and the drift
// test could only compare two lists a human kept in step with a struct. Now
// the struct IS the list: its json tags are the column names, so a column
// added to the struct reaches the fingerprint and this list at once, and the
// drift test is left with the one thing it can still catch, a column the
// store has and the struct does not.
func FlowFingerprintColumns() []string {
	t := reflect.TypeOf(fingerprintClaim{})
	out := make([]string, 0, t.NumField())
	for i := 0; i < t.NumField(); i++ {
		name, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ",")
		if name != "" && name != "-" {
			out = append(out, name)
		}
	}
	return out
}

// fingerprintClaim is a claim on exactly FlowFingerprintColumns, in that
// order. The json tags ARE the column names; a column added to one list and
// not the other fails the drift test.
type fingerprintClaim struct {
	ID                             int64                `json:"id"`
	CoreNodeName                   string               `json:"core_node_name"`
	Role                           protocol.ClaimRole   `json:"role"`
	SwapMode                       protocol.SwapMode    `json:"swap_mode"`
	PayloadCode                    string               `json:"payload_code"`
	ReorderPoint                   int                  `json:"reorder_point"`
	ReorderPointSource             string               `json:"reorder_point_source"`
	AutoReorder                    bool                 `json:"auto_reorder"`
	InboundStaging                 string               `json:"inbound_staging"`
	OutboundStaging                string               `json:"outbound_staging"`
	InboundSource                  string               `json:"inbound_source"`
	OutboundDestination            string               `json:"outbound_destination"`
	AllowedPayloadCodes            []string             `json:"allowed_payload_codes"`
	AutoRequestPayload             string               `json:"auto_request_payload"`
	KeepStaged                     bool                 `json:"keep_staged"`
	EvacuateOnChangeover           bool                 `json:"evacuate_on_changeover"`
	PairedCoreNode                 string               `json:"paired_core_node"`
	AutoConfirm                    bool                 `json:"auto_confirm"`
	Sequence                       int                  `json:"sequence"`
	LinesideSoftThreshold          int                  `json:"lineside_soft_threshold"`
	SecondPairedCoreNode           string               `json:"second_paired_core_node"`
	ReuseCompatibleBins            bool                 `json:"reuse_compatible_bins"`
	AutoPush                       bool                 `json:"auto_push"`
	ChangeoverEvacNodes            []string             `json:"changeover_evac_nodes"`
	ChangeoverEvacDestination      string               `json:"changeover_evac_destination"`
	IndexRobotSupplies             bool                 `json:"index_robot_supplies"`
	KeyRoute                       []string             `json:"key_route"`
	ChangeoverCarryoverDisposition CarryoverDisposition `json:"changeover_carryover_disposition"`
	SourcePresetID                 *int64               `json:"source_preset_id"`
	SourcePresetVersion            *int                 `json:"source_preset_version"`
}

type fingerprintDoc struct {
	ProcessID   int64              `json:"process_id"`
	FromStyleID *int64             `json:"from_style_id"`
	From        []fingerprintClaim `json:"from"`
	ToStyleID   int64              `json:"to_style_id"`
	To          []fingerprintClaim `json:"to"`
}

// FlowFingerprint is the hex sha256 over the canonical JSON of the rows:
// process id, from style id, from claims sorted by core node, to style id, to
// claims sorted. Pure.
//
// It returns an error rather than panicking on a marshal failure. Every field
// is a string, number, bool, list or pointer to one, so the branch is not
// reachable today — but every caller is already returning an error on the
// line above it, and a panic on a request path is a worse answer to "this
// cannot happen" than an error nobody ever sees.
func FlowFingerprint(processID int64, fromStyleID *int64, fromClaims []NodeClaim, toStyleID int64, toClaims []NodeClaim) (string, error) {
	doc := fingerprintDoc{
		ProcessID:   processID,
		FromStyleID: clonePtr(fromStyleID),
		From:        fingerprintClaims(fromClaims),
		ToStyleID:   toStyleID,
		To:          fingerprintClaims(toClaims),
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return "", fmt.Errorf("flow fingerprint: marshal: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func fingerprintClaims(claims []NodeClaim) []fingerprintClaim {
	out := make([]fingerprintClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, fingerprintClaim{
			ID: c.ID, CoreNodeName: c.CoreNodeName, Role: c.Role, SwapMode: c.SwapMode, PayloadCode: c.PayloadCode,
			ReorderPoint: c.ReorderPoint, ReorderPointSource: c.ReorderPointSource, AutoReorder: c.AutoReorder,
			InboundStaging: c.InboundStaging, OutboundStaging: c.OutboundStaging,
			InboundSource: c.InboundSource, OutboundDestination: c.OutboundDestination,
			AllowedPayloadCodes: cloneStrings(c.AllowedPayloadCodes), AutoRequestPayload: c.AutoRequestPayload,
			KeepStaged: c.KeepStaged, EvacuateOnChangeover: c.EvacuateOnChangeover, PairedCoreNode: c.PairedCoreNode,
			AutoConfirm: c.AutoConfirm, Sequence: c.Sequence,
			LinesideSoftThreshold: c.LinesideSoftThreshold, SecondPairedCoreNode: c.SecondPairedCoreNode,
			ReuseCompatibleBins: c.ReuseCompatibleBins, AutoPush: c.AutoPush,
			ChangeoverEvacNodes: cloneStrings(c.ChangeoverEvacNodes), ChangeoverEvacDestination: c.ChangeoverEvacDestination,
			IndexRobotSupplies:             c.IndexRobotSupplies,
			KeyRoute:                       cloneStrings(c.KeyRoute),
			ChangeoverCarryoverDisposition: c.ChangeoverCarryoverDisposition,
			SourcePresetID:                 clonePtr(c.SourcePresetID), SourcePresetVersion: clonePtr(c.SourcePresetVersion),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CoreNodeName != out[j].CoreNodeName {
			return out[i].CoreNodeName < out[j].CoreNodeName
		}
		return out[i].ID < out[j].ID
	})
	return out
}
