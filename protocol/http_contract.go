// The Edge↔Core HTTP contract: the request/response types for the direct
// HTTP calls shingo-edge's CoreClient makes on Core's public /api group.
//
// ONE TYPE PER WIRE SHAPE, BOTH SIDES OF THE SOCKET. Core's handlers encode
// these; the Edge decodes and encodes the very same types. Before this file
// the two sides each kept a private copy, and every field agreement was an
// accident that had held so far: Core wrote a map, Edge read a struct, and
// nothing connected the two except review. The copies are still visible at
// the Edge's call sites as aliases (`type X = protocol.X` in
// shingo-edge/engine/core_client.go) so the Edge's ~25 use sites compile
// unchanged; the definitions live here, once.
//
// BYTE SHAPE IS THE CONTRACT. These types replace map[string]any replies on
// Core's side, so the struct must emit exactly the bytes the map did:
//
//   - A map key is always written, whatever its value. Where the old map
//     wrote a key unconditionally the field here carries NO omitempty — a
//     zero value still writes ("as_of_station":"" and the null fences in
//     BinCountResponse are load-bearing: the Edge's count fence pins them).
//   - map[string]any encodes keys alphabetically. Fields replacing a map
//     reply are therefore declared alphabetically so the byte stream keeps
//     the old key order.
//   - A struct reply that already existed (NodeBinInfo) keeps its original
//     field order and tags verbatim.
//   - The one deviation, deliberate and decode-equivalent: the manifest
//     endpoint's empty-code/unknown-payload arms used to OMIT
//     bin_type_code while its other arms always wrote it (possibly "").
//     PayloadManifestResponse.BinTypeCode carries omitempty, so those two
//     arms drop "bin_type_code":"" — a key absent and a key present-but-
//     empty decode identically, and no Edge consumer reads the difference.
//
// The JSON field names are the keys Core's handlers read and wrote before
// this file existed; moving the types changed nothing on the wire.

package protocol

// NodeBinInfo describes the bin state at a single core node.
//
// BinID carries Core's bins.id so callers can thread the authoritative
// id into BinUOPDelta scopes — needed when the Edge order's BinID is
// nil at release time (REP / complex orders whose OrderDelivered didn't
// carry binID) and capture_reduction would otherwise be silently
// dropped at the BinID==0 gate.
type NodeBinInfo struct {
	NodeName    string `json:"node_name"`
	BinID       int64  `json:"bin_id,omitempty"`
	BinLabel    string `json:"bin_label,omitempty"`
	BinTypeCode string `json:"bin_type_code,omitempty"`
	// Bare is Core's bin_types.bare for the carrier: it holds no container,
	// so the only way out of the window is PUSH AS a real type.
	Bare         bool   `json:"bare,omitempty"`
	PayloadCode  string `json:"payload_code,omitempty"`
	UOPRemaining int    `json:"uop_remaining"`
	// DeltaEpoch is Core's bins.delta_epoch — bumps on every load-
	// lifecycle boundary (SetForProduction, ClearForReuseTx). Edge
	// stores it alongside the bin and stamps every outgoing
	// BinUOPDelta with the value cached here. On startup / cache miss
	// the field deserializes to 0, which Core treats as the bootstrap
	// sentinel and always applies. This used to say "the next bin-state
	// refresh from Core repopulates it" — there was no such refresh, and
	// what actually repopulates it is Core's reply to the first discarded
	// count (protocol.BinEpochRefresh).
	DeltaEpoch        int64   `json:"delta_epoch"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
}

// ManifestItem describes a single line in a payload manifest TEMPLATE.
//
// PartsPerCycle is a ratio — how many of the part one production cycle
// consumes, usually 1. The number physically in a bin is that times the bin's
// UoP count. The JSON key was `quantity` until Core's rename — same value, a
// name that says what it is. Core and Edge ship that rename together, so there
// is no version in which one key is read and the other written.
//
// This is NOT the shape a bin-load request carries — see BinLoadItem. The two
// were one struct, which is how a template ratio and a physical count came to
// share a field name.
type ManifestItem struct {
	PartNumber    string `json:"part_number"`
	PartsPerCycle int64  `json:"parts_per_cycle"`
	Description   string `json:"description"`
}

// BinLoadItem is a single line of a bin-load request: a part number and how
// many of it are actually in the carrier right now. A COUNT, not a ratio.
type BinLoadItem struct {
	PartNumber  string `json:"part_number"`
	Quantity    int64  `json:"quantity"`
	Description string `json:"description"`
}

// PayloadManifestResponse is the full response from Core's manifest endpoint.
//
// BinTypeCode lets press-index changeover detect "from bin type → to
// bin type" changes without a separate Core endpoint. Empty when Core
// has no payload_bin_types rule for this payload (the existing
// advisory pattern: no rules = any compatible bin). Empty value is
// treated by the planner as "unknown bin type" — the comparator falls
// back to "same" so the existing same-bin-type choreography ships.
//
// Field order (alphabetical) matches the map reply this struct replaced, so
// the byte stream keeps the old key order. Items must be non-nil to encode
// "items":[] rather than null.
type PayloadManifestResponse struct {
	BinTypeCode string         `json:"bin_type_code,omitempty"`
	Items       []ManifestItem `json:"items"`
	UOPCapacity int            `json:"uop_capacity"`
}

// NodeChildInfo describes a physical child node of an NGRP.
type NodeChildInfo struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
}

// BinLoadRequest is the request body for loading a bin via HTTP.
type BinLoadRequest struct {
	NodeName string `json:"node_name"`
	// BinID disambiguates when a node holds more than one bin; 0 means "the
	// one bin", the same shape as BinCountRequest and BinClearRequest. Absent
	// on the wire when 0, so a sender that never sets it sends the bytes it
	// always did.
	BinID       int64  `json:"bin_id,omitempty"`
	PayloadCode string `json:"payload_code"`
	// UOPCount is absent-or-value: a count is a count, absence is the
	// question. nil means nobody declared one and Core answers from the
	// payload's standard pack; a value is the count somebody counted, and 0
	// is a bin with nothing in it. It was a plain int64 where 0 carried both
	// meanings, so "I did not measure" and "I measured none" were the same
	// bytes on the wire and the receiver had to guess.
	UOPCount *int64        `json:"uop_count,omitempty"`
	Manifest []BinLoadItem `json:"manifest"`
}

// BinLoadResponse is Core's response after loading a bin.
type BinLoadResponse struct {
	// Field order alphabetical: this replaces a map reply, which encodes
	// keys alphabetically, and the byte stream keeps the old key order.
	BinID        int64  `json:"bin_id"`
	BinLabel     string `json:"bin_label"`
	DeltaEpoch   int64  `json:"delta_epoch"`
	Detail       string `json:"detail,omitempty"`
	Error        string `json:"error,omitempty"`
	PayloadCode  string `json:"payload_code"`
	Status       string `json:"status"`
	UOPRemaining int    `json:"uop_remaining"`
}

// BinCountRequest declares a count an operator made at the line against the
// bin at a node.
type BinCountRequest struct {
	NodeName string `json:"node_name"`
	// BinID disambiguates when a node holds more than one bin; 0 means "the
	// one bin", matching the conditional map key the Edge used to send.
	BinID     int64 `json:"bin_id,omitempty"`
	ActualUOP int   `json:"actual_uop"`
	// Actor has no omitempty: the old map wrote the key even for "".
	Actor string `json:"actor"`
}

// BinCountResponse is Core's reply to a count declared from the line.
type BinCountResponse struct {
	// Field order alphabetical: this replaces a map reply, which encodes
	// keys alphabetically, and the byte stream keeps the old key order.
	// Every old map key was written unconditionally, so no field here has
	// omitempty except the error envelope.
	AsOfNet      *int64 `json:"as_of_net"`
	AsOfSeq      *int64 `json:"as_of_seq"`
	AsOfStation  string `json:"as_of_station"`
	BinID        int64  `json:"bin_id"`
	BinLabel     string `json:"bin_label"`
	DeltaEpoch   int64  `json:"delta_epoch"`
	Detail       string `json:"detail,omitempty"`
	Discrepancy  bool   `json:"discrepancy"`
	Error        string `json:"error,omitempty"`
	Expected     int    `json:"expected"`
	Status       string `json:"status"`
	UOPRemaining int    `json:"uop_remaining"`
	Warning      string `json:"warning"`
}

// BinClearRequest clears the manifest on the bin at a node, optionally
// re-stamping the carrier's type in the same transaction.
type BinClearRequest struct {
	NodeName string `json:"node_name"`
	// BinID disambiguates when a node holds more than one bin; 0 means "the
	// one bin", matching the conditional map key the Edge used to send.
	BinID int64 `json:"bin_id,omitempty"`
	// BinTypeCode re-stamps the carrier's bin_type_id atomically with the
	// clear (dunnage floating). omitempty reproduces the old conditional
	// map key: absent when empty, present when set.
	BinTypeCode string `json:"bin_type_code,omitempty"`
}

// BinClearResponse is Core's reply to a bin clear.
//
// DeltaEpoch is the carrier's new generation stamp. Clearing a carrier for
// reuse ends its old life and starts a new one, and Core has always sent the
// new stamp straight back in this reply — the Edge decoded the status and
// threw the rest away, so it kept reporting counts under the stamp of a life
// that had ended and Core discarded every one of them.
//
// BinID names which carrier Core actually cleared. Core resolves that from
// its own view of the node, so it is not automatically the carrier the Edge
// believes is there; the stamp is only adopted when the two agree.
type BinClearResponse struct {
	// Field order alphabetical: this replaces a map reply, which encodes
	// keys alphabetically, and the byte stream keeps the old key order.
	// Every old map key was written unconditionally, so no field here has
	// omitempty except the error envelope.
	BinID    int64  `json:"bin_id"`
	BinLabel string `json:"bin_label"`
	// ClearedBinTypeCode is the carrier's cart type as an operator knows it —
	// the carrier, never a marker. Core reads it post-commit for the board's
	// CLEAR line; ClearBin logs it and nothing decides on it. Blank from a
	// Core that predates the field or could not read it.
	ClearedBinTypeCode string `json:"cleared_bin_type_code"`
	ClearedPayloadCode string `json:"cleared_payload_code"`
	DeltaEpoch         int64  `json:"delta_epoch"`
	Detail             string `json:"detail,omitempty"`
	Error              string `json:"error,omitempty"`
	Status             string `json:"status"`
}

// PreflightRequest is the payload list the Edge POSTs to Core's preflight
// inventory check before starting a changeover.
type PreflightRequest struct {
	Station  string   `json:"station"`
	Payloads []string `json:"payloads"`
}

// PreflightAvailability is the per-payload count in a preflight response.
type PreflightAvailability struct {
	PayloadCode string `json:"payload_code"`
	BinCount    int    `json:"bin_count"`
}

// PreflightResponse is the rolled-up preflight answer: the missing subset and
// the per-payload counts.
//
// Field order matches Core's service.PreflightResult (missing, absent,
// available) — this struct replaces a STRUCT reply, not a map, so it keeps
// the original order.
type PreflightResponse struct {
	Missing   []string                `json:"missing"`
	Absent    []string                `json:"absent"`
	Available []PreflightAvailability `json:"available"`
}

// SystemCountRequest is the payload list the Edge POSTs to Core's
// system-wide bin count.
type SystemCountRequest struct {
	Payloads []string `json:"payloads"`
}

// PayloadSystemCount is the per-payload count returned by SystemBinCount —
// total bins of one payload in the kanban loop.
type PayloadSystemCount struct {
	PayloadCode string `json:"payload_code"`
	BinCount    int    `json:"bin_count"`
}

// SystemBinCountResult carries per-payload counts. Payloads with zero
// bins are present in the result with BinCount=0 — callers should not
// assume absence means zero.
type SystemBinCountResult struct {
	Counts []PayloadSystemCount `json:"counts"`
}

// ReleasePointsRequest asks Core, for one act, what releasing each order would
// let its robot do next (POST /api/release/points).
type ReleasePointsRequest struct {
	StationID  string   `json:"station_id"`
	OrderUUIDs []string `json:"order_uuids"`
}

// ReleasePointsResponse answers ReleasePointsRequest, one point per order, in
// the request's order.
type ReleasePointsResponse struct {
	Points []ReleasePoint `json:"points"`
}

// ReleasePoint is the dynamic half of a release point (SHAPE §3.7).
type ReleasePoint struct {
	OrderUUID string `json:"order_uuid"`
	// Found is false when Core holds no such order for the station.
	Found bool `json:"found"`
	// Enters is every node the pending segment picks up at or drops at, in
	// order, with Core's redirects applied.
	Enters []string `json:"enters"`
	// AwaitsLift is each drop node another leg must lift a bin from first.
	AwaitsLift []LiftDependency `json:"awaits_lift"`
	// LinesideBin is the bin a release lifts off the order's line node, when
	// its segment picks up there; nil otherwise.
	LinesideBin *NodeBinInfo `json:"lineside_bin,omitempty"`
}

// LiftDependency: the order's next segment sets a bin down on Node, which
// holds a bin now, and the lifter (its Core sibling) lifts from Node before
// any drop of its own there. CoRelease is true when both may go in one act:
// the lifter is staged at the wait whose next segment contains that lift, and
// this order picks something up before it places on Node (SHAPE §3.4 (b)).
type LiftDependency struct {
	LifterUUID string `json:"lifter_uuid"`
	Node       string `json:"node"`
	CoRelease  bool   `json:"co_release"`
}
