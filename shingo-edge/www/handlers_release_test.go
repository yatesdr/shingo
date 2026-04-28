package www

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestParseReleaseRequest_RejectsBareBody verifies the post-2026-04-27
// contract: an empty Content-Length body produces an error suitable for a
// 400 response. This is the exact silent-payload fingerprint c56ceb9 was
// written to close.
func TestParseReleaseRequest_RejectsBareBody(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/release", nil)
	// httptest.NewRequest with nil body sets ContentLength = 0.

	_, err := parseReleaseRequest(r)
	if err == nil {
		t.Fatal("parseReleaseRequest accepted bare body, want error")
	}
	if !strings.Contains(err.Error(), "called_by") {
		t.Errorf("error %q does not mention called_by", err.Error())
	}
}

// TestParseReleaseRequest_RejectsEmptyCalledBy verifies that a well-formed
// JSON body with a missing or whitespace-only called_by is rejected. The
// TrimSpace check matters because a client posting `{"called_by": "  "}`
// would otherwise produce the same empty-audit-trail symptom as a bare
// body.
func TestParseReleaseRequest_RejectsEmptyCalledBy(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"missing", `{}`},
		{"empty", `{"called_by": ""}`},
		{"whitespace", `{"called_by": "   "}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/release",
				bytes.NewBufferString(tc.body))
			r.ContentLength = int64(len(tc.body))

			_, err := parseReleaseRequest(r)
			if err == nil {
				t.Fatalf("parseReleaseRequest accepted body %q, want error", tc.body)
			}
			if !strings.Contains(err.Error(), "called_by") {
				t.Errorf("error %q does not mention called_by", err.Error())
			}
		})
	}
}

// TestParseReleaseRequest_RejectsMalformedJSON verifies that a malformed
// body is surfaced as an error rather than falling through. The decoder
// error is wrapped (returned as-is) so the caller can decide whether to
// expose the raw message or sanitize it; current callers pass it through
// to writeError.
func TestParseReleaseRequest_RejectsMalformedJSON(t *testing.T) {
	body := `{"called_by": "ok", "qty_by_part": 123}` // qty_by_part should be a map
	r := httptest.NewRequest(http.MethodPost, "/release", bytes.NewBufferString(body))
	r.ContentLength = int64(len(body))

	_, err := parseReleaseRequest(r)
	if err == nil {
		t.Fatal("parseReleaseRequest accepted malformed body, want error")
	}
}

// TestParseReleaseRequest_AcceptsWellFormedBody verifies the happy path:
// a body with called_by and any combination of disposition + qty_by_part
// is accepted, and every field flows through to the returned struct.
func TestParseReleaseRequest_AcceptsWellFormedBody(t *testing.T) {
	body := `{"disposition": "capture_lineside", "qty_by_part": {"PART-A": 5}, "called_by": "stephen-station-7"}`
	r := httptest.NewRequest(http.MethodPost, "/release", bytes.NewBufferString(body))
	r.ContentLength = int64(len(body))

	req, err := parseReleaseRequest(r)
	if err != nil {
		t.Fatalf("parseReleaseRequest: %v", err)
	}
	if req.Disposition != "capture_lineside" {
		t.Errorf("Disposition = %q, want %q", req.Disposition, "capture_lineside")
	}
	if req.CalledBy != "stephen-station-7" {
		t.Errorf("CalledBy = %q, want %q", req.CalledBy, "stephen-station-7")
	}
	if req.QtyByPart["PART-A"] != 5 {
		t.Errorf("QtyByPart[PART-A] = %d, want 5", req.QtyByPart["PART-A"])
	}
}

// TestParseReleaseRequest_AcceptsMinimalBody verifies that a body carrying
// only called_by (no disposition, no qty_by_part) is accepted. This is the
// shape that legacy first-party clients post when releasing without a
// chosen disposition — the engine maps it to the zero-value
// ReleaseDisposition (no manifest action at Core).
func TestParseReleaseRequest_AcceptsMinimalBody(t *testing.T) {
	body := `{"called_by": "kanban-page"}`
	r := httptest.NewRequest(http.MethodPost, "/release", bytes.NewBufferString(body))
	r.ContentLength = int64(len(body))

	req, err := parseReleaseRequest(r)
	if err != nil {
		t.Fatalf("parseReleaseRequest: %v", err)
	}
	if req.CalledBy != "kanban-page" {
		t.Errorf("CalledBy = %q, want %q", req.CalledBy, "kanban-page")
	}
	if req.Disposition != "" {
		t.Errorf("Disposition = %q, want empty", req.Disposition)
	}
	if req.QtyByPart != nil {
		t.Errorf("QtyByPart = %v, want nil", req.QtyByPart)
	}
}
