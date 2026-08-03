package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shingoedge/service"
)

// NodeBinInfo describes the bin state at a single core node.
//
// BinID carries Core's bins.id so callers can thread the authoritative
// id into BinUOPDelta scopes — needed when the Edge order's BinID is
// nil at release time (REP / complex orders whose OrderDelivered didn't
// carry binID) and capture_reduction would otherwise be silently
// dropped at the BinID==0 gate.
type NodeBinInfo struct {
	NodeName     string `json:"node_name"`
	BinID        int64  `json:"bin_id,omitempty"`
	BinLabel     string `json:"bin_label,omitempty"`
	BinTypeCode  string `json:"bin_type_code,omitempty"`
	PayloadCode  string `json:"payload_code,omitempty"`
	UOPRemaining int    `json:"uop_remaining"`
	// DeltaEpoch is Core's bins.delta_epoch — bumps on every load-
	// lifecycle boundary (SetForProduction, ClearForReuseTx). Edge
	// stores it alongside the bin and stamps every outgoing
	// BinUOPDelta with the value cached here. On startup / cache miss
	// the field deserializes to 0; the next bin-state refresh from
	// Core repopulates it before Edge emits its first delta.
	DeltaEpoch        int64   `json:"delta_epoch"`
	Manifest          *string `json:"manifest,omitempty"`
	ManifestConfirmed bool    `json:"manifest_confirmed"`
	Occupied          bool    `json:"occupied"`
}

// CoreClient makes lightweight HTTP requests to Core's telemetry API.
type CoreClient struct {
	baseURL string
	http    *http.Client
}

// NewCoreClient creates a CoreClient. baseURL may be empty (disabled).
func NewCoreClient(baseURL string) *CoreClient {
	return &CoreClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		http: &http.Client{
			Timeout: 3 * time.Second,
		},
	}
}

// SetBaseURL updates the Core API base URL (e.g. after config change).
func (c *CoreClient) SetBaseURL(url string) {
	c.baseURL = strings.TrimRight(url, "/")
}

// Available returns true if a Core API URL is configured. Nil-safe so test
// engines that don't wire a CoreClient still report unavailable rather than
// panicking through callers that probe Core telemetry.
func (c *CoreClient) Available() bool {
	return c != nil && c.baseURL != ""
}

// ManifestItem describes a single line in a payload manifest template.
type ManifestItem struct {
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
type PayloadManifestResponse struct {
	UOPCapacity int            `json:"uop_capacity"`
	Items       []ManifestItem `json:"items"`
	BinTypeCode string         `json:"bin_type_code,omitempty"`
}

// FetchPayloadManifest returns the default manifest template and UOP capacity for a payload code.
// Returns nil if Core is unavailable or the payload doesn't exist.
func (c *CoreClient) FetchPayloadManifest(payloadCode string) (*PayloadManifestResponse, error) {
	if c.baseURL == "" || payloadCode == "" {
		return nil, nil
	}
	reqURL := c.baseURL + "/api/telemetry/payload/" + url.PathEscape(payloadCode) + "/manifest"
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var result PayloadManifestResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return &result, nil
}

// NodeChildInfo describes a physical child node of an NGRP.
type NodeChildInfo struct {
	Name     string `json:"name"`
	NodeType string `json:"node_type"`
}

// FetchNodeChildren returns the direct children of an NGRP node.
// When includeSynthetic is true, synthetic children (e.g. LANE nodes) are included;
// otherwise only physical non-synthetic children are returned (the existing default).
// Returns nil if Core is unavailable or the node has no matching children.
func (c *CoreClient) FetchNodeChildren(nodeName string, includeSynthetic bool) ([]NodeChildInfo, error) {
	if c.baseURL == "" || nodeName == "" {
		return nil, nil
	}
	reqURL := c.baseURL + "/api/telemetry/node/" + url.PathEscape(nodeName) + "/children"
	if includeSynthetic {
		reqURL += "?include_synthetic=true"
	}
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var result []NodeChildInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return result, nil
}

// BinUOPRow mirrors service.BinUOPRow on Core.
//
// NOTHING READS ANY OF THESE FIELDS. The one caller of FetchUOPState reads
// the bucket half of the response and never touches the bins array. The
// header used to say "the reconciler's self-heal path reads it to align
// local runtime cache with Core's authoritative bin count" — there is no
// such reconciler; it was deleted, and the sentence outlived it.
//
// Kept rather than deleted, deliberately: the fields cost nothing, Core
// genuinely computes and ships them, and this is the fallback shape if the
// push-and-repair loop turns out to need a pull-side backstop at a plant.
// Deleting is the only irreversible move available here. If it goes, it goes
// as its own change, on the strength of its own no-consumer check, and not as
// a side effect of tidying something else. (That check has now been made once,
// 2026-08-02, and came back zero — recorded so the next person does not have
// to redo it, not as licence to delete without redoing it.)
type BinUOPRow struct {
	BinID        int64  `json:"bin_id"`
	NodeName     string `json:"node_name"`
	PayloadCode  string `json:"payload_code"`
	UOPRemaining int    `json:"uop_remaining"`
	// DeltaEpoch mirrors Core's bins.delta_epoch. The comment here used to
	// describe it as "populated on startup-time reconciliation so Edge can
	// repair a lost bin-state cache" — describing a repair subsystem that has
	// never existed. The real repair is push-shaped: Core announces every
	// generation change and answers every discarded count (see
	// protocol.BinEpochRefresh).
	DeltaEpoch int64 `json:"delta_epoch"`
}

// LinesideBucketRow mirrors service.LinesideBucketRow on Core. Edge
// compares each row against its local node_lineside_bucket table to
// surface bucket-side drift. Item 14 (D6) dropped the NodeID field —
// the reconciler resolves Edge node ids by looking up NodeName in the
// local nodeByName map, so Core's internal NodeID is decorative
// here. Core's wire struct keeps it for parity with database joins.
type LinesideBucketRow struct {
	NodeName   string `json:"node_name"`
	PairKey    string `json:"pair_key"`
	StyleID    int64  `json:"style_id"`
	PartNumber string `json:"part_number"`
	Qty        int    `json:"qty"`
}

// UOPStateResponse is the wire shape for /api/telemetry/uop-state.
type UOPStateResponse struct {
	Bins    []BinUOPRow         `json:"bins"`
	Buckets []LinesideBucketRow `json:"buckets"`
}

// FetchUOPState returns the authoritative bin + bucket snapshot from Core, and
// nil (no error) when Core is unavailable — a missed reconciliation pass is not
// worth surfacing, because the next one reads the same truth.
//
// This used to say it was "matching FetchNodeBins's graceful-degradation
// contract". FetchNodeBins no longer has one: it returns (bins, reachable, err)
// precisely because collapsing every failure into "nothing here" is what let a
// failed occupancy read pass as an empty loader window (the 2026-07-31
// Springfield over-ordering incident). Do not copy this function's shape into a
// caller that ACTS on absence; it is safe here only because the reconciler's
// answer to a missing snapshot is to wait for the next one.
func (c *CoreClient) FetchUOPState(station string, nodeNames []string) (*UOPStateResponse, error) {
	if c.baseURL == "" {
		return nil, nil
	}
	params := url.Values{}
	if station != "" {
		params.Set("station", station)
	}
	if len(nodeNames) > 0 {
		params.Set("nodes", strings.Join(nodeNames, ","))
	}
	resp, err := c.http.Get(c.baseURL + "/api/telemetry/uop-state?" + params.Encode())
	if err != nil {
		return nil, nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var result UOPStateResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return &result, nil
}

// BinAtLineside is the tri-state lookup the reconciler needs at
// the Phase 3 authority flip. Returns:
//
//   - (bin, true, nil)  — Core confirms a bin is present at the node;
//     bin.UOPRemaining is the authoritative count.
//   - (nil, true, nil)  — Core confirms no bin at the node; the
//     reconciler should set local runtime to 0.
//   - (nil, false, err) — Core unreachable (network error, non-200,
//     decode failure). The reconciler MUST retain the prior cached
//     value rather than zeroing — otherwise a transient Core blip
//     would zero every lineside on every retry. This is the B2 fix
//     from plan §2.6.
//
// This was written as the exception: FetchNodeBins collapsed every failure
// into (nil, nil), and this function existed so the ONE caller that could not
// survive that — the reconciler self-heal — had something better. The note here
// used to say the collapse was fine for everyone else, "HMI telemetry where a
// temporary nil-vs-occupied flicker is acceptable". It was not fine. The
// reservation seam is one of those other callers, and the flicker read as a free
// loader window; that is the 2026-07-31 Springfield over-ordering incident.
//
// FetchNodeBins now carries the same three states, so this is no longer an
// exception — it is the single-node convenience wrapper, and the two agree.
func (c *CoreClient) BinAtLineside(nodeName string) (*NodeBinInfo, bool, error) {
	if c.baseURL == "" {
		return nil, false, fmt.Errorf("core API not configured")
	}
	if nodeName == "" {
		return nil, false, fmt.Errorf("node name is required")
	}
	params := url.Values{}
	params.Set("nodes", nodeName)
	resp, err := c.http.Get(c.baseURL + "/api/telemetry/node-bins?" + params.Encode())
	if err != nil {
		return nil, false, fmt.Errorf("fetch node-bins for %q: %w", nodeName, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("fetch node-bins for %q: HTTP %d", nodeName, resp.StatusCode)
	}
	var rows []NodeBinInfo
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, false, fmt.Errorf("decode node-bins for %q: %w", nodeName, err)
	}
	// Find the row for the requested node. Core returns one row per
	// requested node even when unoccupied (Occupied=false).
	for i := range rows {
		if rows[i].NodeName == nodeName {
			if !rows[i].Occupied {
				// Confirmed empty — distinct from Core-unreachable.
				return nil, true, nil
			}
			r := rows[i]
			return &r, true, nil
		}
	}
	// HTTP succeeded but the requested node didn't appear in the
	// response. Treat as confirmed empty — Core would have included
	// the row if the node were known. A typo in the node name lands
	// here too; surfaces via reconciler "no bin at slot" → set to 0.
	return nil, true, nil
}

// The ways an occupancy read can fail to produce an answer. Named so a caller
// can tell "Core says the window is empty" from "Core did not say", and so the
// decision log can record which of the two happened.
var (
	// ErrCoreNotConfigured — no Core API address. Not a failure; there is
	// simply nobody to ask.
	ErrCoreNotConfigured = errors.New("core API not configured")
	// ErrCoreUnreachable — the request never got an answer (transport error).
	ErrCoreUnreachable = errors.New("core unreachable")
	// ErrCoreHTTPStatus — Core answered, but not with 200.
	ErrCoreHTTPStatus = errors.New("core returned a non-200")
	// ErrCoreUndecodable — 200 with a body that would not parse: a truncated
	// or corrupted write.
	ErrCoreUndecodable = errors.New("core response could not be decoded")
)

// FetchNodeBins returns bin state for the given core node names.
//
// Three states, mirroring BinAtLineside:
//
//   - (bins, true, nil)   — Core answered; this list is what is there. An empty
//     list from a non-empty request means Core knows of no bin at those nodes.
//   - (nil, false, err)   — nobody answered. Core is not configured, or the
//     request errored, or the status was not 200, or the body would not decode.
//     `err` says which; the sentinels above match with errors.Is.
//
// It used to return (nil, nil) for all four failures, so "Core could not
// answer" was indistinguishable from "no bin is present" at every call site.
// That collapse is what produced the 2026-07-31 Springfield over-ordering
// incident: the reservation seam read an unanswerable occupancy check as an
// empty window and fired an empty into a window that already held one. A
// caller can now tell, and a caller that acts on absence is choosing to.
//
// Asking about zero nodes returns (nil, true, nil) — a complete answer to an
// empty question, not a failed read.
func (c *CoreClient) FetchNodeBins(nodeNames []string) ([]NodeBinInfo, bool, error) {
	if len(nodeNames) == 0 {
		return nil, true, nil
	}
	if c.baseURL == "" {
		return nil, false, ErrCoreNotConfigured
	}
	params := url.Values{}
	params.Set("nodes", strings.Join(nodeNames, ","))
	reqURL := c.baseURL + "/api/telemetry/node-bins?" + params.Encode()
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, false, fmt.Errorf("%w: fetch node-bins: %w", ErrCoreUnreachable, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false, fmt.Errorf("%w: fetch node-bins: HTTP %d", ErrCoreHTTPStatus, resp.StatusCode)
	}
	var result []NodeBinInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, false, fmt.Errorf("%w: decode node-bins: %w", ErrCoreUndecodable, err)
	}
	return result, true, nil
}

// OccupancyOutcome names what happened to an occupancy read, for the decision
// log. One word per outcome so an over-ordering incident can be reconstructed
// from logs alone — "resident=0" means nothing until you know whether anyone
// was asked.
func OccupancyOutcome(reachable bool, err error) string {
	switch {
	case reachable:
		return "ok"
	case errors.Is(err, ErrCoreNotConfigured):
		return "not_configured"
	case errors.Is(err, ErrCoreUnreachable):
		return "unreachable"
	case errors.Is(err, ErrCoreHTTPStatus):
		return "http_error"
	case errors.Is(err, ErrCoreUndecodable):
		return "decode_err"
	default:
		return "unverifiable"
	}
}

// BinLoadRequest is the request body for loading a bin via HTTP.
type BinLoadRequest struct {
	NodeName    string         `json:"node_name"`
	PayloadCode string         `json:"payload_code"`
	UOPCount    int64          `json:"uop_count"`
	Manifest    []ManifestItem `json:"manifest"`
}

// BinLoadResponse is Core's response after loading a bin.
type BinLoadResponse struct {
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	BinID        int64  `json:"bin_id,omitempty"`
	BinLabel     string `json:"bin_label,omitempty"`
	PayloadCode  string `json:"payload_code,omitempty"`
	UOPRemaining int    `json:"uop_remaining,omitempty"`
	// DeltaEpoch is the new bins.delta_epoch SetForProduction returned.
	// Edge caches it and stamps subsequent BinUOPDeltas against this
	// bin with the value, so Core's epoch-aware dedup accepts them.
	DeltaEpoch int64 `json:"delta_epoch,omitempty"`
}

// LoadBin sets the manifest on the bin at a node via Core's HTTP API.
// Unlike telemetry reads, this returns errors on failure since it is a write operation.
func (c *CoreClient) LoadBin(req *BinLoadRequest) (*BinLoadResponse, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal bin-load request: %w", err)
	}
	resp, err := c.http.Post(c.baseURL+"/api/telemetry/bin-load", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bin-load request failed: %w", err)
	}
	defer resp.Body.Close()
	var result BinLoadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode bin-load response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Status == "error" {
		detail := result.Detail
		if detail == "" {
			detail = fmt.Sprintf("core returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", detail)
	}
	return &result, nil
}

// PreflightInventory POSTs the to-style's required payload list to Core's
// /api/inventory/preflight and returns the per-payload availability +
// missing subset. Used by service.PreflightChecker to gate
// StartProcessChangeover when bins are missing.
//
// Returns service.PreflightCoreResult so the service-package interface
// PreflightCorePoster can be satisfied without an engine→service→engine
// import cycle.
//
// Unlike the read-only telemetry calls above this returns a hard error on
// network failure: a preflight that silently degrades to "all available"
// would defeat the whole point of the gate.
func (c *CoreClient) PreflightInventory(station string, payloads []string) (*service.PreflightCoreResult, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	reqBody := struct {
		Station  string   `json:"station"`
		Payloads []string `json:"payloads"`
	}{Station: station, Payloads: payloads}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshal preflight request: %w", err)
	}
	resp, err := c.http.Post(c.baseURL+"/api/inventory/preflight", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("preflight request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("preflight: core returned %d", resp.StatusCode)
	}
	// Decode into the wire shape that matches Core's JSON. The wire-side
	// struct is local because it carries the json tags Core sends; copy
	// fields into the service-package result type for the return.
	var wire struct {
		Missing   []string `json:"missing"`
		Available []struct {
			PayloadCode string `json:"payload_code"`
			BinCount    int    `json:"bin_count"`
		} `json:"available"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode preflight response: %w", err)
	}
	out := &service.PreflightCoreResult{Missing: wire.Missing}
	out.Available = make([]service.PreflightCoreAvailability, len(wire.Available))
	for i, a := range wire.Available {
		out.Available[i] = service.PreflightCoreAvailability{PayloadCode: a.PayloadCode, BinCount: a.BinCount}
	}
	return out, nil
}

// SystemBinCount POSTs a payload list to Core's /api/inventory/system-count
// and returns the per-payload total bin count using the "in the kanban
// loop" inclusion policy (see shingo-core/service/inventory_system_count.go).
//
// This intentionally answers a different question than PreflightInventory:
// total physical bins in circulation regardless of location or pickability,
// not just bins-available-to-source-right-now. Used by inventory math where a bin staged at the
// consumer line still counts as in circulation.
//
// Returns ([]PayloadSystemCount, true) on success, (nil, false) when Core
// is unreachable or returns an error. Callers fail OPEN at the use site
// (treat as zero): a missed signal leaves the loader idle; a redundant
// signal is dedup'd by the in-flight guard plus Core's dropoff-capacity
// gate. Idle is the worse outcome.
func (c *CoreClient) SystemBinCount(payloads []string) ([]PayloadSystemCount, bool) {
	if c.baseURL == "" || len(payloads) == 0 {
		return nil, false
	}
	reqBody := struct {
		Payloads []string `json:"payloads"`
	}{Payloads: payloads}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, false
	}
	resp, err := c.http.Post(c.baseURL+"/api/inventory/system-count", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, false
	}
	var wire struct {
		Counts []struct {
			PayloadCode string `json:"payload_code"`
			BinCount    int    `json:"bin_count"`
		} `json:"counts"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, false
	}
	out := make([]PayloadSystemCount, len(wire.Counts))
	for i, c := range wire.Counts {
		out[i] = PayloadSystemCount{PayloadCode: c.PayloadCode, BinCount: c.BinCount}
	}
	return out, true
}

// PayloadSystemCount is the Edge-side mirror of Core's
// PayloadSystemCount — total bins of one payload in the kanban loop.
type PayloadSystemCount struct {
	PayloadCode string
	BinCount    int
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
	Status     string `json:"status"`
	Detail     string `json:"detail,omitempty"`
	BinID      int64  `json:"bin_id,omitempty"`
	BinLabel   string `json:"bin_label,omitempty"`
	DeltaEpoch int64  `json:"delta_epoch,omitempty"`
}

// BinCountResponse is Core's reply to a count declared from the line.
type BinCountResponse struct {
	Status       string `json:"status"`
	Detail       string `json:"detail,omitempty"`
	BinID        int64  `json:"bin_id,omitempty"`
	BinLabel     string `json:"bin_label,omitempty"`
	Expected     int    `json:"expected"`
	UOPRemaining int    `json:"uop_remaining"`
	Discrepancy  bool   `json:"discrepancy"`
	Warning      string `json:"warning,omitempty"`
	DeltaEpoch   int64  `json:"delta_epoch,omitempty"`
}

// RecordBinCount declares a count an operator made at the line to Core.
//
// This is the direction that did not exist. Counts travel with declarations,
// and Core had two doors for declaring one downward while the Edge had none for
// declaring one upward except as a rider on an order release. An operator who
// noticed a carrier's count was wrong outside a release had nowhere to say so —
// so the number that is authoritative, the one standing next to the carrier,
// could not reach Core's ledger.
//
// Like the other writes here it returns a hard error rather than degrading:
// a declaration that silently did not arrive is worse than one that failed
// loudly, because the operator would believe they had corrected it.
func (c *CoreClient) RecordBinCount(nodeName string, actualUOP int, actor string) (*BinCountResponse, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	body, err := json.Marshal(map[string]any{
		"node_name":  nodeName,
		"actual_uop": actualUOP,
		"actor":      actor,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal bin-count request: %w", err)
	}
	resp, err := c.http.Post(c.baseURL+"/api/telemetry/bin-count", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bin-count request failed: %w", err)
	}
	defer resp.Body.Close()
	var result BinCountResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode bin-count response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Status == "error" {
		detail := result.Detail
		if detail == "" {
			detail = fmt.Sprintf("core returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", detail)
	}
	return &result, nil
}

// ClearBin clears the manifest on the bin at a node via Core's HTTP API.
// binTypeCode is optional: when non-empty Core re-stamps the carrier's
// bin_type_id atomically with the manifest clear (dunnage float).
func (c *CoreClient) ClearBin(nodeName, binTypeCode string) (*BinClearResponse, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	reqBody := map[string]string{"node_name": nodeName}
	if binTypeCode != "" {
		reqBody["bin_type_code"] = binTypeCode
	}
	body, _ := json.Marshal(reqBody)
	resp, err := c.http.Post(c.baseURL+"/api/telemetry/bin-clear", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("bin-clear request failed: %w", err)
	}
	defer resp.Body.Close()
	var result BinClearResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode bin-clear response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Status == "error" {
		detail := result.Detail
		if detail == "" {
			detail = fmt.Sprintf("core returned %d", resp.StatusCode)
		}
		return nil, fmt.Errorf("%s", detail)
	}
	return &result, nil
}
