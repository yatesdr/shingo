package rds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// GetRobotsInCountGroup returns the robot IDs currently in the named advanced zone.
//
// CONTRACT (empirical — endpoint not in AIVISION HTTP API PDF; verified via
// Postman against live RDS 0.2.0 on 2026-04-15):
//   - Method: POST. GET returns 404.
//   - Body: {"group":"<name>"} — lowercase, case-sensitive.
//   - Success: HTTP 200 + bare JSON array (["AMR-01"] or []).
//   - Error:   HTTP 4xx + standard wrapped envelope {"code":..., "msg":..., "create_on":...}.
//   - Unknown group: HTTP 200 + bare [], indistinguishable from real-but-empty zone.
//
// Success and error use DIFFERENT response shapes — this method does its own
// defensive decode rather than going through c.post()/checkResponse().
//
// Compression note: this RDS server falls back to identity for Go clients
// (Go's stdlib advertises gzip only). No compression handling required here.
// Postman shows br because Postman advertises br by default.
func (c *Client) GetRobotsInCountGroup(group string) ([]string, error) {
	req := struct {
		Group string `json:"group"`
	}{Group: group}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rds marshal: %w", err)
	}

	fullURL := c.url("/robotsInCountGroup")
	c.dbg("-> POST %s body=%s", fullURL, string(body))

	resp, err := c.httpClient.Post(fullURL, "application/json", bytes.NewReader(body))
	if err != nil {
		c.dbg("<- POST /robotsInCountGroup error: %v", err)
		return nil, fmt.Errorf("rds POST /robotsInCountGroup: %w", err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rds read body: %w", err)
	}
	c.dbg("<- POST /robotsInCountGroup %d body=%s", resp.StatusCode, truncate(data, 2048))

	if resp.StatusCode == 200 {
		// Decode A: bare array — the documented empirical contract today.
		var robots []string
		if err := json.Unmarshal(data, &robots); err == nil {
			return robots, nil
		}
		// Decode B: wrapped success envelope with array under .report.
		// Future-proofs against AIVISION normalising this endpoint to match
		// the rest of the RDS API. Without this fallback, a firmware update
		// that switches to the standard envelope would feed every poll into
		// the fail-safe timer and force every plant light ON until patched.
		//
		// The `Report != nil` guard is essential: an arbitrary JSON object
		// like {"unexpected":"shape"} decodes cleanly into this struct with
		// Code=0 (zero value) and Report=nil. Without the nil check we'd
		// silently return "no robots" for unrecognised bodies, defeating
		// the point of the fail-safe path.
		var wrapped struct {
			Response
			Report []string `json:"report"`
		}
		if err := json.Unmarshal(data, &wrapped); err == nil && wrapped.Code == 0 && wrapped.Report != nil {
			return wrapped.Report, nil
		}
		// Neither shape decoded. Surface as a failure (advances fail-safe timer)
		// rather than silently returning [] and misleading downstream debounce.
		return nil, fmt.Errorf("rds /robotsInCountGroup: cannot decode 200 body: %s", truncate(data, 256))
	}

	// Non-200: always wrapped envelope.
	var env Response
	_ = json.Unmarshal(data, &env)
	return nil, fmt.Errorf("rds /robotsInCountGroup HTTP %d code=%d: %s",
		resp.StatusCode, env.Code, env.Msg)
}

// GetRobotsStatus retrieves status for all robots.
func (c *Client) GetRobotsStatus() ([]RobotStatus, error) {
	var resp RobotsStatusResponse
	if err := c.get("/robotsStatus", &resp); err != nil {
		return nil, err
	}
	if err := checkResponse(&resp.Response); err != nil {
		return nil, err
	}
	return resp.Report, nil
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
