package rds

import (
	"fmt"
	"strings"
)

// GetBinDetails retrieves bin fill status, optionally filtered by group names.
func (c *Client) GetBinDetails(groups ...string) ([]BinDetail, error) {
	path := "/binDetails"
	if len(groups) > 0 {
		path = fmt.Sprintf("/binDetails?binGroups=%s", strings.Join(groups, ","))
	}
	var resp BinDetailsResponse
	if err := c.get(path, &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// CheckBins validates that bin locations exist and are valid.
func (c *Client) CheckBins(bins []string) ([]BinCheckResult, error) {
	var resp BinCheckResponse
	if err := c.post("/binCheck", &BinCheckRequest{Bins: bins}, &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Bins, nil
}

// GetScene retrieves the full RDS scene configuration.
func (c *Client) GetScene() (*Scene, error) {
	var resp SceneResponse
	if err := c.get("/scene", &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// Ping checks RDS Core connectivity and returns product/version info.
func (c *Client) Ping() (*PingResponse, error) {
	var resp PingResponse
	if err := c.get("/ping", &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}
