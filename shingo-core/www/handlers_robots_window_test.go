//go:build docker

package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// The board's window parameters — the 24h fix and the from/to range.
//
// A separate file from handlers_robots_test.go so the window tests can name
// their own fixtures without disturbing the robot-control tests there.
//
// The assertions are on the WINDOW ECHO, not the lanes: an empty test DB has
// no rolled-up days, and window.from/to/label are the payload's own record of
// what was asked and how it was grained — exactly the part a graining bug
// would corrupt and a lane count would never see.

func getBoard(t *testing.T, h *Handlers, query string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/robots/localization"+query, nil)
	rec := httptest.NewRecorder()
	h.apiLocalizationBoard(rec, req)
	if rec.Code != http.StatusOK {
		return rec, nil
	}
	var body struct {
		Window map[string]any `json:"window"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode board payload: %v", err)
	}
	return rec, body.Window
}

func TestApiLocalizationBoard_Presets(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	for _, tc := range []struct {
		query string
		label string
		days  int
	}{
		{"", "7d", 7}, // absent → the default, not an error
		{"?window=7d", "7d", 7},
		{"?window=30d", "30d", 30},
	} {
		rec, win := getBoard(t, h, tc.query)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d: %s", tc.query, rec.Code, rec.Body.String())
		}
		if win["label"] != tc.label {
			t.Errorf("%s: label = %v, want %q", tc.query, win["label"], tc.label)
		}
		if int(win["requested_days"].(float64)) != tc.days {
			t.Errorf("%s: requested_days = %v, want %d", tc.query, win["requested_days"], tc.days)
		}
	}
}

// 24h WAS a preset, and it asked for a day the roll-up never writes: today's
// rows are written tomorrow, so the window was empty every day forever. The
// 400 (not a silently-empty 200) is what makes a stale client visible instead
// of mysteriously blank.
func TestApiLocalizationBoard_24hIsRejected(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	req := httptest.NewRequest(http.MethodGet, "/api/robots/localization?window=24h", nil)
	rec := httptest.NewRecorder()
	h.apiLocalizationBoard(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("window=24h: status %d, want 400; body=%s", rec.Code, rec.Body.String())
	}
}

func TestApiLocalizationBoard_DateRange(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	// to is INCLUSIVE on the wire and exclusive in the board's own grain —
	// the conversion is this handler's job, and from/to are the payload's
	// record of whether it happened.
	rec, win := getBoard(t, h, "?from=2026-08-01&to=2026-08-13")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	wantFrom := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	wantTo := time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC) // 13th inclusive → 14th exclusive
	gotFrom, _ := time.Parse(time.RFC3339, fmt.Sprint(win["from"]))
	gotTo, _ := time.Parse(time.RFC3339, fmt.Sprint(win["to"]))
	if !gotFrom.Equal(wantFrom) || !gotTo.Equal(wantTo) {
		t.Errorf("window = %s → %s, want %s → %s (to must convert inclusive→exclusive)",
			gotFrom, gotTo, wantFrom, wantTo)
	}
	if int(win["requested_days"].(float64)) != 13 {
		t.Errorf("requested_days = %v, want 13", win["requested_days"])
	}
	if win["label"] != "2026-08-01 → 2026-08-13" {
		t.Errorf("label = %v, want the date pair", win["label"])
	}

	// A single day is the shortest legitimate range: from == to.
	rec, win = getBoard(t, h, "?from=2026-08-13&to=2026-08-13")
	if rec.Code != http.StatusOK {
		t.Fatalf("single day: status %d: %s", rec.Code, rec.Body.String())
	}
	if int(win["requested_days"].(float64)) != 1 {
		t.Errorf("single day: requested_days = %v, want 1", win["requested_days"])
	}
}

func TestApiLocalizationBoard_DateRangeRejected(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	for _, tc := range []struct {
		name  string
		query string
	}{
		{"both a preset and a range", "?window=7d&from=2026-08-01&to=2026-08-13"},
		{"from without to", "?from=2026-08-01"},
		{"to without from", "?to=2026-08-13"},
		{"unparseable from", "?from=Aug-1&to=2026-08-13"},
		{"unparseable to", "?from=2026-08-01&to=13/8/26"},
		{"from after to", "?from=2026-08-13&to=2026-08-01"},
		{"to in the future", fmt.Sprintf("?from=2026-08-01&to=%s",
			time.Now().AddDate(0, 0, 5).Format("2006-01-02"))},
		{"span over the cap", "?from=2024-01-01&to=2026-12-31"},
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/robots/localization"+tc.query, nil)
		rec := httptest.NewRecorder()
		h.apiLocalizationBoard(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400; body=%s",
				tc.name, rec.Code, rec.Body.String())
		}
	}
}

// tomorrow is the boundary the future check draws: today as an inclusive end
// is legitimate (it simply has no rolled-up rows yet, and data_days says so),
// the day after is not.
func TestApiLocalizationBoard_TodayAsEndIsAllowed(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)

	today := time.Now().UTC().Format("2006-01-02")
	rec, _ := getBoard(t, h, "?from=2026-08-01&to="+today)
	if rec.Code != http.StatusOK {
		t.Fatalf("to=today: status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
}
