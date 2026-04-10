package plc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// WarlinkClient abstracts the WarLink HTTP API so the Manager can be tested
// without a real WarLink instance.
type WarlinkClient interface {
	// ListPLCs returns all PLCs known to WarLink.
	ListPLCs(ctx context.Context) ([]WarlinkPLC, error)

	// ListTags returns all published tag values for a connected PLC.
	ListTags(ctx context.Context, plcName string) (map[string]WarlinkTag, error)

	// ListAllTags returns all tags (published and unpublished) for a PLC.
	ListAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error)

	// SetTagPublishing enables or disables REST publishing for a tag.
	SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error

	// OpenEventStream opens a long-lived SSE connection for real-time updates.
	// The caller is responsible for closing the returned ReadCloser.
	OpenEventStream(ctx context.Context) (io.ReadCloser, error)
}

// WarlinkPLC is the PLC info returned by the WarLink API.
type WarlinkPLC struct {
	Name        string `json:"name"`
	Address     string `json:"address"`
	Slot        int    `json:"slot"`
	Status      string `json:"status"`
	ProductName string `json:"product_name"`
	Error       string `json:"error"`
}

// WarlinkTag is a single tag value returned by the WarLink tags endpoint.
type WarlinkTag struct {
	PLC   string      `json:"plc"`
	Name  string      `json:"name"`
	Type  string      `json:"type"`
	Value interface{} `json:"value"`
	Error string      `json:"error"`
}

// WarlinkTagInfo describes a tag from the WarLink all-tags endpoint,
// including tags that are not yet enabled for REST publishing.
type WarlinkTagInfo struct {
	Name       string      `json:"name"`
	Type       string      `json:"type"`
	Configured bool        `json:"configured"`
	Enabled    bool        `json:"enabled"`
	Writable   bool        `json:"writable,omitempty"`
	NoREST     bool        `json:"no_rest,omitempty"`
	Value      interface{} `json:"value,omitempty"`
}

// httpWarlinkClient is the production implementation that talks to WarLink over HTTP.
type httpWarlinkClient struct {
	baseURL   string
	client    http.Client
	sseClient http.Client // no timeout for long-lived SSE connections
}

// NewWarlinkClient creates an HTTP-backed WarLink client for the given base URL.
func NewWarlinkClient(baseURL string) WarlinkClient {
	return &httpWarlinkClient{
		baseURL:   baseURL,
		client:    http.Client{Timeout: 10 * time.Second},
		sseClient: http.Client{Timeout: 0},
	}
}

func (c *httpWarlinkClient) ListPLCs(ctx context.Context) ([]WarlinkPLC, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WarLink returned %d", resp.StatusCode)
	}
	var plcs []WarlinkPLC
	if err := json.NewDecoder(resp.Body).Decode(&plcs); err != nil {
		return nil, fmt.Errorf("decode PLCs: %w", err)
	}
	return plcs, nil
}

func (c *httpWarlinkClient) ListTags(ctx context.Context, plcName string) (map[string]WarlinkTag, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/"+url.PathEscape(plcName)+"/tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WarLink tags %s returned %d", plcName, resp.StatusCode)
	}
	var tags map[string]WarlinkTag
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode tags %s: %w", plcName, err)
	}
	return tags, nil
}

func (c *httpWarlinkClient) ListAllTags(ctx context.Context, plcName string) ([]WarlinkTagInfo, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/"+url.PathEscape(plcName)+"/all-tags", nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("WarLink all-tags %s returned %d", plcName, resp.StatusCode)
	}
	var tags []WarlinkTagInfo
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return nil, fmt.Errorf("decode all-tags %s: %w", plcName, err)
	}
	return tags, nil
}

func (c *httpWarlinkClient) SetTagPublishing(ctx context.Context, plcName, tagName string, enabled bool) error {
	payload := map[string]interface{}{"enabled": enabled}
	if enabled {
		payload["no_rest"] = false
	}
	body, _ := json.Marshal(payload)

	req, err := http.NewRequestWithContext(ctx, "PATCH", c.baseURL+"/"+url.PathEscape(plcName)+"/tags/"+url.PathEscape(tagName), bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("WarLink PATCH %s/%s returned %d", plcName, tagName, resp.StatusCode)
	}
	return nil
}

func (c *httpWarlinkClient) OpenEventStream(ctx context.Context) (io.ReadCloser, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+"/events?types=value-change,status-change,health", nil)
	if err != nil {
		return nil, fmt.Errorf("SSE request: %w", err)
	}
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.sseClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("SSE connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("SSE status %d", resp.StatusCode)
	}
	return resp.Body, nil
}
