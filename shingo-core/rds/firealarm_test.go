package rds

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
		case "/fireOperations":
			var req fireAlarmRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				w.WriteHeader(400)
				json.NewEncoder(w).Encode(map[string]any{"code": 50000, "msg": "parse json error"})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{
				"code":      0,
				"msg":       "ok",
				"create_on": "2024-01-15T10:30:00Z",
			})
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestGetFireAlarmStatus_Clear(t *testing.T) {
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
	srv := fireAlarmServer(t, false, "")
	defer srv.Close()

	c := NewClient(srv.URL, 5*time.Second)
	if err := c.SetFireAlarm(true, false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}
