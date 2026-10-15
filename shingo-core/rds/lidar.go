package rds

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"
)

// RobokitCodeLaser is robot_status_laser: one frame from every lidar on the
// robot, on the STATUS port. It sits beside the Robokit codes in robokit.go;
// it is declared here because this file is its only user.
//
// Verified 2026-10-15 at both plants (DATA-laser-frame-gauge.md §10, the
// airknife working folder at the GitHub root): through /generalRobokitAPI it
// returns the full ~37 KB frame in 44–81 ms, the same numbers as a direct call
// within one beam. So it goes through RDS's relay like the map fetch, and Core
// never opens a connection to a robot.
const RobokitCodeLaser = 1009

// LaserNearMetres is the distance under which a VALID beam counts as near:
// something within 0.3 m of the window, as DATA §3 counted it.
const LaserNearMetres = 0.3

// LaserFrame is the part of a robot_status_laser response this package reads.
// Robokit omits a beam's empty keys: an invalid beam may carry only angle and
// rssi, so a missing valid is false and a missing dist is 0. A missing
// use_forLoc is false.
type LaserFrame struct {
	Lasers []struct {
		Beams []struct {
			Dist  float64 `json:"dist"`
			RSSI  float64 `json:"rssi"`
			Valid bool    `json:"valid"`
		} `json:"beams"`
		DeviceInfo struct {
			DeviceName string `json:"device_name"`
		} `json:"device_info"`
		UseForLoc bool `json:"use_forLoc"`
	} `json:"lasers"`
}

// LaserStats is one lidar's frame reduced to the numbers that are kept. The
// beams themselves are discarded.
//
// The rssi quartiles are over VALID beams only, and nil when no beam was
// valid. Exclusion is by the robot's own valid flag and never by the rssi
// value: an h100 fills its invalid beams with exactly 30.0, but a c200 reports
// real returns at exactly 30.0 too, so "drop the 30.0s" would delete real data
// from one model to clean the other.
type LaserStats struct {
	Lidar        string
	UseForLoc    bool
	Beams        int
	Valid        int
	RSSIP25Valid *float64
	RSSIMedValid *float64
	RSSIP75Valid *float64
	NearValid    int
}

// ReduceLaserFrame reduces every lidar in a frame. An empty frame is an empty
// slice, not an error: a robot with no lidar data has nothing to say.
func ReduceLaserFrame(f *LaserFrame) []LaserStats {
	out := make([]LaserStats, 0, len(f.Lasers))
	for _, l := range f.Lasers {
		s := LaserStats{Lidar: l.DeviceInfo.DeviceName, UseForLoc: l.UseForLoc, Beams: len(l.Beams)}
		var rssi []float64
		for _, b := range l.Beams {
			if !b.Valid {
				continue
			}
			s.Valid++
			rssi = append(rssi, b.RSSI)
			if b.Dist < LaserNearMetres {
				s.NearValid++
			}
		}
		if len(rssi) > 0 {
			sort.Float64s(rssi)
			p25, med, p75 := quantile(rssi, 0.25), quantile(rssi, 0.5), quantile(rssi, 0.75)
			s.RSSIP25Valid, s.RSSIMedValid, s.RSSIP75Valid = &p25, &med, &p75
		}
		out = append(out, s)
	}
	return out
}

// quantile is linear interpolation between closest ranks over sorted xs — the
// pct() of the wipe probes that produced the DATA tables, so a stored number
// and a number in those tables mean the same thing. At p = 0.5 it equals
// Python's statistics.median (the mean of the middle two for an even count).
func quantile(sorted []float64, p float64) float64 {
	k := float64(len(sorted)-1) * p
	f := int(k)
	c := f + 1
	if c > len(sorted)-1 {
		c = len(sorted) - 1
	}
	return sorted[f] + (sorted[c]-sorted[f])*(k-float64(f))
}

// ReadLaserStats asks one robot for a laser frame through RDS's relay and
// reduces it. The relay's own guards (an error status in the body, a body too
// small to hold a payload) are the error path; a body that does not decode is
// an error too.
func (c *Client) ReadLaserStats(vehicle string, timeout time.Duration) ([]LaserStats, error) {
	data, err := c.GeneralRobokitAPI(GeneralRobokitRequest{
		Vehicle: vehicle,
		Port:    RobokitPortStatus,
		Code:    RobokitCodeLaser,
		Cmd:     map[string]bool{"return_beams3D": false},
	}, timeout)
	if err != nil {
		return nil, err
	}
	var f LaserFrame
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, fmt.Errorf("rds robokit laser frame: %w", err)
	}
	return ReduceLaserFrame(&f), nil
}
