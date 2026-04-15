package rds

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// countGroupServer builds a server that captures the request body and
// returns a programmed (status, body) pair. The body is raw JSON so
// tests can exercise both bare-array and wrapped-envelope responses.
func countGroupServer(t *testing.T, wantGroup string, status int, body string) (*httptest.Server, *Client) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robotsInCountGroup" {
			t.Errorf("path = %q, want /robotsInCountGroup", r.URL.Path)
		}
		if r.Method != http.MethodPost {
			t.Errorf("method = %q, want POST", r.Method)
		}
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Group string `json:"group"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if req.Group != wantGroup {
			t.Errorf("group = %q, want %q", req.Group, wantGroup)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	client := NewClient(srv.URL, 2*time.Second)
	return srv, client
}

func TestGetRobotsInCountGroup_BareArrayOccupied(t *testing.T) {
	srv, client := countGroupServer(t, "Crosswalk1", 200, `["AMR-01"]`)
	defer srv.Close()

	got, err := client.GetRobotsInCountGroup("Crosswalk1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0] != "AMR-01" {
		t.Fatalf("got %v, want [AMR-01]", got)
	}
}

func TestGetRobotsInCountGroup_BareArrayEmpty(t *testing.T) {
	srv, client := countGroupServer(t, "Crosswalk1", 200, `[]`)
	defer srv.Close()

	got, err := client.GetRobotsInCountGroup("Crosswalk1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestGetRobotsInCountGroup_UnknownGroupReturnsEmpty(t *testing.T) {
	// Empirical contract: unknown group is indistinguishable from
	// real-but-empty — both return 200 + [].
	srv, client := countGroupServer(t, "Bogus", 200, `[]`)
	defer srv.Close()

	got, err := client.GetRobotsInCountGroup("Bogus")
	if err != nil {
		t.Fatalf("unknown-group should not error (contract), got %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("unknown-group should return empty, got %v", got)
	}
}

func TestGetRobotsInCountGroup_WrappedEnvelopeSuccess(t *testing.T) {
	// Future-proofing: if AIVISION normalises to the standard envelope,
	// the defensive decoder must extract the array from .report.
	body := `{"code":0,"msg":"ok","create_on":"2026-04-15T20:00:00Z","report":["AMR-02","AMR-03"]}`
	srv, client := countGroupServer(t, "Crosswalk1", 200, body)
	defer srv.Close()

	got, err := client.GetRobotsInCountGroup("Crosswalk1")
	if err != nil {
		t.Fatalf("wrapped envelope should decode, got %v", err)
	}
	if len(got) != 2 || got[0] != "AMR-02" || got[1] != "AMR-03" {
		t.Fatalf("got %v, want [AMR-02 AMR-03]", got)
	}
}

func TestGetRobotsInCountGroup_MalformedJSONReturns400(t *testing.T) {
	body := `{"code":50000,"msg":"parse json error","create_on":"2026-04-15T19:58:24Z"}`
	srv, client := countGroupServer(t, "X", 400, body)
	defer srv.Close()

	_, err := client.GetRobotsInCountGroup("X")
	if err == nil {
		t.Fatalf("expected error for 400 response")
	}
	if !contains(err.Error(), "50000") {
		t.Fatalf("error should surface RDS code 50000, got %v", err)
	}
}

func TestGetRobotsInCountGroup_MissingGroupFieldReturns400(t *testing.T) {
	body := `{"code":50001,"msg":"don't have group or group is not a string","create_on":"2026-04-15T19:59:14Z"}`
	srv, client := countGroupServer(t, "X", 400, body)
	defer srv.Close()

	_, err := client.GetRobotsInCountGroup("X")
	if err == nil {
		t.Fatalf("expected error for 400 response")
	}
	if !contains(err.Error(), "50001") {
		t.Fatalf("error should surface RDS code 50001, got %v", err)
	}
}

func TestGetRobotsInCountGroup_UnknownBodyShapeErrors(t *testing.T) {
	// 200 with neither bare array nor wrapped-with-report. Must error,
	// not silently return nil/empty.
	body := `{"unexpected":"shape"}`
	srv, client := countGroupServer(t, "X", 200, body)
	defer srv.Close()

	_, err := client.GetRobotsInCountGroup("X")
	if err == nil {
		t.Fatalf("expected error for unrecognisable 200 body")
	}
	if !contains(err.Error(), "cannot decode") {
		t.Fatalf("error should mention decode failure, got %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && indexOf(s, sub) >= 0
}

func indexOf(s, sub string) int {
	n := len(s) - len(sub)
	for i := 0; i <= n; i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
