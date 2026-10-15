//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/testutil"
	"shingocore/store/scene"
)

// cleaningGet decodes GET /api/robots/cleaning.
func cleaningGet(t *testing.T, h *Handlers) cleaningPage {
	t.Helper()
	rec := getPlain(t, h.apiRobotCleaning, "/api/robots/cleaning")
	if rec.Code != http.StatusOK {
		t.Fatalf("GET cleaning: %d %s", rec.Code, rec.Body.String())
	}
	var p cleaningPage
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&p), "decode cleaning page")
	return p
}

func refusalOf(p cleaningPage, vehicle string) (string, bool) {
	for _, r := range p.Robots {
		if r.VehicleID == vehicle {
			return r.Refusal, true
		}
	}
	return "", false
}

// TestCleaningSetup_SaveAndFieldErrors pins the setup door: a 3 h frequency,
// a point the scene does not have and a bad clock are refused in the config
// page's shape with each error keyed by its field (points.<i> by the point's
// place); LocationMarks that are not Core nodes save; the saved setup reads
// back; and an empty list is a saved setup with the loop off.
func TestCleaningSetup_SaveAndFieldErrors(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	for _, p := range []string{"LM513", "LM514"} {
		testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{AreaName: "default", InstanceName: p,
			ClassName: "LocationMark", PropertiesJSON: "{}"}), "seed "+p)
	}

	rec := postJSON(t, h.apiRobotCleaningSetup, "/api/robots/cleaning/setup", map[string]any{
		"points": []string{"LM513", "LM999"}, "frequency": "3h",
		"active_from": "5am", "active_to": "23:00", "min_battery": 25,
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("refused save: %d %s", rec.Code, rec.Body.String())
	}
	var refused struct {
		OK     bool              `json:"ok"`
		Errors map[string]string `json:"errors"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &refused), "decode refusal")
	for _, k := range []string{"frequency", "points.1", "active_from"} {
		if refused.Errors[k] == "" {
			t.Errorf("no field error for %s: %v", k, refused.Errors)
		}
	}
	if _, ok := refused.Errors["points.0"]; ok || refused.OK {
		t.Errorf("LM513 refused or ok=true: %+v", refused)
	}
	if p := cleaningGet(t, h); p.Setup.Saved || p.Words != "off" {
		t.Errorf("a refused save left a setup: %+v words %q", p.Setup, p.Words)
	}

	rec = postJSON(t, h.apiRobotCleaningSetup, "/api/robots/cleaning/setup", map[string]any{
		"points": []string{"LM513", " LM514 ", ""}, "frequency": "4h",
		"active_from": "05:00", "active_to": "23:00", "min_battery": 30,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("good save: %d %s", rec.Code, rec.Body.String())
	}
	p := cleaningGet(t, h)
	if !p.Setup.Saved || strings.Join(p.Setup.Points, ",") != "LM513,LM514" || p.Setup.Frequency != "4h" ||
		p.Setup.ActiveFrom != "05:00" || p.Setup.MinBattery != 30 {
		t.Errorf("saved setup = %+v", p.Setup)
	}
	// The test engine runs the simulator, which cannot report a mission's
	// state, so a set-up mission is stopped and says why.
	if p.Words != "stopped: the fleet cannot report a mission's state" {
		t.Errorf("words = %q", p.Words)
	}

	rec = postJSON(t, h.apiRobotCleaningSetup, "/api/robots/cleaning/setup", map[string]any{
		"points": []string{}, "frequency": "4h", "active_from": "05:00", "active_to": "23:00", "min_battery": 30,
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("empty save: %d %s", rec.Code, rec.Body.String())
	}
	if p := cleaningGet(t, h); !p.Setup.Saved || len(p.Setup.Points) != 0 || p.Words != "off" {
		t.Errorf("empty points: saved=%v points=%v words %q, want saved, none, off", p.Setup.Saved, p.Setup.Points, p.Words)
	}
}

// TestCleaning_RefusalIsThePostsWords pins that the robot pop-up's disabled
// reason (GET /api/robots/cleaning, per robot) and the words POST clean-now
// refuses with are one sentence, at three states: nothing set up, set up on a
// fleet that cannot report a mission's state, and the opt-out reaching the
// record with an audit row.
func TestCleaning_RefusalIsThePostsWords(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)

	// The opt-out puts AMR-01 in the record, so the page has a line for it.
	rec := postJSON(t, h.apiRobotCleaningOptOut, "/api/robots/cleaning/opt-out", map[string]any{"vehicle_id": "AMR-01", "out": true})
	if rec.Code != http.StatusOK {
		t.Fatalf("opt-out: %d %s", rec.Code, rec.Body.String())
	}
	check := func(want string) {
		t.Helper()
		got, ok := refusalOf(cleaningGet(t, h), "AMR-01")
		if !ok {
			t.Fatal("AMR-01 not on the Cleaning page after its opt-out")
		}
		rec := postJSON(t, h.apiRobotCleanNow, "/api/robots/cleaning/clean-now", map[string]any{"vehicle_id": "AMR-01"})
		if rec.Code != http.StatusConflict {
			t.Fatalf("clean-now: %d %s, want 409", rec.Code, rec.Body.String())
		}
		var e map[string]string
		testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &e), "decode")
		if got != e["error"] || got != want {
			t.Errorf("page says %q, POST says %q, want both %q", got, e["error"], want)
		}
	}
	check("no clean mission is set up")

	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{AreaName: "default", InstanceName: "LM513",
		ClassName: "LocationMark", PropertiesJSON: "{}"}), "seed LM513")
	if rec := postJSON(t, h.apiRobotCleaningSetup, "/api/robots/cleaning/setup", map[string]any{
		"points": []string{"LM513"}, "frequency": "4h", "active_from": "00:00", "active_to": "00:00", "min_battery": 25,
	}); rec.Code != http.StatusOK {
		t.Fatalf("save: %d %s", rec.Code, rec.Body.String())
	}
	check("the fleet cannot report a mission's state")

	if p := cleaningGet(t, h); len(p.Robots) == 0 || !p.Robots[0].OptedOut {
		t.Errorf("opt-out not in the record: %+v", p.Robots)
	}
	var n int
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type = 'cleaning'
		AND action = 'opt_out' AND new_value LIKE 'AMR-01%'`).Scan(&n), "audit count")
	if n != 1 {
		t.Errorf("opt-out audit rows = %d, want 1 naming AMR-01", n)
	}
}

// TestRobotHours_Endpoint pins GET /api/robots/hours: the plant's zone and
// day, seven prior days, an empty fleet as an empty list, and a refused span.
func TestRobotHours_Endpoint(t *testing.T) {
	t.Parallel()
	h, _ := testHandlers(t)
	rec := getPlain(t, h.apiRobotHours, "/api/robots/hours?days=7")
	if rec.Code != http.StatusOK {
		t.Fatalf("hours: %d %s", rec.Code, rec.Body.String())
	}
	var v struct {
		Zone      string `json:"zone"`
		Today     string `json:"today"`
		PriorDays int    `json:"prior_days"`
		Robots    []any  `json:"robots"`
	}
	testutil.MustNoErr(t, json.Unmarshal(rec.Body.Bytes(), &v), "decode")
	if v.Zone == "" || len(v.Today) != 10 || v.PriorDays != 7 || v.Robots == nil || len(v.Robots) != 0 {
		t.Errorf("hours = %+v", v)
	}
	if rec := getPlain(t, h.apiRobotHours, "/api/robots/hours?days=400"); rec.Code != http.StatusBadRequest {
		t.Errorf("days=400: %d, want 400", rec.Code)
	}
}

// TestRobotsPage_TabsAndTheirDoors pins the Robots page's §22 tabs and the
// doors its tabs call: every tab button has its panel; every URL the Fleet and
// Cleaning modules and the pop-up fetch is a route on the real router; and the
// Diagnostics page no longer carries the E-Maint tab it moved from.
func TestRobotsPage_TabsAndTheirDoors(t *testing.T) {
	h, _ := testHandlersForPages(t)
	router, ok := realRouterFor(t, h).(*chi.Mux)
	if !ok {
		t.Fatal("NewRouter is not a *chi.Mux")
	}

	page, err := os.ReadFile("templates/robots.html")
	testutil.MustNoErr(t, err, "read robots.html")
	tabRe := regexp.MustCompile(`class="tab[^"]*" data-tab="(\w+)" data-action="switchTab:(\w+)"`)
	tabs := tabRe.FindAllStringSubmatch(string(page), -1)
	if len(tabs) != 3 {
		t.Fatalf("robots.html has %d tabs, want Robots, Fleet, Cleaning", len(tabs))
	}
	for _, m := range tabs {
		if m[1] != m[2] {
			t.Errorf("tab data-tab %q switches to %q", m[1], m[2])
		}
		if !strings.Contains(string(page), `class="tab-panel`) || !strings.Contains(string(page), `id="tab-`+m[1]+`"`) {
			t.Errorf("tab %q has no panel id=tab-%s", m[1], m[1])
		}
	}

	doors := map[string]string{ // url -> method, as the modules call them
		"/api/telemetry/e-maint":          "GET",
		"/api/telemetry/e-maint/download": "GET",
		"/api/robots/hours":               "GET",
		"/api/robots/cleaning":            "GET",
		"/api/map/marks":                  "GET",
		"/api/robots/cleaning/setup":      "POST",
		"/api/robots/cleaning/clean-now":  "POST",
		"/api/robots/cleaning/opt-out":    "POST",
	}
	var src strings.Builder
	for _, f := range []string{"static/pages/robots.js", "static/components/robot-fleet-tab.js", "static/components/robot-cleaning-tab.js", "templates/robots.html"} {
		b, err := os.ReadFile(f)
		testutil.MustNoErr(t, err, "read "+f)
		src.Write(b)
	}
	for url, method := range doors {
		if !strings.Contains(src.String(), "'"+url) && !strings.Contains(src.String(), `"`+url) {
			t.Errorf("the Robots page no longer calls %s; update this list", url)
		}
		if !router.Match(chi.NewRouteContext(), method, url) {
			t.Errorf("%s %s is not a route", method, url)
		}
	}

	diag, err := os.ReadFile("templates/diagnostics.html")
	testutil.MustNoErr(t, err, "read diagnostics.html")
	djs, err := os.ReadFile("static/pages/diagnostics.js")
	testutil.MustNoErr(t, err, "read diagnostics.js")
	for _, s := range []string{"emaint", "E-Maint", "e-maint"} {
		if strings.Contains(string(diag), s) || strings.Contains(string(djs), s) {
			t.Errorf("Diagnostics still carries %q; the E-Maint tab moved to the Robots page", s)
		}
	}
}
