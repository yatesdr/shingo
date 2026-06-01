package engine

import (
	"bytes"
	"encoding/json"
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

// FetchNodeChildren returns the direct physical children of an NGRP node.
// Returns nil if Core is unavailable or the node has no physical children.
func (c *CoreClient) FetchNodeChildren(nodeName string) ([]NodeChildInfo, error) {
	if c.baseURL == "" || nodeName == "" {
		return nil, nil
	}
	resp, err := c.http.Get(c.baseURL + "/api/telemetry/node/" + url.PathEscape(nodeName) + "/children")
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

// BinUOPRow mirrors service.BinUOPRow on Core. The reconciler's
// self-heal path reads it to align local runtime cache with Core's
// authoritative bin count.
type BinUOPRow struct {
	BinID        int64  `json:"bin_id"`
	NodeName     string `json:"node_name"`
	PayloadCode  string `json:"payload_code"`
	UOPRemaining int    `json:"uop_remaining"`
	// DeltaEpoch mirrors Core's bins.delta_epoch — populated on
	// startup-time reconciliation so Edge can repair a lost bin-state
	// cache against the current load's epoch instead of starting at 0.
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

// FetchUOPState returns the authoritative bin + bucket snapshot from
// Core. Returns nil (no error) when Core is unavailable, matching
// FetchNodeBins's graceful-degradation contract — a missed
// reconciliation pass is not worth surfacing.
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
// Replaces the unsafe (nil, nil) collapse from FetchNodeBins for
// the reconciler self-heal path. FetchNodeBins keeps its existing
// graceful-degradation contract for non-self-heal callers (HMI
// telemetry where a temporary nil-vs-occupied flicker is acceptable).
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

// FetchNodeBins returns bin state for the given core node names.
// Returns nil (no error) if Core is unavailable or unreachable.
func (c *CoreClient) FetchNodeBins(nodeNames []string) ([]NodeBinInfo, error) {
	if c.baseURL == "" || len(nodeNames) == 0 {
		return nil, nil
	}
	params := url.Values{}
	params.Set("nodes", strings.Join(nodeNames, ","))
	reqURL := c.baseURL + "/api/telemetry/node-bins?" + params.Encode()
	resp, err := c.http.Get(reqURL)
	if err != nil {
		return nil, nil // Core unreachable — graceful degradation
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, nil
	}
	var result []NodeBinInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, nil
	}
	return result, nil
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
// not just bins-available-to-source-right-now. Used by kanban demand math
// (refillLoaderForPayload) where a bin staged at the consumer line still
// counts as inventory.
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

// ClearBin clears the manifest on the bin at a node via Core's HTTP API.
func (c *CoreClient) ClearBin(nodeName string) error {
	if c.baseURL == "" {
		return fmt.Errorf("core API not configured")
	}
	body, _ := json.Marshal(map[string]string{"node_name": nodeName})
	resp, err := c.http.Post(c.baseURL+"/api/telemetry/bin-clear", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bin-clear request failed: %w", err)
	}
	defer resp.Body.Close()
	var result struct {
		Status string `json:"status"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return fmt.Errorf("decode bin-clear response: %w", err)
	}
	if resp.StatusCode != http.StatusOK || result.Status == "error" {
		detail := result.Detail
		if detail == "" {
			detail = fmt.Sprintf("core returned %d", resp.StatusCode)
		}
		return fmt.Errorf("%s", detail)
	}
	return nil
}
