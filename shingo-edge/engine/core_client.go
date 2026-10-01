package engine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"shingo/protocol"
	"shingoedge/service"
)

// NodeBinInfo and the other Edge↔Core HTTP contract types are defined in
// shingo/protocol (http_contract.go) and aliased here so existing call sites
// compile unchanged.
type NodeBinInfo = protocol.NodeBinInfo

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

// ManifestItem is the payload-template manifest line of the manifest endpoint.
type ManifestItem = protocol.ManifestItem

// BinLoadItem is one line of a bin-load request: part number + physical count.
type BinLoadItem = protocol.BinLoadItem

// PayloadManifestResponse is the full response from the manifest endpoint.
type PayloadManifestResponse = protocol.PayloadManifestResponse

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

// NodeChildInfo is one physical child of an NGRP in the children endpoint.
type NodeChildInfo = protocol.NodeChildInfo

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
type BinLoadRequest = protocol.BinLoadRequest

// coreErrorText picks the readable half of a failed Core reply.
//
// ── WHY THIS EXISTS, AND WHAT IT COST NOT TO ──────────────────────────────
//
// Core answers a failure in TWO shapes and these structs only knew one. The
// success-shaped handlers reply {"status":"error","detail":"..."} and the
// generic refusals go through www.jsonError, which writes {"error":"..."} —
// a field none of the three response types declared. So every jsonError
// refusal decoded into a zero struct, `detail` came out empty, and the caller
// logged the status code alone.
//
// MEASURED, sim 2026-08-30: the sim operator's auto-clear failed 115 times in
// one run, eight retries each, and every line said `clear bin: core returned
// 400`. The actual sentence Core sent — "no bin at node FGN_001" — was decoded
// into a field that did not exist and thrown away. A carrier that is never
// cleared is a carrier that never rejoins the empty pool, so this is the
// diagnostic half of the empties famine, and it was invisible for as long as
// the reason was.
//
// Order matters: detail first (the richer, deliberate field), then error, then
// the bare status code so a caller always gets SOMETHING.
func coreErrorText(detail, errText string, statusCode int) string {
	if detail != "" {
		return detail
	}
	if errText != "" {
		return errText
	}
	return fmt.Sprintf("core returned %d", statusCode)
}

// BinLoadResponse is the response after loading a bin via HTTP.
type BinLoadResponse = protocol.BinLoadResponse

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
		return nil, fmt.Errorf("%s", coreErrorText(result.Detail, result.Error, resp.StatusCode))
	}
	return &result, nil
}

// ReleasePoints asks Core what releasing each order would let its robot do
// next: the nodes its pending segment enters, the lifts it waits on, and the
// lineside bin it departs with (protocol.ReleasePoint). One round trip per act.
// A failure is an error: a release decided on facts Core did not give is the
// guess this read exists to replace.
func (c *CoreClient) ReleasePoints(station string, orderUUIDs []string) ([]protocol.ReleasePoint, error) {
	if c.baseURL == "" {
		return nil, ErrCoreNotConfigured
	}
	body, err := json.Marshal(protocol.ReleasePointsRequest{StationID: station, OrderUUIDs: orderUUIDs})
	if err != nil {
		return nil, fmt.Errorf("marshal release points request: %w", err)
	}
	resp, err := c.http.Post(c.baseURL+"/api/release/points", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("release points request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("release points: core returned %d", resp.StatusCode)
	}
	var out protocol.ReleasePointsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode release points: %w", err)
	}
	return out.Points, nil
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
	reqBody := protocol.PreflightRequest{Station: station, Payloads: payloads}
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
		Missing []string `json:"missing"`
		// A pointer so an absent key (a Core too old to answer) is told apart
		// from an empty list (a Core that has every payload somewhere).
		Absent    *[]string `json:"absent"`
		Available []struct {
			PayloadCode string `json:"payload_code"`
			BinCount    int    `json:"bin_count"`
		} `json:"available"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, fmt.Errorf("decode preflight response: %w", err)
	}
	out := &service.PreflightCoreResult{Missing: wire.Missing}
	if wire.Absent != nil {
		out.Absent, out.AbsentKnown = *wire.Absent, true
	}
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
	reqBody := protocol.SystemCountRequest{Payloads: payloads}
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
	var wire protocol.SystemBinCountResult
	if err := json.NewDecoder(resp.Body).Decode(&wire); err != nil {
		return nil, false
	}
	return wire.Counts, true
}

// PayloadSystemCount is the per-payload count of the system-count endpoint.
type PayloadSystemCount = protocol.PayloadSystemCount

// BinClearResponse is the response after clearing a bin via HTTP.
type BinClearResponse = protocol.BinClearResponse

// BinCountResponse is the response to a count declared from the line.
type BinCountResponse = protocol.BinCountResponse

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
	body, err := json.Marshal(protocol.BinCountRequest{
		NodeName:  nodeName,
		ActualUOP: actualUOP,
		Actor:     actor,
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
		return nil, fmt.Errorf("%s", coreErrorText(result.Detail, result.Error, resp.StatusCode))
	}
	return &result, nil
}

// SetBinQualityHold sets or clears a bin's quality-hold marker on Core
// (telemetry machine path, same group as bin-load/bin-clear). Best-effort at
// the call site: the containment move order is the containment; the marker is
// the audit and the re-release protection, and a failed marker write logs
// rather than blocks.
func (c *CoreClient) SetBinQualityHold(binID int64, hold bool, by string) error {
	if c.baseURL == "" {
		return fmt.Errorf("core API not configured")
	}
	body, _ := json.Marshal(map[string]any{"bin_id": binID, "hold": hold, "by": by})
	resp, err := c.http.Post(c.baseURL+"/api/telemetry/bin-quality-hold", "application/json", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("bin-quality-hold request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("bin-quality-hold returned %d: %s", resp.StatusCode, coreErrorText(string(raw), "", resp.StatusCode))
	}
	return nil
}

// GetContainment reads Core's quality-containment state (public read — the
// containment screens render state; they hold no Core credentials).
func (c *CoreClient) GetContainment() (*ContainmentState, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	resp, err := c.http.Get(c.baseURL + "/api/containment")
	if err != nil {
		return nil, fmt.Errorf("containment read failed: %w", err)
	}
	defer resp.Body.Close()
	var state ContainmentState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		return nil, fmt.Errorf("decode containment: %w", err)
	}
	return &state, nil
}

// ContainmentState mirrors Core's /api/containment body.
type ContainmentState struct {
	Containment []ContainmentRow `json:"containment"`
	HeldBins    []HeldBinRow     `json:"held_bins"`
}

// ContainmentRow is one payload's containment flag state.
type ContainmentRow struct {
	PayloadCode   string `json:"payload_code"`
	Active        bool   `json:"active"`
	Reason        string `json:"reason"`
	ActivatedBy   string `json:"activated_by"`
	ActivatedAt   string `json:"activated_at"`
	DeactivatedBy string `json:"deactivated_by"`
	DeactivatedAt string `json:"deactivated_at"`
}

// HeldBinRow is one bin carrying the hold marker.
type HeldBinRow struct {
	BinID       int64  `json:"bin_id"`
	Label       string `json:"label"`
	PayloadCode string `json:"payload_code"`
	NodeName    string `json:"node_name"`
	HoldBy      string `json:"hold_by"`
	HoldAt      string `json:"hold_at"`
}

// ClearBin clears the manifest on the bin at a node via Core's HTTP API.
// binTypeCode is optional: when non-empty Core re-stamps the carrier's
// bin_type_id atomically with the manifest clear (dunnage float).
func (c *CoreClient) ClearBin(nodeName, binTypeCode string) (*BinClearResponse, error) {
	if c.baseURL == "" {
		return nil, fmt.Errorf("core API not configured")
	}
	reqBody := protocol.BinClearRequest{NodeName: nodeName, BinTypeCode: binTypeCode}
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
		return nil, fmt.Errorf("%s", coreErrorText(result.Detail, result.Error, resp.StatusCode))
	}
	return &result, nil
}
