package rds

import (
	"encoding/json"
	"maps"
	"net/http"
	"net/http/httptest"
	"shingo/protocol/testutil"
	"testing"
	"time"
)

func fireAlarmServer(t *testing.T, isFire bool, createOn string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/isFire":
			json.NewEncoder(w).Encode(map[string]any{
				"code":      0,
				"msg":       "ok",
				"is_fire":   isFire,
				"create_on": createOn,
			})
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestGetFireAlarmStatus_Clear(t *testing.T) {
	t.Parallel()
	srv := fireAlarmServer(t, false, "2024-01-15T08:00:00Z")
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	status, err := c.GetFireAlarmStatus()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.IsFire {
		t.Error("expected IsFire=false")
	}
	if status.ChangedAt != "2024-01-15T08:00:00Z" {
		t.Errorf("expected ChangedAt to be preserved, got %q", status.ChangedAt)
	}
}

func TestGetFireAlarmStatus_Active(t *testing.T) {
	t.Parallel()
	srv := fireAlarmServer(t, true, "2024-01-15T10:30:00Z")
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	status, err := c.GetFireAlarmStatus()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !status.IsFire {
		t.Error("expected IsFire=true")
	}
	if status.ChangedAt != "2024-01-15T10:30:00Z" {
		t.Errorf("expected ChangedAt='2024-01-15T10:30:00Z', got %q", status.ChangedAt)
	}
}

func TestGetFireAlarmStatus_EpochZeroSuppressed(t *testing.T) {
	t.Parallel()
	srv := fireAlarmServer(t, false, "1970-01-01T00:00:00Z")
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	status, err := c.GetFireAlarmStatus()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if status.ChangedAt != "" {
		t.Errorf("expected epoch-zero suppressed to empty, got %q", status.ChangedAt)
	}
}

func TestSetFireAlarm(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name           string
		on, autoResume bool
		want           map[string]any
	}{
		{"trigger", true, false, map[string]any{"on": true}},
		{"clear and resume", false, true, map[string]any{"on": false, "autoResume": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var got map[string]any
			srv, c := testServer(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost || r.URL.Path != "/fireOperations" {
					t.Errorf("request = %s %s, want POST /fireOperations", r.Method, r.URL.Path)
				}
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Errorf("decode request: %v", err)
				}
				if err := json.NewEncoder(w).Encode(Response{Code: 0, Msg: "ok"}); err != nil {
					t.Errorf("write the reply: %v", err)
				}
			})
			defer srv.Close()

			testutil.MustNoErr(t, c.SetFireAlarm(tc.on, tc.autoResume), "unexpected error")
			if !maps.Equal(got, tc.want) {
				t.Errorf("SetFireAlarm(%v, %v) sent %v, want %v", tc.on, tc.autoResume, got, tc.want)
			}
		})
	}
}
