package rds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL    string
	httpClient *http.Client
}

func NewClient(baseURL string, timeout time.Duration) *Client {
	return &Client{
		baseURL: baseURL,
		httpClient: &http.Client{
			Timeout: timeout,
		},
	}
}

func (c *Client) get(path string, result any) error {
	resp, err := c.httpClient.Get(c.baseURL + path)
	if err != nil {
		return fmt.Errorf("rds GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	return c.decode(resp, result)
}

func (c *Client) post(path string, body any, result any) error {
	var bodyReader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("rds marshal: %w", err)
		}
		bodyReader = bytes.NewReader(data)
	}
	resp, err := c.httpClient.Post(c.baseURL+path, "application/json", bodyReader)
	if err != nil {
		return fmt.Errorf("rds POST %s: %w", path, err)
	}
	defer resp.Body.Close()
	return c.decode(resp, result)
}

func (c *Client) decode(resp *http.Response, result any) error {
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("rds read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		return fmt.Errorf("rds HTTP %d: %s", resp.StatusCode, string(data))
	}
	if result != nil {
		if err := json.Unmarshal(data, result); err != nil {
			return fmt.Errorf("rds decode: %w", err)
		}
	}
	return nil
}

// Reconfigure updates the client's base URL and timeout for hot-reload.
func (c *Client) Reconfigure(baseURL string, timeout time.Duration) {
	c.baseURL = baseURL
	c.httpClient.Timeout = timeout
}

// checkResponse validates the RDS response envelope code.
func checkResponse(r *Response) error {
	if r.Code != 0 {
		return fmt.Errorf("rds error %d: %s", r.Code, r.Msg)
	}
	return nil
}
