package seerrds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestReadLidarFrames_ThroughTheRelay pins the one path a laser frame takes:
// a POST to RDS's /generalRobokitAPI naming the robot, the status port and
// code 1009 with return_beams3D false — never a robot address — reduced to
// LidarStats. The body is synthetic.
func TestReadLidarFrames_ThroughTheRelay(t *testing.T) {
	t.Parallel()
	var got map[string]any
	beams := strings.Repeat(`{"angle":0,"dist":2.0,"rssi":40.06,"valid":true},`, 9) +
		`{"angle":1,"rssi":30}`
	body := `{"ret_code":0,"lasers":[{"beams":[` + beams + `],"device_info":{"device_name":"laser"},"use_forLoc":true}]}`

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/generalRobokitAPI" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		w.Write([]byte(body))
	}))
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})

	stats, err := a.ReadLidarFrames("AMR-01")
	if err != nil {
		t.Fatalf("ReadLidarFrames: %v", err)
	}
	if got["vehicle"] != "AMR-01" || got["port"] != float64(19204) || got["code"] != float64(1009) {
		t.Errorf("relay request = %v, want vehicle AMR-01 port 19204 code 1009", got)
	}
	if cmd, _ := got["cmd"].(map[string]any); cmd == nil || cmd["return_beams3D"] != false {
		t.Errorf("cmd = %v, want return_beams3D false", got["cmd"])
	}
	if len(stats) != 1 || stats[0].Lidar != "laser" || !stats[0].UseForLoc || stats[0].Beams != 10 ||
		stats[0].Valid != 9 || stats[0].RSSIMedValid == nil || *stats[0].RSSIMedValid != 40.06 {
		t.Errorf("stats = %+v", stats)
	}
}

// TestReadLidarFrames_EmptyRelayIsAnError: RDS answering a bare OK envelope
// (accepted, never reached the robot) is a failed read, not a robot with no
// lidar.
func TestReadLidarFrames_EmptyRelayIsAnError(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"code":0,"msg":"ok"}`))
	}))
	defer srv.Close()
	a := New(Config{BaseURL: srv.URL, Timeout: 5 * time.Second, DebugLog: func(string, ...any) {}})
	if stats, err := a.ReadLidarFrames("AMR-01"); err == nil {
		t.Fatalf("empty relay accepted as %+v", stats)
	}
}
