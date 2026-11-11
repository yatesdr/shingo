package rds

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// The Robokit proxy — how Core reaches a robot's own map, and once an hour a
// parked robot's laser frame (rds/lidar.go).
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
// THIS IS NOT A SECOND POLL AND MUST NOT BECOME ONE. The map is an on-change
// fetch, gated by a hash that arrives on the /robotsStatus poll Core already
// makes; the laser frame is one call per parked robot per hour, from its own
// loop (engine/robot_lidar.go), never the 2-second poll. The original design
// deliberately refused a second connection to every robot because RDS already
// aggregates; both keep that property by talking to RDS.

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
	// command marks a call that changes something on the robot and has no
	// payload (RobokitCommand): the empty-relay floor does not apply to it, and
	// the robot's ret_code is read instead. Not on the wire.
	command bool
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
	// #4011 -- returned the same 56-byte {"code":0,"msg":"ok"} after a uniform
	// ~3.07 s. RDS accepted the request, did not reach the robot, and reported
	// success. A status field cannot catch that; only the size can.
	//
	// That was a state of Springfield's RDS, not a property of the relay.
	// Re-measured 2026-10-15 at both plants (#1009, the laser frame): every
	// call relayed a full ~37 KB frame in 44-81 ms, equal to a direct call to
	// the robot within one beam (airknife-cleaning-2026-10-13/DATA-laser-frame-
	// gauge.md §10). What changed on Springfield's RDS in between was not
	// investigated, which is why the guard stays: if the empty envelope comes
	// back, this is where it is caught instead of being stored as "no beams".
	//
	// The threshold is deliberately crude, and it applies only to a call that
	// asks for a payload -- a map list, a 37 KB laser frame, or a 7.3 MB map --
	// so "smaller than a plausible envelope plus a little" separates "relayed
	// nothing" from "relayed something" without needing to know the shape of
	// each response. A COMMAND (RobokitCommand) has no payload: the robot's
	// whole answer is a 57-byte {"create_on":...,"ret_code":0}, which this floor
	// would refuse every time, so a command is judged by the robot's ret_code.
	if !req.command && len(data) < minRelayedBody {
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

// RobokitCommand relays a Robokit COMMAND through RDS: a call that changes
// something on the robot and answers only whether it took it.
//
// SUCCESS IS THE ROBOT'S ret_code, PRESENT AND ZERO, and nothing else. The
// relay's own guard (GeneralRobokitAPI) refuses a non-zero `code` or `ret_code`;
// what it cannot refuse is RDS's {"code":0,"msg":"ok"}, which is RDS saying it
// answered and the robot did not (the 56-byte envelope measured at
// Springfield 2026-08-07). A command has no payload to measure, so the size
// floor GeneralRobokitAPI uses cannot tell the two apart; the robot's own
// field can. Measured at Hopkinsville 2026-11-10, AMR-05, #3057: the robot's
// answer is {"create_on":"…","ret_code":0}, 57 bytes
// (turntable-bintask-2026-11-10/turn3057_145043.log).
func (c *Client) RobokitCommand(req GeneralRobokitRequest, timeout time.Duration) error {
	req.command = true
	data, err := c.GeneralRobokitAPI(req, timeout)
	if err != nil {
		return err
	}
	var probe struct {
		RetCode *int `json:"ret_code"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return fmt.Errorf("rds robokit: vehicle %s port %d code %d: the reply is not JSON (%v): %s",
			req.Vehicle, req.Port, req.Code, err, truncate(data, 256))
	}
	if probe.RetCode == nil {
		return fmt.Errorf("rds robokit: vehicle %s port %d code %d: RDS answered with no ret_code (%s) -- "+
			"RDS took the call and the robot did not answer it",
			req.Vehicle, req.Port, req.Code, truncate(data, 256))
	}
	return nil // GeneralRobokitAPI has already refused a non-zero ret_code
}

// RobokitCodeSpin is #3057, robot_task_spin_req: turn the jack table. SEER's
// names run the other way round from what they describe: "spin" (3057) turns
// the table, "turn" (3056) rotates the robot body.
const RobokitCodeSpin = 3057

// TurnTable sends #3057 on the navigation port: turn the robot's jack table to
// rad, an ABSOLUTE angle in the robot's frame, by the nearest way. Absolute on
// purpose: sending it twice turns the table once, so a retry after a lost
// answer is safe. increase_spin_angle, the relative form, would turn it twice
// and is never sent. The robot holds task_status at 2 for the whole turn
// (measured 2026-11-10, about 8 s for 90°).
func (c *Client) TurnTable(vehicle string, rad float64, timeout time.Duration) error {
	return c.RobokitCommand(GeneralRobokitRequest{
		Vehicle: vehicle,
		Port:    RobokitPortNav,
		Code:    RobokitCodeSpin,
		Cmd:     TableSpinCmd{RobotSpinAngle: rad, SpinDirection: 0},
	}, timeout)
}

// TableSpinCmd is #3057's body. SpinDirection 0 is the nearest way round (1
// counterclockwise, -1 clockwise).
type TableSpinCmd struct {
	RobotSpinAngle float64 `json:"robot_spin_angle"`
	SpinDirection  int     `json:"spin_direction"`
}

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
