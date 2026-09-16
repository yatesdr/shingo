package www

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"

	"shingo/protocol/testutil"

	"shingoedge/domain"
	"shingoedge/store/processes"
)

// desktop_write_paths_test.go — one handler-level test per write the Processes
// page makes, posting EXACTLY the body the page posts.
//
// WHY THE BODY COMES FROM THE PAGE'S OWN CODE. Every handler here writes every
// field it decodes: there is no partial update on the process PUT, the style
// PUT or either station handler, so a field left out of a body is a field set
// to its zero value. U9d wired four writes from bodies that named only what
// the screen had changed, and every one destroyed data — a settings save
// blanked production_state and ungrouped the process, rename and Expected
// CATID came back 400 on a missing process_id (and would have blanked
// description), and an operator-screen edit blanked five columns and switched a
// live HMI off.
//
// A test with a hand-written body would have passed on all four, because the
// hand-written body would have been the correct one. So these run
// static/js/pages/desktop-bodies.js under node, take the objects the PAGE
// builds, and send those. The test cannot drift from the page, because there
// is only one builder.
//
// WHAT EACH CASE ASSERTS: the row before, the request, the row after, and
// every column the handler writes — not just the one the screen changed. The
// edited field moved; nothing else did.
//
// Skipped without node, like every other JS-backed pin in this package.

// desktopBodies is the map the node harness prints: write name → request body.
type desktopBodies map[string]map[string]any

// buildDesktopBodies runs desktop-bodies.js over fixtures written from Go and
// returns what it produced. The fixtures are the rows AS THEY STAND — the
// input the page's builders take — so the bodies are the ones a browser would
// have sent for the same rows.
func buildDesktopBodies(t *testing.T, fixture map[string]any) desktopBodies {
	t.Helper()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the desktop write-path bodies")
	}
	blob, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal fixture: %v", err)
	}
	dir := t.TempDir()
	fixturePath := filepath.Join(dir, "fixture.json")
	if err := os.WriteFile(fixturePath, blob, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// The harness is written here rather than committed: it is three lines of
	// glue, and a committed copy is a second place the builder names live.
	modulePath, err := filepath.Abs(filepath.Join("static", "js", "pages", "desktop-bodies.js"))
	if err != nil {
		t.Fatalf("resolve desktop-bodies.js: %v", err)
	}
	harness := fmt.Sprintf(`
'use strict';
const B = require(%q);
const f = require(%q);
process.stdout.write(JSON.stringify({
  'process create':      B.processCreate({ name: 'Made here', description: 'a press', group_id: f.groupID }),
  'settings save':       B.processSettings(f.process, f.draft),
  'gate':                B.processGate(true),
  'rename':              B.styleWrite(f.style, f.processID, { name: 'Renamed here' }),
  'expected catid':      B.styleWrite(f.style, f.processID, { expected_catid: 'CAT-NEW' }),
  'clone':               B.styleClone('Cloned here'),
  'mark as running':     B.processActiveStyle(f.style.id),
  'operator-screen edit': B.stationWrite(f.station, f.processID, true, { name: 'Renamed screen', note: 'a note' }),
  'operator-screen add':  B.stationWrite(null, f.processID, false, { name: 'Added screen', note: '' }),
  'claimed nodes':       B.stationNodes(f.claimedNodes),
  'adopt':               B.routingEnable(true),
  'add-to-set':          B.routingAdd('Supermarket Area', 'destination'),
  'name a flow':         B.flowPresetCreate('Two up, index', { styleID: f.style.id }),
  'name a found shape':  B.flowPresetCreate('Two up, index', { shapeKey: f.shapeKey }),
  'apply a preset':      B.flowPresetApplySave(f.style.id, f.cells, 'FP', f.station.id, { id: 7, version: 3 }),
  'save with no preset': B.flowPresetApplySave(f.style.id, f.cells, 'FP', f.station.id, null),
}));
`, filepath.ToSlash(modulePath), filepath.ToSlash(fixturePath))
	harnessPath := filepath.Join(dir, "emit.js")
	if err := os.WriteFile(harnessPath, []byte(harness), 0o644); err != nil {
		t.Fatalf("write harness: %v", err)
	}
	raw, err := exec.Command(nodePath, harnessPath).Output()
	if err != nil {
		t.Fatalf("run desktop-bodies.js: %v", err)
	}
	var out desktopBodies
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("decode bodies (%s): %v", raw, err)
	}
	if len(out) != 16 {
		t.Fatalf("the harness emitted %d bodies, want 16 — a write lost its builder", len(out))
	}
	return out
}

// TestDesktopWritePaths_EveryBodyNamesEveryColumnItsHandlerWrites is the whole
// point: each write, sent as the page sends it, then the row read back.
func TestDesktopWritePaths_EveryBodyNamesEveryColumnItsHandlerWrites(t *testing.T) {
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	groupID, err := testDB.CreateProcessGroup("WP-Group", "a group")
	if err != nil {
		t.Fatalf("create group: %v", err)
	}
	pid := seedProcess(t, "WP-Press")
	sid := seedStyle(t, "WP-Style", pid)
	stationID := seedOperatorStation(t, pid, "WP-CODE", "WP Screen")

	// The rows are given every column a value, because a column that is
	// already zero cannot show that a write zeroed it. This is the single
	// biggest reason the original four bugs went unseen.
	if err := testDB.UpdateProcess(pid, "WP-Press", "the description", "active_production", "PLC-1", "TAG-1", true); err != nil {
		t.Fatalf("seed process columns: %v", err)
	}
	if err := testDB.SetProcessGroupID(pid, &groupID); err != nil {
		t.Fatalf("seed process group: %v", err)
	}
	if err := testDB.UpdateStyle(sid, "WP-Style", "the style description", pid); err != nil {
		t.Fatalf("seed style: %v", err)
	}
	if err := testDB.SetStyleExpectedCATID(sid, "CAT-OLD"); err != nil {
		t.Fatalf("seed expected catid: %v", err)
	}
	// Every station column given a value, so a write that zeroes one shows.
	if err := testDB.UpdateOperatorStation(stationID, domain.StationInput{
		ProcessID: pid, Code: "WP-CODE", Name: "WP Screen", Note: "the original note",
		AreaLabel: "Cell 4", Sequence: 3, ControllerNodeID: "CTRL-9",
		DeviceMode: "roaming_tablet", Enabled: true,
	}); err != nil {
		t.Fatalf("seed station columns: %v", err)
	}

	proc, err := testDB.GetProcess(pid)
	if err != nil {
		t.Fatalf("read process: %v", err)
	}
	style, err := testDB.GetStyle(sid)
	if err != nil {
		t.Fatalf("read style: %v", err)
	}
	station, err := testDB.GetOperatorStation(stationID)
	if err != nil {
		t.Fatalf("read station: %v", err)
	}

	// A backfilled routing row for the adopt case.
	rowID, err := testDB.UpsertRoutingNode(domain.RoutingNodeInput{
		ProcessID: pid, CoreNodeName: "WP-SOURCE", Role: domain.RoutingRoleSource,
		Origin: domain.RoutingOriginBackfill, Enabled: false,
	})
	if err != nil {
		t.Fatalf("seed backfill row: %v", err)
	}

	bodies := buildDesktopBodies(t, map[string]any{
		"processID": pid,
		"process": map[string]any{
			"id": pid, "name": proc.Name, "production_state": proc.ProductionState,
		},
		// The D5 draft: what the settings screen holds. It edits the name and
		// nothing else, which is the case that broke.
		"draft": map[string]any{
			"name": "WP-Press renamed", "description": proc.Description,
			"counter_plc_name": proc.CounterPLCName, "counter_tag_name": proc.CounterTagName,
			"counter_enabled": proc.CounterEnabled, "changeover_auto_arm": proc.ChangeoverAutoArm,
			"group_id": groupID,
		},
		"style": map[string]any{
			"id": sid, "name": style.Name, "description": style.Description,
			"process_id": style.ProcessID, "expected_catid": style.ExpectedCATID,
		},
		"station": map[string]any{
			"id": stationID, "process_id": pid, "code": station.Code, "name": station.Name,
			"note": station.Note, "area_label": station.AreaLabel, "sequence": station.Sequence,
			"controller_node_id": station.ControllerNodeID, "device_mode": station.DeviceMode,
			"enabled": station.Enabled,
		},
		// U10: the shape key an offered candidate carries, and the cells an
		// apply would save. Both are opaque to the builders — this test is
		// about the FIELDS the body names, and the apply's end-to-end write is
		// pinned below against the real preview's own fingerprint.
		"shapeKey": "SHAPE-KEY",
		"cells":    []any{},
		// U10: the Add-process sheet's own two inputs — the group it picked,
		// and the positions its operator screen claims. The blank and the
		// duplicate are deliberate: the builder is what keeps the body saying
		// exactly what the engineer picked.
		"groupID":      groupID,
		"claimedNodes": []any{"WP-POS-1", " WP-POS-2 ", "WP-POS-1", ""},
	})

	// ── Add process ──────────────────────────────────────────────────────────
	//
	// The first write of the chain the page lost in the flow-composer wave.
	// The counter and the auto-arm are not on the sheet, and this is what
	// "left to Settings" has to MEAN in the row: empty, off, and 'auto' —
	// not whatever the last INSERT happened to leave.
	t.Run("a created process arrives grouped, in production, with no counter", func(t *testing.T) {
		resp := doRequest(t, router, "POST", "/api/processes", bodies["process create"], cookie)
		assertStatus(t, resp, http.StatusOK)
		rows, err := testDB.ListProcesses()
		if err != nil {
			t.Fatalf("list processes: %v", err)
		}
		var made *processes.Process
		for i := range rows {
			if rows[i].Name == "Made here" {
				made = &rows[i]
			}
		}
		if made == nil {
			t.Fatalf("no process named %q after the create: %+v", "Made here", rows)
		}
		if made.ProductionState != "active_production" {
			t.Errorf("production_state = %q, want active_production", made.ProductionState)
		}
		if made.GroupID == nil || *made.GroupID != groupID {
			t.Errorf("group_id = %v, want %d — the sheet's group did not land", made.GroupID, groupID)
		}
		if made.CounterPLCName != "" || made.CounterTagName != "" || made.CounterEnabled {
			t.Errorf("a new process arrived wired to a counter the sheet never asked about: %+v", made)
		}
		if made.ChangeoverAutoArm != "auto" {
			t.Errorf("changeover_auto_arm = %q, want auto", made.ChangeoverAutoArm)
		}
		if made.FlowComposerEnabled {
			t.Error("a new process arrived with the flow-composer gate open — it opens on a reviewed routing set, not on a create")
		}
	})

	// ── the positions a screen claims ────────────────────────────────────────
	//
	// StationService.SetNodes is what mints process_nodes rows, and NOTHING on
	// the desktop called it: a process created any other way had no positions
	// and no way to get them. The body is the whole list because the endpoint
	// is a set-to, and the blank and the duplicate in the fixture are what
	// that list looks like coming off a picker.
	t.Run("claimed nodes mint the process's positions, trimmed and deduplicated", func(t *testing.T) {
		resp := doRequest(t, router, "PUT", "/api/operator-stations/"+itoa(stationID)+"/claimed-nodes",
			bodies["claimed nodes"], cookie)
		assertStatus(t, resp, http.StatusOK)
		nodes, err := testDB.ListProcessNodesByStation(stationID)
		if err != nil {
			t.Fatalf("list process nodes: %v", err)
		}
		got := make([]string, 0, len(nodes))
		for _, n := range nodes {
			got = append(got, n.CoreNodeName)
		}
		sort.Strings(got)
		want := []string{"WP-POS-1", "WP-POS-2"}
		if !slices.Equal(got, want) {
			t.Errorf("positions = %v, want %v — a blank or a repeat reached the store", got, want)
		}
	})

	// ── settings save ────────────────────────────────────────────────────────
	t.Run("settings save keeps production_state and the group", func(t *testing.T) {
		resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), bodies["settings save"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetProcess(pid)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.ProductionState != "active_production" {
			t.Errorf("production_state = %q, want active_production", got.ProductionState)
		}
		if got.GroupID == nil || *got.GroupID != groupID {
			t.Errorf("group_id = %v, want %d — a nil group_id is the handler's Ungrouped", got.GroupID, groupID)
		}
		if got.Name != "WP-Press renamed" {
			t.Errorf("name = %q, want the edit to have landed", got.Name)
		}
		if got.Description != proc.Description || got.CounterPLCName != proc.CounterPLCName ||
			got.CounterTagName != proc.CounterTagName || got.CounterEnabled != proc.CounterEnabled {
			t.Errorf("a column nobody edited moved: %+v", got)
		}
	})

	// THE DIRECTION OF THE production_state BUG IS THE OTHER WAY ROUND, and
	// this is the case that shows it.
	//
	// 82f05880's message says a body without production_state "blanked" it.
	// It cannot: store/processes.Update coerces an empty productionState to
	// "active_production" before the UPDATE (processes.go:102). So the missing
	// field did not clear the column — it FORCED it to active_production,
	// whatever it held. The state that gets destroyed is therefore
	// 'changeover_active', the value changeover_service sets for the duration
	// of a changeover: an engineer saving an unrelated setting on D5 while the
	// floor was mid-changeover silently told the rest of the system the
	// changeover was over. A process already active_production saw nothing,
	// which is why a test seeded at the default would have missed it.
	t.Run("a settings save mid-changeover does not end the changeover", func(t *testing.T) {
		if err := testDB.SetProcessProductionState(pid, "changeover_active"); err != nil {
			t.Fatalf("seed changeover_active: %v", err)
		}
		t.Cleanup(func() { _ = testDB.SetProcessProductionState(pid, "active_production") })
		mid, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		body := buildDesktopBodies(t, map[string]any{
			"processID": pid,
			"process": map[string]any{
				"id": pid, "name": mid.Name, "production_state": mid.ProductionState,
			},
			"draft": map[string]any{
				"name": mid.Name, "description": mid.Description,
				"counter_plc_name": mid.CounterPLCName, "counter_tag_name": mid.CounterTagName,
				"counter_enabled": mid.CounterEnabled, "changeover_auto_arm": mid.ChangeoverAutoArm,
				"group_id": groupID,
			},
			"style":   map[string]any{"id": sid},
			"station": map[string]any{"id": stationID},
		})["settings save"]
		resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), body, cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		if got.ProductionState != "changeover_active" {
			t.Errorf("production_state = %q, want changeover_active — saving a setting ended a live changeover", got.ProductionState)
		}
	})

	// ── the gate ─────────────────────────────────────────────────────────────
	t.Run("the gate flips on its own and touches nothing else", func(t *testing.T) {
		before, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		resp := doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid), bodies["gate"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		if !got.FlowComposerEnabled {
			t.Error("flow_composer_enabled did not read back")
		}
		if got.Name != before.Name || got.ProductionState != before.ProductionState ||
			got.Description != before.Description {
			t.Errorf("the gate PATCH moved another column: %+v", got)
		}
	})

	// ── rename, Expected CATID ───────────────────────────────────────────────
	t.Run("rename keeps the description and the process", func(t *testing.T) {
		resp := doRequest(t, router, "PUT", "/api/styles/"+itoa(sid), bodies["rename"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetStyle(sid)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.Name != "Renamed here" {
			t.Errorf("name = %q, want the rename to have landed", got.Name)
		}
		if got.Description != style.Description {
			t.Errorf("description = %q, want %q — a rename blanked it", got.Description, style.Description)
		}
		if got.ProcessID != pid {
			t.Errorf("process_id = %d, want %d — a zero process_id is a 400 from this handler", got.ProcessID, pid)
		}
		if got.ExpectedCATID != "CAT-OLD" {
			t.Errorf("expected_catid = %q, want CAT-OLD — a rename moved the PLC's match", got.ExpectedCATID)
		}
	})

	t.Run("expected CATID keeps the name and the description", func(t *testing.T) {
		resp := doRequest(t, router, "PUT", "/api/styles/"+itoa(sid), bodies["expected catid"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetStyle(sid)
		testutil.MustNoErr(t, err, "testDB.GetStyle")
		if got.ExpectedCATID != "CAT-NEW" {
			t.Errorf("expected_catid = %q, want CAT-NEW", got.ExpectedCATID)
		}
		if got.Description != style.Description || got.ProcessID != pid {
			t.Errorf("a column nobody edited moved: %+v", got)
		}
	})

	// ── clone, mark as running ───────────────────────────────────────────────
	t.Run("clone takes the name and copies the rest", func(t *testing.T) {
		resp := doRequest(t, router, "POST", "/api/styles/"+itoa(sid)+"/clone", bodies["clone"], cookie)
		assertStatus(t, resp, http.StatusOK)
		styles, err := testDB.ListStylesByProcess(pid)
		if err != nil {
			t.Fatalf("list styles: %v", err)
		}
		var clone *processes.Style
		for i := range styles {
			if styles[i].Name == "Cloned here" {
				clone = &styles[i]
			}
		}
		if clone == nil {
			t.Fatalf("no style named %q after the clone: %+v", "Cloned here", styles)
		}
		if clone.ProcessID != pid {
			t.Errorf("clone landed on process %d, want %d", clone.ProcessID, pid)
		}
	})

	t.Run("mark as running sets the active style and nothing else", func(t *testing.T) {
		before, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/active-style", bodies["mark as running"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetProcess(pid)
		testutil.MustNoErr(t, err, "testDB.GetProcess")
		if got.ActiveStyleID == nil || *got.ActiveStyleID != sid {
			t.Errorf("active_style_id = %v, want %d", got.ActiveStyleID, sid)
		}
		if got.Name != before.Name || got.ProductionState != before.ProductionState {
			t.Errorf("marking a style running moved another column: %+v", got)
		}
	})

	// ── the operator screens ─────────────────────────────────────────────────
	t.Run("a screen edit keeps the five columns the sheet does not show", func(t *testing.T) {
		resp := doRequest(t, router, "PUT", "/api/operator-stations/"+itoa(stationID), bodies["operator-screen edit"], cookie)
		assertStatus(t, resp, http.StatusOK)
		got, err := testDB.GetOperatorStation(stationID)
		if err != nil {
			t.Fatalf("read back: %v", err)
		}
		if got.Name != "Renamed screen" || got.Note != "a note" {
			t.Errorf("the edit did not land: name %q note %q", got.Name, got.Note)
		}
		if got.Code != station.Code {
			t.Errorf("code = %q, want %q — an edit blanked the screen's code", got.Code, station.Code)
		}
		if got.AreaLabel != station.AreaLabel {
			t.Errorf("area_label = %q, want %q", got.AreaLabel, station.AreaLabel)
		}
		if got.Sequence != station.Sequence {
			t.Errorf("sequence = %d, want %d", got.Sequence, station.Sequence)
		}
		if got.ControllerNodeID != station.ControllerNodeID {
			t.Errorf("controller_node_id = %q, want %q", got.ControllerNodeID, station.ControllerNodeID)
		}
		if got.DeviceMode != station.DeviceMode {
			t.Errorf("device_mode = %q, want %q — an edit reset it", got.DeviceMode, station.DeviceMode)
		}
		if !got.Enabled {
			t.Error("enabled = false — fixing a typo in a note switched a live HMI off")
		}
	})

	t.Run("a new screen arrives enabled, on this process", func(t *testing.T) {
		resp := doRequest(t, router, "POST", "/api/operator-stations", bodies["operator-screen add"], cookie)
		assertStatus(t, resp, http.StatusOK)
		rows, err := testDB.ListOperatorStationsByProcess(pid)
		if err != nil {
			t.Fatalf("list stations: %v", err)
		}
		var added *domain.Station
		for i := range rows {
			if rows[i].Name == "Added screen" {
				added = &rows[i]
			}
		}
		if added == nil {
			t.Fatalf("no screen named %q: %+v", "Added screen", rows)
		}
		if !added.Enabled {
			t.Error("a new screen arrived disabled")
		}
		if added.DeviceMode != "fixed_hmi" {
			t.Errorf("device_mode = %q, want fixed_hmi", added.DeviceMode)
		}
	})

	// ── the routing set ──────────────────────────────────────────────────────
	t.Run("adopting a backfill enables it and records the engineer", func(t *testing.T) {
		resp := doRequest(t, router, "PATCH", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(rowID), bodies["adopt"], cookie)
		assertStatus(t, resp, http.StatusOK)
		rows, err := testDB.ListRoutingNodes(pid)
		if err != nil {
			t.Fatalf("list routing: %v", err)
		}
		for _, r := range rows {
			if r.ID != rowID {
				continue
			}
			if !r.Enabled {
				t.Error("enabled did not read back")
			}
			if r.Origin != domain.RoutingOriginEngineer {
				t.Errorf("origin = %q, want engineer — the switch is the approval", r.Origin)
			}
			if r.CalledBy == "" {
				t.Error("called_by is empty; the server did not stamp the session user")
			}
		}
	})

	t.Run("add-to-set stamps the origin server-side", func(t *testing.T) {
		resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/routing-nodes", bodies["add-to-set"], cookie)
		assertStatus(t, resp, http.StatusOK)
		rows, err := testDB.ListRoutingNodes(pid)
		testutil.MustNoErr(t, err, "testDB.ListRoutingNodes")
		var found *domain.RoutingNode
		for i := range rows {
			if rows[i].CoreNodeName == "Supermarket Area" && rows[i].Role == domain.RoutingRoleDestination {
				found = &rows[i]
			}
		}
		if found == nil {
			t.Fatalf("the row was not added: %+v", rows)
		}
		if found.Origin != domain.RoutingOriginEngineer {
			t.Errorf("origin = %q, want engineer", found.Origin)
		}
		if found.CalledBy == "" {
			t.Error("called_by is empty; the server did not stamp the session user")
		}
		if !found.Enabled {
			t.Error("a name an engineer typed arrived switched off")
		}
	})

	// ── the deletes ──────────────────────────────────────────────────────────
	// Neither takes a body, so there is no builder and nothing to drift. What
	// is worth pinning is that the URL the page uses is the one that works.
	t.Run("the two deletes take no body", func(t *testing.T) {
		rows, err := testDB.ListRoutingNodes(pid)
		testutil.MustNoErr(t, err, "testDB.ListRoutingNodes")
		var target int64
		for _, r := range rows {
			if r.CoreNodeName == "Supermarket Area" {
				target = r.ID
			}
		}
		resp := doRequest(t, router, "DELETE", "/api/processes/"+itoa(pid)+"/routing-nodes/"+itoa(target), nil, cookie)
		assertStatus(t, resp, http.StatusOK)

		doomed := seedStyle(t, "WP-Doomed", pid)
		resp = doRequest(t, router, "DELETE", "/api/styles/"+itoa(doomed), nil, cookie)
		assertStatus(t, resp, http.StatusOK)
	})

	// ── U10 · naming a shape, and applying one ───────────────────────────────
	//
	// THE BODIES FIRST, because the two create bodies are the ones a field
	// could go missing from. created_by is the session's and must NOT be in
	// either: a page that named an author would be a page deciding authorship,
	// which is the rule the routing origin above is held to.
	t.Run("neither create body names an author, and each names one source", func(t *testing.T) {
		fromFlow := bodies["name a flow"]
		fromShape := bodies["name a found shape"]
		for label, b := range map[string]map[string]any{"name a flow": fromFlow, "name a found shape": fromShape} {
			for _, banned := range []string{"created_by", "created_at", "version", "id"} {
				if _, ok := b[banned]; ok {
					t.Errorf("%s names %q — the server owns that column", label, banned)
				}
			}
			if b["name"] != "Two up, index" {
				t.Errorf("%s: name = %v", label, b["name"])
			}
		}
		// EXACTLY ONE SOURCE. The handler refuses both or neither, so the
		// builder has to pick, and which one it picked is the difference
		// between naming a flow and naming an offered shape.
		if _, ok := fromFlow["from_style_id"]; !ok {
			t.Error("naming a flow did not name from_style_id")
		}
		if _, ok := fromFlow["from_candidate_shape"]; ok {
			t.Error("naming a flow also named a candidate shape; the handler refuses both")
		}
		if fromShape["from_candidate_shape"] != "SHAPE-KEY" {
			t.Errorf("naming a found shape: from_candidate_shape = %v", fromShape["from_candidate_shape"])
		}
		if _, ok := fromShape["from_style_id"]; ok {
			t.Error("naming a found shape also named a style; the handler refuses both")
		}
	})

	// BOTH PROVENANCE FIELDS OR NEITHER. A row saying "preset 42, version
	// nothing" is provenance nobody can read, and the builder cannot half-send
	// it because both come off one object.
	t.Run("the apply body carries both provenance columns, and a plain save carries neither", func(t *testing.T) {
		applied := bodies["apply a preset"]
		if applied["source_preset_id"] != float64(7) || applied["source_preset_version"] != float64(3) {
			t.Errorf("apply body provenance = %v / %v, want 7 / 3",
				applied["source_preset_id"], applied["source_preset_version"])
		}
		// Every column D1's own save names has to be named here too: this body
		// goes through the SAME handler, and a field left out of a flow save
		// is a field set to its zero value.
		for _, want := range []string{"to_style_id", "cells", "fingerprint", "station_id"} {
			if _, ok := applied[want]; !ok {
				t.Errorf("the apply body does not name %q — flow/save writes it", want)
			}
		}
		plain := bodies["save with no preset"]
		for _, banned := range []string{"source_preset_id", "source_preset_version"} {
			if _, ok := plain[banned]; ok {
				t.Errorf("a save with no preset named %q — a hand edit must leave the columns alone", banned)
			}
		}
	})

	// AN APPLY WITH HALF ITS PROVENANCE IS REFUSED, not half-written.
	t.Run("an id without a version is refused", func(t *testing.T) {
		resp := doRequest(t, router, "POST", "/api/processes/"+itoa(pid)+"/flow/save",
			map[string]any{
				"to_style_id": sid, "cells": []any{}, "fingerprint": "x",
				"station_id": stationID, "source_preset_id": 7,
			}, cookie)
		assertStatus(t, resp, http.StatusBadRequest)
		if why, _ := decodeBody(t, resp)["error"].(string); !strings.Contains(why, "go together") {
			t.Errorf("the refusal reads %q; it should say the two columns go together", why)
		}
	})
}
