package rds

import "fmt"

// GetRobotsStatus retrieves status for all robots.
func (c *Client) GetRobotsStatus() ([]RobotStatus, error) {
	resp, err := c.GetRobotsStatusFull()
	if err != nil {
		return nil, err
	}
	return resp.Report, nil
}

// GetRobotsStatusFull retrieves the whole /robotsStatus envelope.
//
// Callers that only want robots should use GetRobotsStatus. This exists
// because the envelope also carries scene_md5 and the disabled-lane list,
// which belong to no robot and are therefore invisible to anything that
// reaches straight for .Report — which was everything, for as long as this
// endpoint has been polled.
func (c *Client) GetRobotsStatusFull() (*RobotsStatusResponse, error) {
	var resp RobotsStatusResponse
	if err := c.get("/robotsStatus", &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return &resp, nil
}

// SetDispatchable sets dispatchability for robots.
func (c *Client) SetDispatchable(req *DispatchableRequest) error {
	var resp Response
	if err := c.post("/dispatchable", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// RedoFailed retries the current failed block for given robots.
func (c *Client) RedoFailed(req *RedoFailedRequest) error {
	var resp Response
	if err := c.post("/redoFailedOrder", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// ManualFinish marks the current block as manually finished for given robots.
func (c *Client) ManualFinish(req *ManualFinishRequest) error {
	var resp Response
	if err := c.post("/manualFinished", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// GetRobotMap downloads the map file from a specific robot.
func (c *Client) GetRobotMap(vehicle, mapName string) ([]byte, error) {
	path := fmt.Sprintf("/robotSmap?vehicle=%s&map=%s", vehicle, mapName)
	return c.getRaw(path)
}

// PreemptControl takes exclusive manual control of one or more robots.
func (c *Client) PreemptControl(vehicles []string) error {
	var resp Response
	if err := c.post("/lock", &VehiclesRequest{Vehicles: vehicles}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// ReleaseControl releases manual control of one or more robots.
func (c *Client) ReleaseControl(vehicles []string) error {
	var resp Response
	if err := c.post("/unlock", &VehiclesRequest{Vehicles: vehicles}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// SetParamsTemp temporarily modifies robot parameters (lost on restart).
func (c *Client) SetParamsTemp(vehicle string, body map[string]map[string]any) error {
	var resp Response
	if err := c.post("/setParams", &ModifyParamsRequest{Vehicle: vehicle, Body: body}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// SetParamsPerm permanently modifies robot parameters (survives restart).
func (c *Client) SetParamsPerm(vehicle string, body map[string]map[string]any) error {
	var resp Response
	if err := c.post("/saveParams", &ModifyParamsRequest{Vehicle: vehicle, Body: body}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// RestoreParamDefaults resets specific plugin parameters to factory defaults.
func (c *Client) RestoreParamDefaults(req *RestoreParamsRequest) error {
	var resp Response
	if err := c.post("/reloadParams", req, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// SwitchMap switches a robot to a different map.
func (c *Client) SwitchMap(vehicle, mapName string) error {
	var resp Response
	if err := c.post("/switchMap", &SwitchMapRequest{Vehicle: vehicle, Map: mapName}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// ConfirmRelocalization confirms a robot's position after manual repositioning.
func (c *Client) ConfirmRelocalization(vehicles []string) error {
	var resp Response
	if err := c.post("/reLocConfirm", &VehiclesRequest{Vehicles: vehicles}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// PauseNavigation pauses one or more robots in place.
func (c *Client) PauseNavigation(vehicles []string) error {
	var resp Response
	if err := c.post("/gotoSitePause", &VehiclesRequest{Vehicles: vehicles}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}

// ResumeNavigation resumes navigation for one or more paused robots.
func (c *Client) ResumeNavigation(vehicles []string) error {
	var resp Response
	if err := c.post("/gotoSiteResume", &VehiclesRequest{Vehicles: vehicles}, &resp); err != nil {
		return err
	}
	return checkResponse(&resp)
}
