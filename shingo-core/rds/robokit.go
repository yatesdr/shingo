package rds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The Robokit proxy — how Core reaches a robot's own map.
//
// RDS's own HTTP surface has no area or reflector endpoint at all: its /scene
// returns advancedAreaList: [] and there is not one hit for "reflector" across
// the whole spec. The areas and the reflector positions exist only inside each
// robot's onboard .smap, on Robokit ports that the plant network blocks from
// the Core box.
//
// /generalRobokitAPI is the way through. It relays an arbitrary Robokit call
// to a named robot on a named port using RDS's OWN connection to that robot,
// which reaches the ports Core cannot. Verified read-only at Springfield
// 2026-08-06: #4011 on port 19207 returned the whole 7.3 MB map.
//
// THIS IS NOT A SECOND POLL AND MUST NOT BECOME ONE. It is an on-change fetch,
// gated by a hash that arrives on the /robotsStatus poll Core already makes.
// The original design deliberately refused a second connection to every robot
// because RDS already aggregates; this keeps that property by talking to RDS.

// Robokit port numbers. Only the status port is reachable directly from the
// Core box; the rest are why the proxy exists.
const (
	RobokitPortStatus  = 19204
	RobokitPortControl = 19205
	RobokitPortNav     = 19206
	RobokitPortConfig  = 19207
)

// Robokit API codes used here.
const (
	RobokitCodeMapList     = 1300 // robot_status_runningstatus_map_req
	RobokitCodeDownloadMap = 4011 // robot_config_downloadmap
)

// GeneralRobokitRequest is the proxy envelope.
type GeneralRobokitRequest struct {
	Vehicle string `json:"vehicle"`
	Port    int    `json:"port"`
	Code    int    `json:"code"`
	Cmd     any    `json:"cmd,omitempty"`
}

// GeneralRobokitAPI relays a Robokit call through RDS and returns the raw
// response body.
//
// RAW BYTES, NOT A DECODED STRUCT, and deliberately. The map response is 7.3 MB
// of JSON whose useful content is a few hundred kilobytes; the caller parses
// the handful of lists it wants and archives the rest compressed without ever
// materialising the whole thing as Go values. It also means the archived bytes
// are the bytes the robot sent — which is what makes a content hash mean
// anything, and what a re-marshalled canonical form would quietly destroy as
// key order drifted.
func (c *Client) GeneralRobokitAPI(req GeneralRobokitRequest, timeout time.Duration) ([]byte, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("rds robokit marshal: %w", err)
	}
	fullURL := c.url("/generalRobokitAPI")
	c.dbg("-> POST %s vehicle=%s port=%d code=%d", fullURL, req.Vehicle, req.Port, req.Code)
	start := time.Now()

	// Its own client, because the map download is orders of magnitude slower
	// than any other call this package makes and the shared timeout is tuned
	// for a 2-second poll. Borrowing that would turn a working fetch into a
	// timeout that looks like a robot fault.
	hc := &http.Client{Timeout: timeout}
	resp, err := hc.Post(fullURL, "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("rds robokit POST: %w", err)
	}
	defer resp.Body.Close()

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("rds robokit read: %w", err)
	}
	c.dbg("<- POST /generalRobokitAPI %d %d bytes in %dms",
		resp.StatusCode, len(data), time.Since(start).Milliseconds())
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("rds robokit: HTTP %d: %s", resp.StatusCode, truncate(data, 256))
	}
	// THE PROXY RETURNS 200 ON A FAILED RELAY, AND ITS ERROR FIELD IS `code`.
	//
	// An unreachable robot, a bad map name or a refused port all come back as
	// a normal HTTP 200 carrying a non-zero status in the BODY. A caller that
	// trusted the status line would archive an error body as a map version and
	// report a plant that lost every reflector overnight.
	//
	// THIS GUARD USED TO READ `ret_code` AND THEREFORE NEVER FIRED. Measured
	// against Springfield RDS 2026-08-07, a deliberately malformed request
	// returns:
	//
	//	{"code":50001,"msg":"generalRobokitAPI error: cmd is not json object"}
	//
	// `code`, not `ret_code` -- so the probe found nil every time and every
	// failed relay was returned to the caller as success. Nothing downstream
	// caught it either: ApplyMapSnapshot happens to refuse the body because it
	// does not parse as a map, which is a property of THIS error body rather
	// than of the design. An error that parsed as a valid-but-empty map would
	// have been accepted, and every area and reflector would have versioned to
	// "gone" in one sync.
	//
	// So this is not a hardening pass. It is the only actual guard on the path,
	// and both spellings are checked because the vendor has now used one of
	// them and documented the other.
	var probe struct {
		Code    *int   `json:"code"`
		RetCode *int   `json:"ret_code"`
		Msg     string `json:"msg"`
		ErrMsg  string `json:"err_msg"`
	}
	if err := json.Unmarshal(data, &probe); err == nil {
		status, field := (*int)(nil), ""
		if probe.RetCode != nil && *probe.RetCode != 0 {
			status, field = probe.RetCode, "ret_code"
		} else if probe.Code != nil && *probe.Code != 0 {
			status, field = probe.Code, "code"
		}
		if status != nil {
			msg := probe.ErrMsg
			if msg == "" {
				msg = probe.Msg
			}
			return nil, fmt.Errorf("rds robokit: vehicle %s port %d code %d returned %s %d: %s",
				req.Vehicle, req.Port, req.Code, field, *status, msg)
		}
	}
	// A RELAY THAT SUCCEEDS AND RETURNS NOTHING IS ALSO A FAILURE.
	//
	// Measured at Springfield 2026-08-07: every valid call -- #1000, #1300,
	// #4011 -- returns the same 56-byte {"code":0,"msg":"ok"} after a uniform
	// ~3.07 s. RDS accepts the request, does not reach the robot, and reports
	// success. A status field cannot catch that; only the size can.
	//
	// The threshold is deliberately crude. Every Robokit response this package
	// asks for carries a payload orders of magnitude larger than an envelope --
	// a map list, or a 7.3 MB map -- so "smaller than a plausible envelope plus
	// a little" separates "relayed nothing" from "relayed something" without
	// needing to know the shape of each response.
	if len(data) < minRelayedBody {
		return nil, fmt.Errorf(
			"rds robokit: vehicle %s port %d code %d relayed no payload (%d bytes, status ok) -- "+
				"RDS accepted the call and did not reach the robot; the proxy is not relaying",
			req.Vehicle, req.Port, req.Code, len(data))
	}
	return data, nil
}

// minRelayedBody is the smallest body that could contain a relayed payload
// rather than a bare status envelope. RDS's own OK envelope is 56 bytes.
const minRelayedBody = 200

// MapListResponse is #1300: which map a robot is running and its hash.
type MapListResponse struct {
	CurrentMap    string `json:"current_map"`
	CurrentMapMD5 string `json:"current_map_md5"`
	MapFilesInfo  []struct {
		Name string `json:"name"`
		Size int64  `json:"size"`
	} `json:"map_files_info"`
}

// GetRobotMapList asks one robot which map it is running.
//
// Reachable on the STATUS port, which is open from the Core box directly — so
// this is the cheap half of the gate, usable even if the proxy is unavailable.
// The expensive half (#4011, the whole map) only runs when the hash it returns
// has moved.
func (c *Client) GetRobotMapList(vehicle string, timeout time.Duration) (*MapListResponse, error) {
	data, err := c.GeneralRobokitAPI(GeneralRobokitRequest{
		Vehicle: vehicle, Port: RobokitPortStatus, Code: RobokitCodeMapList,
	}, timeout)
	if err != nil {
		return nil, err
	}
	var out MapListResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("rds robokit map list: %w", err)
	}
	return &out, nil
}

// DownloadMap fetches a robot's .smap verbatim.
//
// #4011 on the CONFIG port, which the plant network blocks from the Core box —
// hence the proxy. Read-only: this downloads a map, it never uploads or
// switches one.
func (c *Client) DownloadMap(vehicle, mapName string, timeout time.Duration) ([]byte, error) {
	return c.GeneralRobokitAPI(GeneralRobokitRequest{
		Vehicle: vehicle,
		Port:    RobokitPortConfig,
		Code:    RobokitCodeDownloadMap,
		Cmd:     map[string]string{"map_name": mapName},
	}, timeout)
}
