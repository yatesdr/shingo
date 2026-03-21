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
	c.dbg("<- GET %s %d after %dms body=%s", path, resp.StatusCode, time.Since(start).Milliseconds(), truncate(data, 2048))

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
	c.dbg("<- POST %s %d after %dms body=%s", path, resp.StatusCode, time.Since(start).Milliseconds(), truncate(data, 2048))

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
	c.dbg("<- GET %s %d after %dms body=%s", path, resp.StatusCode, time.Since(start).Milliseconds(), truncate(data, 2048))

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
	c.dbg("<- POST %s %d after %dms body=%s", path, resp.StatusCode, time.Since(start).Milliseconds(), truncate(data, 2048))

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
