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
	if resp.Scene == nil {
		c.dbg("!! /scene returned code=0 but scene=null")
		return nil, fmt.Errorf("scene: empty response data")
	}
	return resp.Scene, nil
}
