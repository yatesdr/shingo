package rds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

type Client struct {
	mu         sync.RWMutex
	baseURL    string
	httpClient *http.Client
	DebugLog   func(string, ...any)

	// countGroupSeen is the last logged outcome per count group, so the
	// 500ms occupancy poll logs on transition instead of on tick. See
	// logCountGroupChange.
	cgMu           sync.Mutex
	countGroupSeen map[string]string
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) dbg(format string, args ...any) {
	if fn := c.DebugLog; fn != nil {
		fn(format, args...)
	}
}

// logCountGroupChange emits one debug line only when a group's occupancy
// outcome differs from the last one logged for that group.
//
// The count-group poll runs at 500ms and its two per-tick trace lines were
// 334,361 lines/day at Springfield — 53% of the whole journal — for a value
// that changes a few thousand times a day. Edge-triggering it is modelled on
// wireChanged in engine/sourceability_monitor.go: log the operator-visible
// verdict, not the sample.
//
// This is a logging change only. The interlock still polls at 500ms and the
// debouncer still sees every sample — deduping upstream of the debouncer
// would starve its off-threshold counter (see the note in countgroup/loop.go
// runTick). countgroup at Springfield returns 1-2 robots 6,265 times a day;
// it is working and stays enabled.
func (c *Client) logCountGroupChange(group, outcome string) {
	if c.DebugLog == nil {
		return
	}
	c.cgMu.Lock()
	prev, seen := c.countGroupSeen[group]
	if seen && prev == outcome {
		c.cgMu.Unlock()
		return
	}
	if c.countGroupSeen == nil {
		c.countGroupSeen = make(map[string]string, 4)
	}
	c.countGroupSeen[group] = outcome
	c.cgMu.Unlock()

	if !seen {
		c.dbg("/robotsInCountGroup group=%s %s (first poll)", group, outcome)
		return
	}
	c.dbg("/robotsInCountGroup group=%s %s (was %s)", group, outcome, prev)
}

// slowResponseThreshold is the latency cutoff above which the response
// body is included in success-path debug logs. Hot endpoints like
// /robotsStatus return 200 in ~2ms with a ~4KB body that's the same
// shape every poll — dumping it on every tick buries every other event
// in the diagnostics log. Bodies are still logged unconditionally on
// non-200 responses (where the body is the error envelope) and on slow
// successes (where the body might explain the slowdown).
const slowResponseThreshold = 1 * time.Second

// logResponse emits the success/slow trace line: URL + status + timing
// always; body only when the status is non-200 or the elapsed time
// exceeds slowResponseThreshold. Centralises the body-or-not gate so
// all four request paths (get, post, getRaw, postRaw) stay in lockstep.
func (c *Client) logResponse(method, path string, statusCode int, elapsed time.Duration, data []byte) {
	if statusCode != 200 || elapsed > slowResponseThreshold {
		c.dbg("<- %s %s %d after %dms body=%s",
			method, path, statusCode, elapsed.Milliseconds(), truncate(data, 2048))
		return
	}
	c.dbg("<- %s %s %d after %dms", method, path, statusCode, elapsed.Milliseconds())
}

func (c *Client) url(path string) string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL + path
}

func (c *Client) get(path string, result any) error {
	fullURL := c.url(path)
	c.dbg("-> GET %s", fullURL)
	start := time.Now()

	resp, err := c.httpClient.Get(fullURL)
	if err != nil {
		c.dbg("<- GET %s error after %dms: %v", path, time.Since(start).Milliseconds(), err)
		return fmt.Errorf("rds GET %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("rds read body: %w", err)
	}
	c.logResponse("GET", path, resp.StatusCode, time.Since(start), data)

	return c.decodeBytes(data, resp.StatusCode, result)
}

func (c *Client) post(path string, body any, result any) error {
	var bodyReader io.Reader
	var bodyData []byte
	if body != nil {
		var err error
		bodyData, err = json.Marshal(body)
		if err != nil {
			return fmt.Errorf("rds marshal: %w", err)
		}
		bodyReader = bytes.NewReader(bodyData)
	}

	fullURL := c.url(path)
	c.dbg("-> POST %s body=%s", fullURL, truncate(bodyData, 2048))
	start := time.Now()

	resp, err := c.httpClient.Post(fullURL, "application/json", bodyReader)
	if err != nil {
		c.dbg("<- POST %s error after %dms: %v", path, time.Since(start).Milliseconds(), err)
		return fmt.Errorf("rds POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("rds read body: %w", err)
	}
	c.logResponse("POST", path, resp.StatusCode, time.Since(start), data)

	return c.decodeBytes(data, resp.StatusCode, result)
}

func (c *Client) decodeBytes(data []byte, statusCode int, result any) error {
	if statusCode >= 400 {
		return fmt.Errorf("rds HTTP %d: %s", statusCode, string(data))
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("rds decode: %w", err)
		}
	}
	return nil
}

func (c *Client) getRaw(path string) ([]byte, error) {
	fullURL := c.url(path)
	c.dbg("-> GET %s", fullURL)
	start := time.Now()

	resp, err := c.httpClient.Get(fullURL)
	if err != nil {
		c.dbg("<- GET %s error after %dms: %v", path, time.Since(start).Milliseconds(), err)
		return nil, fmt.Errorf("rds GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rds read body: %w", err)
	}
	c.logResponse("GET", path, resp.StatusCode, time.Since(start), data)

	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("rds HTTP %d: %s", resp.StatusCode, string(data))
	}
	return data, nil
}

func (c *Client) postRaw(path string, contentType string, body io.Reader, result any) error {
	fullURL := c.url(path)
	c.dbg("-> POST %s (raw)", fullURL)
	start := time.Now()

	resp, err := c.httpClient.Post(fullURL, contentType, body)
	if err != nil {
		c.dbg("<- POST %s error after %dms: %v", path, time.Since(start).Milliseconds(), err)
		return fmt.Errorf("rds POST %s: %w", path, err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("rds read body: %w", err)
	}
	c.logResponse("POST", path, resp.StatusCode, time.Since(start), data)

	return c.decodeBytes(data, resp.StatusCode, result)
}

// BaseURL returns the client's base URL.
func (c *Client) BaseURL() string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.baseURL
}

// Reconfigure updates the client's base URL and timeout for hot-reload.
func (c *Client) Reconfigure(baseURL string, timeout time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.baseURL = baseURL
	c.httpClient = &http.Client{Timeout: timeout}
}

// checkResponse validates the RDS response envelope code.
func checkResponse(r *Response) error {
	if r.Code != 0 {
		return fmt.Errorf("rds error %d: %s", r.Code, r.Msg)
	}
	return nil
}

func truncate(data []byte, maxLen int) string {
	if len(data) == 0 {
		return "<empty>"
	}
	if len(data) <= maxLen {
		return string(data)
	}
	return string(data[:maxLen]) + "...(truncated)"
}
