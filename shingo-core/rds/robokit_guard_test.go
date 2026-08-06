package rds

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The proxy guard, against the bodies Springfield RDS actually returns.
//
// Both cases were MEASURED on 2026-08-07, and both used to pass through as
// success: the status probe read `ret_code`, which RDS never sends, and
// nothing at all checked for a relay that succeeded and returned no payload.
func TestGeneralRobokitAPI_RejectsWhatRDSActuallyReturns(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{
			// The real error shape. `code`, not `ret_code` -- the whole reason
			// the old guard never fired.
			name: "non-zero code",
			body: `{"code":50001,"create_on":"2026-08-07T19:14:26Z","msg":"generalRobokitAPI error: cmd is not json object"}`,
			want: "code 50001",
		},
		{
			// The documented spelling, kept because the vendor uses one and
			// documents the other.
			name: "non-zero ret_code",
			body: `{"ret_code":1001,"err_msg":"robot unreachable"}`,
			want: "ret_code 1001",
		},
		{
			// THE ONE A STATUS FIELD CANNOT CATCH. RDS accepts the call, does
			// not reach the robot, and reports success in 56 bytes. Every valid
			// call at Springfield returns exactly this.
			name: "status ok but nothing relayed",
			body: `{"code":0,"create_on":"2026-08-07T19:13:26Z","msg":"ok"}`,
			want: "relayed no payload",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK) // the proxy always says 200
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()

			c := NewClient(srv.URL, time.Second)
			got, err := c.GeneralRobokitAPI(GeneralRobokitRequest{
				Vehicle: "AMR-01", Port: RobokitPortConfig, Code: RobokitCodeDownloadMap,
			}, 5*time.Second)
			if err == nil {
				t.Fatalf("accepted a failed relay as success (%d bytes) -- this is the "+
					"only guard on the path, and downstream refusal is luck rather "+
					"than design: an error body that PARSED as an empty map would "+
					"version every area and reflector to gone", len(got))
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

// A real payload still passes. The size floor must not reject a short but
// genuine response.
func TestGeneralRobokitAPI_AcceptsARelayedPayload(t *testing.T) {
	t.Parallel()
	body := `{"code":0,"msg":"ok","current_map":"SPRAMRMAP","current_map_md5":"a54877","map_files_info":[{"name":"SPRAMRMAP","size":7654321}],"padding":"` +
		strings.Repeat("x", 120) + `"}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	c := NewClient(srv.URL, time.Second)
	got, err := c.GeneralRobokitAPI(GeneralRobokitRequest{
		Vehicle: "AMR-01", Port: RobokitPortStatus, Code: RobokitCodeMapList,
	}, 5*time.Second)
	if err != nil {
		t.Fatalf("rejected a genuine payload: %v", err)
	}
	if len(got) != len(body) {
		t.Errorf("got %d bytes, want %d -- the body must arrive unaltered, or the "+
			"content hash over it means nothing", len(got), len(body))
	}
}
