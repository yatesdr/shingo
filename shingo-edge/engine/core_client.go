package engine

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// NodeBinInfo describes the bin state at a single core node.
type NodeBinInfo struct {
	NodeName    string `json:"node_name"`
	BinLabel    string `json:"bin_label,omitempty"`
	PayloadCode string `json:"payload_code,omitempty"`
	UOPRemaining int   `json:"uop_remaining"`
	Occupied    bool   `json:"occupied"`
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
	url := c.baseURL + "/api/telemetry/payload/" + payloadCode + "/manifest"
	resp, err := c.http.Get(url)
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

// FetchNodeBins returns bin state for the given core node names.
// Returns nil (no error) if Core is unavailable or unreachable.
func (c *CoreClient) FetchNodeBins(nodeNames []string) ([]NodeBinInfo, error) {
	if c.baseURL == "" || len(nodeNames) == 0 {
		return nil, nil
	}
	url := c.baseURL + "/api/telemetry/node-bins?nodes=" + strings.Join(nodeNames, ",")
	resp, err := c.http.Get(url)
	if err != nil {
		return nil, nil // Core unreachable — graceful degradation
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("core returned %d", resp.StatusCode)
	}
	var result []NodeBinInfo
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode node bins: %w", err)
	}
	return result, nil
}
