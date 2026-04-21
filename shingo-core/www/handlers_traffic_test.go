//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"shingocore/config"
)

// Characterization tests for handlers_traffic.go — the traffic (count-group)
// page and its save/add/delete form endpoints. Uses the same temp config.yaml
// pattern as handlers_config_test.go so cfg.Save calls round-trip to disk.

// --- handleTraffic ----------------------------------------------------------

// TestHandleTraffic_RendersHTML pins the page render path. Seeds two count
// groups on the in-memory config so both table rows show up in the rendered
// HTML.
func TestHandleTraffic_RendersHTML(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "zone-A", Enabled: true},
		{Name: "zone-B", Enabled: false},
	}
	cfg.Unlock()

	req := httptest.NewRequest(http.MethodGet, "/traffic", nil)
	rec := httptest.NewRecorder()
	h.handleTraffic(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Traffic Zones") {
		t.Errorf("expected 'Traffic Zones' heading in body")
	}
	if !strings.Contains(body, "zone-A") || !strings.Contains(body, "zone-B") {
		t.Errorf("seeded zone names missing from rendered body")
	}
}

// TestHandleTraffic_SavedBanner pins the `?saved=added` query surfaces the
// "Group added." banner in the page.
func TestHandleTraffic_SavedBanner(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	req := httptest.NewRequest(http.MethodGet, "/traffic?saved=added", nil)
	rec := httptest.NewRecorder()
	h.handleTraffic(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "Group added") {
		t.Errorf("expected 'Group added' banner")
	}
}

// TestHandleTraffic_ErrorBanner pins the `?err=duplicate` query → the
// "already exists" banner.
func TestHandleTraffic_ErrorBanner(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	req := httptest.NewRequest(http.MethodGet, "/traffic?err=duplicate", nil)
	rec := httptest.NewRecorder()
	h.handleTraffic(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "already exists") {
		t.Errorf("expected duplicate-name error banner")
	}
}

// --- apiTrafficGroups -------------------------------------------------------

// TestApiTrafficGroups_EmptyConfig pins the JSON shape on the default config
// (no groups): empty list, 200 OK.
func TestApiTrafficGroups_EmptyConfig(t *testing.T) {
	h, _ := testHandlers(t)

	rec := getPlain(t, h.apiTrafficGroups, "/api/traffic/groups")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var groups []config.CountGroupConfig
	if err := json.NewDecoder(rec.Body).Decode(&groups); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(groups) != 0 {
		t.Errorf("expected empty groups, got %d", len(groups))
	}
}

// TestApiTrafficGroups_SeededConfig pins that the API faithfully echoes the
// in-memory CountGroups.Groups list.
func TestApiTrafficGroups_SeededConfig(t *testing.T) {
	h, _ := testHandlers(t)
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "g1", Enabled: true},
		{Name: "g2", Enabled: false},
	}
	cfg.Unlock()

	rec := getPlain(t, h.apiTrafficGroups, "/api/traffic/groups")
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var groups []config.CountGroupConfig
	if err := json.NewDecoder(rec.Body).Decode(&groups); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(groups) != 2 || groups[0].Name != "g1" || groups[1].Name != "g2" {
		t.Errorf("groups: got %+v", groups)
	}
	if !groups[0].Enabled || groups[1].Enabled {
		t.Errorf("enabled flags: got %+v", groups)
	}
}

// --- handleTrafficSave ------------------------------------------------------

// TestHandleTrafficSave_ReplacesGroups pins the form-decode contract: the
// handler collects group_name_N / group_enabled_N pairs and replaces the
// in-memory list entirely, persists to disk, and redirects with
// ?saved=groups.
func TestHandleTrafficSave_ReplacesGroups(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	// Seed a pre-existing group that should NOT survive the save (replace-all
	// semantics).
	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "pre-existing", Enabled: true},
	}
	cfg.Unlock()

	form := url.Values{}
	form.Set("group_name_0", "zone-new-1")
	form.Set("group_enabled_0", "on")
	form.Set("group_name_1", "zone-new-2")
	// group_enabled_1 omitted → enabled=false
	form.Set("group_name_2", "")        // blank name → skipped
	form.Set("group_enabled_2", "on")

	rec := postForm(t, h.handleTrafficSave, "/traffic/save", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "saved=groups") {
		t.Errorf("redirect: got %q, want '...saved=groups'", loc)
	}

	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 2 {
		t.Fatalf("groups after save: got %d, want 2: %+v",
			len(cfg.CountGroups.Groups), cfg.CountGroups.Groups)
	}
	if cfg.CountGroups.Groups[0].Name != "zone-new-1" || !cfg.CountGroups.Groups[0].Enabled {
		t.Errorf("group[0]: got %+v", cfg.CountGroups.Groups[0])
	}
	if cfg.CountGroups.Groups[1].Name != "zone-new-2" || cfg.CountGroups.Groups[1].Enabled {
		t.Errorf("group[1]: got %+v", cfg.CountGroups.Groups[1])
	}
}

// TestHandleTrafficSave_EmptyFormClearsGroups pins that submitting an empty
// form wipes the group list (the loop finds no group_name_0 and returns).
func TestHandleTrafficSave_EmptyFormClearsGroups(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{{Name: "to-wipe", Enabled: true}}
	cfg.Unlock()

	rec := postForm(t, h.handleTrafficSave, "/traffic/save", url.Values{})
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}

	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 0 {
		t.Errorf("expected groups cleared, got %+v", cfg.CountGroups.Groups)
	}
}

// --- handleTrafficAdd -------------------------------------------------------

// TestHandleTrafficAdd_HappyPath pins the happy path: name arrives → appended
// with Enabled=true and redirected with ?saved=added.
func TestHandleTrafficAdd_HappyPath(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("name", "newzone")

	rec := postForm(t, h.handleTrafficAdd, "/traffic/add", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "saved=added") {
		t.Errorf("redirect: got %q, want '...saved=added'", loc)
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 1 ||
		cfg.CountGroups.Groups[0].Name != "newzone" ||
		!cfg.CountGroups.Groups[0].Enabled {
		t.Errorf("groups after add: got %+v", cfg.CountGroups.Groups)
	}
}

// TestHandleTrafficAdd_EmptyName pins that empty name → redirect with
// ?err=empty and no mutation.
func TestHandleTrafficAdd_EmptyName(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	form := url.Values{}
	form.Set("name", "   ") // whitespace → trimmed to empty

	rec := postForm(t, h.handleTrafficAdd, "/traffic/add", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=empty") {
		t.Errorf("redirect: got %q, want '...err=empty'", loc)
	}

	cfg := h.engine.AppConfig()
	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 0 {
		t.Errorf("no group should be added on empty name, got %+v",
			cfg.CountGroups.Groups)
	}
}

// TestHandleTrafficAdd_Duplicate pins the duplicate-name guard: adding a
// name that already exists → redirect with ?err=duplicate, no mutation.
func TestHandleTrafficAdd_Duplicate(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "zone-dup", Enabled: true},
	}
	cfg.Unlock()

	form := url.Values{}
	form.Set("name", "zone-dup")

	rec := postForm(t, h.handleTrafficAdd, "/traffic/add", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "err=duplicate") {
		t.Errorf("redirect: got %q, want '...err=duplicate'", loc)
	}

	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 1 {
		t.Errorf("duplicate add should not grow the list; got %+v", cfg.CountGroups.Groups)
	}
}

// --- handleTrafficDelete ----------------------------------------------------

// TestHandleTrafficDelete_HappyPath pins the delete path: name matches →
// group is removed and redirect carries ?saved=deleted.
func TestHandleTrafficDelete_HappyPath(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "keep", Enabled: true},
		{Name: "remove-me", Enabled: false},
	}
	cfg.Unlock()

	form := url.Values{}
	form.Set("name", "remove-me")

	rec := postForm(t, h.handleTrafficDelete, "/traffic/delete", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "saved=deleted") {
		t.Errorf("redirect: got %q, want '...saved=deleted'", loc)
	}

	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 1 || cfg.CountGroups.Groups[0].Name != "keep" {
		t.Errorf("after delete: got %+v, want [keep]", cfg.CountGroups.Groups)
	}
}

// TestHandleTrafficDelete_UnknownName pins that deleting a non-existent name
// is a no-op (still redirects 303, no mutation).
func TestHandleTrafficDelete_UnknownName(t *testing.T) {
	h, _, _ := testHandlersWithConfigPath(t)

	cfg := h.engine.AppConfig()
	cfg.Lock()
	cfg.CountGroups.Groups = []config.CountGroupConfig{
		{Name: "keep-1", Enabled: true},
	}
	cfg.Unlock()

	form := url.Values{}
	form.Set("name", "nonexistent")

	rec := postForm(t, h.handleTrafficDelete, "/traffic/delete", form)
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303", rec.Code)
	}

	cfg.Lock()
	defer cfg.Unlock()
	if len(cfg.CountGroups.Groups) != 1 {
		t.Errorf("unknown-name delete should not change groups; got %+v",
			cfg.CountGroups.Groups)
	}
}
