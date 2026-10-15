package rds

import (
	"encoding/json"
	"math"
	"testing"
)

// laserFixture is a SYNTHETIC robot_status_laser body in the live shape (DATA
// §10): an h100-shaped nav lidar, a c200-shaped obstacle lidar, and a lidar
// with no valid beam. Robokit omits a beam's empty keys, so two of the h100's
// invalid beams carry only angle and rssi, and the c200 has no use_forLoc key.
const laserFixture = `{"create_on":"2026-10-15T00:00:00Z","ret_code":0,"lasers":[
 {"beams":[
   {"angle":-90,"dist":2.5,"rssi":30.06,"valid":true},
   {"angle":-89.5,"dist":3.0,"rssi":33.06,"valid":true},
   {"angle":-89,"dist":1.2,"rssi":55.06,"valid":true},
   {"angle":-88.5,"dist":4.0,"rssi":31.06,"valid":true},
   {"angle":-88,"rssi":30},
   {"angle":-87.5,"rssi":30},
   {"angle":-87,"dist":0,"rssi":30,"valid":false}],
  "device_info":{"device_name":"laser","max_angle":90,"min_angle":-90},
  "header":{"frame_id":"FocueRay-h100-TCP"},
  "install_info":{"upside":true,"x":0.3,"z":0.2},
  "use_forLoc":true},
 {"beams":[
   {"angle":-90,"dist":1.5,"rssi":30.0,"valid":true},
   {"angle":-89.5,"dist":0.2,"rssi":47,"valid":true},
   {"angle":-89,"dist":2.0,"rssi":44,"valid":true},
   {"angle":-88.5,"dist":0,"rssi":30,"valid":false}],
  "device_info":{"device_name":"laser2"},
  "header":{"frame_id":"FocueRay-c200-TCP"}},
 {"beams":[{"angle":0,"rssi":30},{"angle":1,"rssi":30}],
  "device_info":{"device_name":"laser3"}}
]}`

func approx(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil || math.Abs(*got-want) > 1e-9 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// TestReduceLaserFrame pins the reduction against a fixture carrying both h100
// shapes (valid whole+0.06 values, exactly-30.0 invalid fills with keys
// omitted) and a VALID c200 beam at exactly 30.0 that must be kept: exclusion
// is by the valid flag, never by the rssi value. Quartiles are linear
// interpolation (the wipe probes' pct()).
func TestReduceLaserFrame(t *testing.T) {
	t.Parallel()
	var f LaserFrame
	if err := json.Unmarshal([]byte(laserFixture), &f); err != nil {
		t.Fatal(err)
	}
	got := ReduceLaserFrame(&f)
	if len(got) != 3 {
		t.Fatalf("got %d lidars, want 3", len(got))
	}

	nav := got[0]
	if nav.Lidar != "laser" || !nav.UseForLoc || nav.Beams != 7 || nav.Valid != 4 || nav.NearValid != 0 {
		t.Errorf("nav = %+v, want laser use_forLoc 7 beams 4 valid 0 near", nav)
	}
	// valid rssi sorted: 30.06 31.06 33.06 55.06 — the invalid 30.0 fills are out.
	approx(t, "nav p25", nav.RSSIP25Valid, 30.81)
	approx(t, "nav median", nav.RSSIMedValid, 32.06)
	approx(t, "nav p75", nav.RSSIP75Valid, 38.56)

	obst := got[1]
	if obst.Lidar != "laser2" || obst.UseForLoc || obst.Beams != 4 || obst.Valid != 3 || obst.NearValid != 1 {
		t.Errorf("obstacle = %+v, want laser2 !use_forLoc 4 beams 3 valid 1 near", obst)
	}
	// valid rssi sorted: 30.0 44 47. Dropping the valid 30.0 would make the
	// median 45.5; keeping it makes it 44.
	approx(t, "obstacle p25", obst.RSSIP25Valid, 37)
	approx(t, "obstacle median", obst.RSSIMedValid, 44)
	approx(t, "obstacle p75", obst.RSSIP75Valid, 45.5)

	none := got[2]
	if none.Beams != 2 || none.Valid != 0 || none.RSSIP25Valid != nil || none.RSSIMedValid != nil || none.RSSIP75Valid != nil {
		t.Errorf("no-valid lidar = %+v, want 2 beams, 0 valid, nil quartiles", none)
	}
}

// TestReduceLaserFrame_Empty: a frame with no lasers is an empty slice, not
// an error and not nil.
func TestReduceLaserFrame_Empty(t *testing.T) {
	t.Parallel()
	var f LaserFrame
	if err := json.Unmarshal([]byte(`{"ret_code":0,"lasers":[]}`), &f); err != nil {
		t.Fatal(err)
	}
	if got := ReduceLaserFrame(&f); got == nil || len(got) != 0 {
		t.Errorf("empty frame = %#v, want empty non-nil slice", got)
	}
}
