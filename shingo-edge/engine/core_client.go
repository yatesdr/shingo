package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// NodeBinInfo describes the bin state at a single core node.
type NodeBinInfo struct {
	NodeName          string  `json:"node_name"`
	BinLabel          string  `json:"bin_label,omitempty"`
	BinTypeCode       string  `json:"bin_type_code,omitempty"`
	PayloadCode       string  `json:"payload_code,omitempty"`
	UOPRemaining      int     `json:"uop_remaining"`
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

// Available returns true if a Core API URL is configured.
func (c *CoreClient) Available() bool {
	return c.baseURL != ""
}

// ManifestItem describes a single line in a payload manifest template.
type ManifestItem struct {
	PartNumber  string `json:"part_number"`
	Quantity    int64  `json:"quantity"`
	Description string `json:"description"`
}

// PayloadManifestResponse is the full response from Core's manifest endpoint.
type PayloadManifestResponse struct {
	UOPCapacity int            `json:"uop_capacity"`
	Items       []ManifestItem `json:"items"`
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
