package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestCoreErrorText_ReadsBothShapesCoreAnswersIn is the 115 unattributable log
// lines, in one test.
//
// Core answers a failure two ways. The success-shaped handlers reply
// {"status":"error","detail":"..."}; the generic refusals go through
// www.jsonError, which writes {"error":"..."}. The three bin response types
// declared only `detail`, so every jsonError refusal decoded to a zero struct
// and the caller logged the bare status code.
//
// Sim 2026-08-30: the sim operator's auto-clear failed 115 times in one run,
// eight retries apiece, every line reading `clear bin: core returned 400`. The
// sentence Core actually sent — "no bin at node FGN_001" — went into a field
// that did not exist.
func TestCoreErrorText_ReadsBothShapesCoreAnswersIn(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name            string
		detail, errText string
		status          int
		want            string
	}{
		{"detail wins when both are present", "richer detail", "generic error", 400, "richer detail"},
		{"the jsonError shape is no longer thrown away", "", "no bin at node FGN_001", 400, "no bin at node FGN_001"},
		{"the status code is the last resort, never nothing", "", "", 503, "core returned 503"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := coreErrorText(tc.detail, tc.errText, tc.status); got != tc.want {
				t.Errorf("coreErrorText(%q, %q, %d) = %q, want %q", tc.detail, tc.errText, tc.status, got, tc.want)
			}
		})
	}
}

// TestClearBin_SurfacesCoresReason drives the real decode path against a server
// answering exactly as www.jsonError does, so the struct tag and the helper are
// proven together rather than one at a time.
func TestClearBin_SurfacesCoresReason(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": "no bin at node FGN_001"})
	}))
	t.Cleanup(srv.Close)

	_, err := NewCoreClient(srv.URL).ClearBin("FGN_001", "")
	if err == nil {
		t.Fatal("ClearBin returned no error on a 400")
	}
	if !strings.Contains(err.Error(), "no bin at node FGN_001") {
		t.Errorf("error = %q, want Core's own sentence — a bare status code is what made 115 "+
			"failures unattributable", err)
	}
}
