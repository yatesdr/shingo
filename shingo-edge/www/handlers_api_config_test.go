package www

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
)

// ═══════════════════════════════════════════════════════════════════════
// Processes — List, Create, Update, Delete, SetActiveStyle, ListStylesByProcess
// DB call sites: ListProcesses, CreateProcess, UpdateProcess, DeleteProcess,
//                SetActiveStyle, ListStylesByProcess
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ListProcesses(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	// Seed two processes
	seedProcess(t, db, "Assembly")
	seedProcess(t, db, "Paint")

	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var procs []processes.Process
	decodeJSON(t, resp, &procs)
	if len(procs) < 2 {
		t.Fatalf("expected at least 2 processes, got %d", len(procs))
	}
	names := make(map[string]bool)
	for _, p := range procs {
		names[p.Name] = true
	}
	if !names["Assembly"] || !names["Paint"] {
		t.Errorf("expected Assembly and Paint in response, got %v", names)
	}
}

// TestApiConfig_ListProcesses_CoreBoardsIsEveryPositionALoaderWindow pins
// core_boards on the list's row: true only for a process with a position and
// every position (enabled or not, on a screen or not) a Core loader's window,
// so the page can draw a loader's or unloader's process as Core's to set up.
// One loader window among press positions is a press; no position at all is
// not a loader's either. The key is always on the row, false included.
func TestApiConfig_ListProcesses_CoreBoardsIsEveryPositionALoaderWindow(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)
	loader, err := domain.NewSharedWindowLoader(domain.LoaderID("LD-BOARDS"), domain.RoleConsume,
		domain.ReplenishmentOperator, []domain.Window{{Node: "LDW_001"}, {Node: "LDW_002"}}, nil)
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	h.engine.(*stubEngine).loaders = oneLoader{l: loader}

	loaderLine := seedProcess(t, db, "CoreBoards Loader")
	ldrScreen := seedOperatorStation(t, db, loaderLine, "CB-LDR", "CoreBoards Loader Screen")
	seedProcessNode(t, db, loaderLine, ldrScreen, "LDW_001")
	off := seedProcessNode(t, db, loaderLine, 0, "LDW_002")
	if _, err := db.Exec(`UPDATE process_nodes SET enabled = 0 WHERE id = ?`, off); err != nil {
		t.Fatalf("disable LDW_002: %v", err)
	}
	press := seedProcess(t, db, "CoreBoards Press")
	seedProcessNode(t, db, press, 0, "LDW_001")
	seedProcessNode(t, db, press, 0, "CB-PRESS-POS")
	bare := seedProcess(t, db, "CoreBoards Bare")

	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var rows []map[string]any
	decodeJSON(t, resp, &rows)
	got := map[int64]any{}
	for _, r := range rows {
		id, _ := r["id"].(float64)
		v, has := r["core_boards"]
		if !has {
			t.Errorf("process %v's row carries no core_boards; the page reads false from the key, not its absence", r["name"])
		}
		got[int64(id)] = v
	}
	for pid, want := range map[int64]bool{loaderLine: true, press: false, bare: false} {
		if got[pid] != want {
			t.Errorf("process %d core_boards = %v, want %v", pid, got[pid], want)
		}
	}
}

// TestApiConfig_ListProcesses_CoreAddedAndRemoved pins what a loader's
// process's row says Core changed that its screens do not show: core_added,
// each window of its loaders that is no position here, in the loader's order;
// core_removed, each position a loader held (loader_window) and none holds
// now, which keeps the process Core's. A line's row carries neither key, nor
// does a loader's process with nothing to say.
func TestApiConfig_ListProcesses_CoreAddedAndRemoved(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)
	loader, err := domain.NewSharedWindowLoader(domain.LoaderID("LD-CHANGES"), domain.RoleProduce,
		domain.ReplenishmentOperator, []domain.Window{{Node: "LCW_001"}, {Node: "LCW_003"}, {Node: "LCW_002"}}, []domain.PayloadCode{"PART-LC"})
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	h.engine.(*stubEngine).loaders = oneLoader{l: loader}

	changed := seedProcess(t, db, "Changes Loader")
	screen := seedOperatorStation(t, db, changed, "CH-LDR", "Changes Loader Screen")
	seedProcessNode(t, db, changed, screen, "LCW_001")
	gone := seedProcessNode(t, db, changed, screen, "LCW_GONE")
	if _, err := db.Exec(`UPDATE process_nodes SET loader_window = 1 WHERE id = ?`, gone); err != nil {
		t.Fatalf("mark LCW_GONE: %v", err)
	}
	press := seedProcess(t, db, "Changes Press")
	seedProcessNode(t, db, press, 0, "LCW_002")
	seedProcessNode(t, db, press, 0, "CH-PRESS-POS")

	resp := doRequest(t, router, "GET", "/api/processes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var rows []map[string]any
	decodeJSON(t, resp, &rows)
	byID := map[int64]map[string]any{}
	for _, r := range rows {
		id, _ := r["id"].(float64)
		byID[int64(id)] = r
	}
	got := byID[changed]
	if got["core_boards"] != true {
		t.Fatalf("the loader's process core_boards = %v, want true: a removed window keeps it Core's", got["core_boards"])
	}
	// LCW_002 is the press's position, so it is not added here.
	if want := []any{"LCW_003"}; !reflect.DeepEqual(got["core_added"], want) {
		t.Errorf("core_added = %v, want %v", got["core_added"], want)
	}
	if want := []any{"LCW_GONE"}; !reflect.DeepEqual(got["core_removed"], want) {
		t.Errorf("core_removed = %v, want %v", got["core_removed"], want)
	}
	for _, key := range []string{"core_added", "core_removed"} {
		if _, has := byID[press][key]; has {
			t.Errorf("the press's row carries %s", key)
		}
	}
}

func TestApiConfig_CreateProcess(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	body := map[string]any{
		"name":        "TestLine",
		"description": "A test line",
	}
	resp := doRequest(t, router, "POST", "/api/processes", body, cookie)
	assertStatus(t, resp, http.StatusOK)

	var result map[string]int64
	decodeJSON(t, resp, &result)
	if result["id"] == 0 {
		t.Fatal("expected non-zero process id")
	}

	// Verify DB state
	p, err := db.GetProcess(result["id"])
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if p.Name != "TestLine" {
		t.Errorf("process name: got %q, want %q", p.Name, "TestLine")
	}
}

func TestApiConfig_CreateProcess_MissingName(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"description": "no name"}
	resp := doRequest(t, router, "POST", "/api/processes", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "name is required")
}

// A TAKEN NAME IS THE PERSON'S TO FIX, not a fault: 409, said in plain words,
// and as the name field's error so the page can draw it there. Not SQLite's
// "UNIQUE constraint failed: processes.name (2067)".
func TestApiConfig_CreateProcess_NameTaken(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)
	seedProcess(t, db, "Press 6")

	resp := doRequest(t, router, "POST", "/api/processes", map[string]any{"name": "Press 6"}, cookie)
	assertStatus(t, resp, http.StatusConflict)
	var got struct {
		Error  string            `json:"error"`
		Errors map[string]string `json:"errors"`
	}
	decodeJSON(t, resp, &got)
	if want := `another process is already called "Press 6"`; got.Error != want || got.Errors["name"] != want {
		t.Errorf("a taken name answered %+v", got)
	}
	list, err := db.ListProcesses()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Errorf("a refused create left %d processes, want 1", len(list))
	}
}

// A rename to a taken name writes NOTHING: the counter and identity edit runs
// before the name's, so a refusal found only at the name's write left the
// identity switched and the name as it was.
func TestApiConfig_UpdateProcess_NameTakenWritesNothing(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)
	seedProcess(t, db, "Press 6")
	pid := seedProcess(t, db, "Press 7")

	body := map[string]any{"name": "Press 6", "description": "renamed", "identity_none": true}
	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), body, cookie)
	assertStatus(t, resp, http.StatusConflict)
	var got struct {
		Error  string            `json:"error"`
		Errors map[string]string `json:"errors"`
	}
	decodeJSON(t, resp, &got)
	if want := `another process is already called "Press 6"`; got.Error != want || got.Errors["name"] != want {
		t.Errorf("a taken name answered %+v", got)
	}
	p, err := db.GetProcess(pid)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "Press 7" || p.Description != "test process" || p.IdentityNone {
		t.Errorf("a refused rename wrote: name %q, description %q, identity_none %v", p.Name, p.Description, p.IdentityNone)
	}

	// Its own name is not taken from it: a save that keeps the name saves.
	resp = doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), map[string]any{"name": "Press 7", "description": "kept"}, cookie)
	assertStatus(t, resp, http.StatusOK)
}

func TestApiConfig_UpdateProcess(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "ToUpdate")

	body := map[string]any{
		"name":        "Updated",
		"description": "updated desc",
	}
	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid), body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	p, err := db.GetProcess(pid)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if p.Name != "Updated" {
		t.Errorf("process name: got %q, want %q", p.Name, "Updated")
	}
}

func TestApiConfig_DeleteProcess(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "ToDelete")

	resp := doRequest(t, router, "DELETE", "/api/processes/"+itoa(pid), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	_, err := db.GetProcess(pid)
	if err == nil {
		t.Error("expected error getting deleted process")
	}
}

func TestApiConfig_SetActiveStyle(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "ActiveStyleLine")
	sid := seedStyle(t, db, "ActiveStyle", pid)

	body := map[string]any{"style_id": sid}
	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/active-style", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB state
	activeID, err := db.GetActiveStyleID(pid)
	if err != nil {
		t.Fatalf("GetActiveStyleID: %v", err)
	}
	if activeID == nil || *activeID != sid {
		t.Errorf("active_style_id: got %v, want %d", activeID, sid)
	}
}

// The admin style flip is a cutover: the pile at the process's node is
// stranded with its qty.
// Flipped under change #2: the flip used to leave the pile active, still
// stamped with the outgoing style.
func TestApiConfig_SetActiveStyleStrandsThePiles(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "PileFlipLine")
	fromID := seedStyle(t, db, "PileFlipFrom", pid)
	toID := seedStyle(t, db, "PileFlipTo", pid)
	if err := db.SetActiveStyle(pid, &fromID); err != nil {
		t.Fatalf("SetActiveStyle: %v", err)
	}
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: pid, CoreNodeName: "PILE-FLIP-SEAT", Code: "PF1",
		Name: "PILE-FLIP-SEAT", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	if _, err := db.CaptureLinesideBucket(nodeID, "SYN-PART-1", 9); err != nil {
		t.Fatalf("CaptureLinesideBucket: %v", err)
	}

	resp := doRequest(t, router, "PUT", "/api/processes/"+itoa(pid)+"/active-style", map[string]any{"style_id": toID}, cookie)
	assertStatus(t, resp, http.StatusOK)

	rows, err := db.ListLinesideBuckets(nodeID)
	if err != nil {
		t.Fatalf("ListLinesideBuckets: %v", err)
	}
	if len(rows) != 1 || rows[0].State != "stranded" || rows[0].Qty != 9 {
		t.Errorf("piles after the flip = %+v, want one stranded pile of 9", rows)
	}
}

func TestApiConfig_ListProcessStyles(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "StyleListLine")
	seedStyle(t, db, "S1", pid)
	seedStyle(t, db, "S2", pid)

	resp := doRequest(t, router, "GET", "/api/processes/"+itoa(pid)+"/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var styles []processes.Style
	decodeJSON(t, resp, &styles)
	if len(styles) != 2 {
		t.Fatalf("expected 2 styles, got %d", len(styles))
	}
	names := make(map[string]bool)
	for _, s := range styles {
		names[s.Name] = true
	}
	if !names["S1"] || !names["S2"] {
		t.Errorf("expected S1 and S2, got %v", names)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Styles — List, Create, Update, Delete
// DB call sites: ListStyles, CreateStyle, UpdateStyle, DeleteStyle
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_StylesCRUD(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)
	pid := seedProcess(t, db, "StyleCRUDLine")

	// --- List (empty) ---
	resp := doRequest(t, router, "GET", "/api/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var styles []processes.Style
	decodeJSON(t, resp, &styles)

	// --- Create ---
	body := map[string]any{
		"name":        "Red Widget",
		"description": "A red one",
		"process_id":  pid,
	}
	resp = doRequest(t, router, "POST", "/api/styles", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	var createResult map[string]int64
	decodeJSON(t, resp, &createResult)
	styleID := createResult["id"]
	if styleID == 0 {
		t.Fatal("expected non-zero style id")
	}

	// Verify DB
	s, err := db.GetStyle(styleID)
	if err != nil {
		t.Fatalf("GetStyle: %v", err)
	}
	if s.Name != "Red Widget" {
		t.Errorf("style name: got %q, want %q", s.Name, "Red Widget")
	}
	if s.ProcessID != pid {
		t.Errorf("style process_id: got %d, want %d", s.ProcessID, pid)
	}

	// --- List (has one) ---
	resp = doRequest(t, router, "GET", "/api/styles", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	decodeJSON(t, resp, &styles)
	found := false
	for _, s := range styles {
		if s.ID == styleID {
			found = true
		}
	}
	if !found {
		t.Error("created style not found in list")
	}

	// --- Update ---
	updateBody := map[string]any{
		"name":        "Blue Widget",
		"description": "Updated",
		"process_id":  pid,
	}
	resp = doRequest(t, router, "PUT", "/api/styles/"+itoa(styleID), updateBody, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	s, _ = db.GetStyle(styleID)
	if s.Name != "Blue Widget" {
		t.Errorf("updated style name: got %q, want %q", s.Name, "Blue Widget")
	}

	// --- Delete ---
	resp = doRequest(t, router, "DELETE", "/api/styles/"+itoa(styleID), nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Delete is a RETIREMENT, not a removal, and the two halves of that are
	// both worth asserting because getting either wrong is silent.
	//
	// The row must SURVIVE: GetStyle is how a changeover row, an hourly count
	// and a reporting point resolve a name they already hold an id for, and a
	// hard delete is what made those render blank.
	s, err = db.GetStyle(styleID)
	if err != nil {
		t.Fatalf("retired style must still resolve by id (display joins depend on it): %v", err)
	}
	if s.DeletedAt == nil {
		t.Error("retired style has no deleted_at")
	}

	// And it must LEAVE the picker.
	live, err := db.ListStyles()
	if err != nil {
		t.Fatalf("ListStyles: %v", err)
	}
	for _, ls := range live {
		if ls.ID == styleID {
			t.Errorf("retired style %d is still offered by ListStyles", styleID)
		}
	}

	// The name it held is free again. Under the old table-level
	// UNIQUE(process_id, name) this failed, which is the regression soft delete
	// would have shipped silently.
	resp = doRequest(t, router, "POST", "/api/styles", map[string]any{
		"name": "Blue Widget", "description": "reborn", "process_id": pid,
	}, cookie)
	assertStatus(t, resp, http.StatusOK)
}

func TestApiConfig_CreateStyle_MissingProcessID(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"name": "Orphan"}
	resp := doRequest(t, router, "POST", "/api/styles", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "process_id is required")
}

func TestApiConfig_UpdateStyle_MissingProcessID(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	pid := seedProcess(t, db, "StyleUpdateLine")
	sid := seedStyle(t, db, "NeedsPID", pid)

	body := map[string]any{"name": "X"}
	resp := doRequest(t, router, "PUT", "/api/styles/"+itoa(sid), body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "process_id is required")
}

// ═══════════════════════════════════════════════════════════════════════
// Style Node Claims — List, Upsert, Delete
// DB call sites: ListStyleNodeClaims, UpsertStyleNodeClaim,
//                DeleteStyleNodeClaim
// ═══════════════════════════════════════════════════════════════════════

// ═══════════════════════════════════════════════════════════════════════
// Payload Catalog — List
// DB call site: ListPayloadCatalog
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ListPayloadCatalog(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	// Seed catalog entries
	db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "Bin S", Code: "BIN-S", Description: "Small bin", UOPCapacity: 24,
	})
	db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 2, Name: "Bin L", Code: "BIN-L", Description: "Large bin", UOPCapacity: 48,
	})

	resp := doRequest(t, router, "GET", "/api/payload-catalog", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var entries []*catalog.CatalogEntry
	decodeJSON(t, resp, &entries)
	if len(entries) < 2 {
		t.Fatalf("expected at least 2 catalog entries, got %d", len(entries))
	}
	codes := make(map[string]bool)
	for _, e := range entries {
		codes[e.Code] = true
	}
	if !codes["BIN-S"] || !codes["BIN-L"] {
		t.Errorf("expected BIN-S and BIN-L in catalog, got %v", codes)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// PLC / WarLink — ListPLCs, WarLinkStatus, SyncCoreNodes, SyncPayloadCatalog
// apiTestKafka (validation)
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_SyncCoreNodes(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/core-nodes/sync", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_SyncPayloadCatalog(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "POST", "/api/payload-catalog/sync", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

// TestApiConfig_SyncBeforeTheSyncsAreWired: until main.go wires the
// heartbeater's two syncs (cmd/shingoedge setupKafkaSubscribers, once messaging
// connects; never, on an Edge with no broker configured), both Sync doors
// refuse 503 with the Edge's sentence for a hook not wired and ask nothing.
// Once wired, each answers ok and asks once. They used to answer ok and send
// nothing (docket D31). A real engine, never started: the stub above always
// answers as wired.
func TestApiConfig_SyncBeforeTheSyncsAreWired(t *testing.T) {
	t.Parallel()
	cfg := config.Defaults()
	cfg.StationUID = "sync.edge"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: testdb.Open(t), LogFunc: func(string, ...any) {}})
	h := &Handlers{engine: eng, orchestration: eng, sessions: newSessionStore(""), eventHub: NewEventHub(),
		stationViews: newStationViewGroup()}
	r := chi.NewRouter()
	r.Post("/api/core-nodes/sync", h.apiSyncCoreNodes)
	r.Post("/api/payload-catalog/sync", h.apiSyncPayloadCatalog)
	for _, door := range []struct {
		path, refused string
		wire          func(func() error)
	}{
		{"/api/core-nodes/sync", "the heartbeater is not running (messaging not connected yet): no node list request sent",
			eng.SetNodeSyncFunc},
		{"/api/payload-catalog/sync", "the heartbeater is not running (messaging not connected yet): no catalog request sent",
			eng.SetCatalogSyncFunc},
	} {
		t.Run(door.path, func(t *testing.T) {
			resp := doRequest(t, r, "POST", door.path, nil, nil)
			assertStatus(t, resp, http.StatusServiceUnavailable)
			assertJSONPath(t, resp, "error", door.refused)

			asked := 0
			door.wire(func() error { asked++; return nil })
			resp = doRequest(t, r, "POST", door.path, nil, nil)
			assertStatus(t, resp, http.StatusOK)
			assertJSONPath(t, resp, "status", "ok")
			if asked != 1 {
				t.Errorf("wired: the door asked the heartbeater %d times, want 1", asked)
			}
		})
	}
}

// TestProcessesPage_TheCatalogSyncSaysItWasRequested: the Processes page's
// Sync from Core says what the door's ok means, that the Edge asked Core for
// the catalog; the catalog arrives with Core's answer, later (docket R72). It
// said "Catalog synced from Core.", as if the catalog had come.
func TestProcessesPage_TheCatalogSyncSaysItWasRequested(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Join("static", "js", "pages", "processes-desktop.js"))
	if err != nil {
		t.Fatalf("read processes-desktop.js: %v", err)
	}
	s := strings.ReplaceAll(string(src), "\r\n", "\n")
	start := strings.Index(s, "\nasync function syncCatalog(")
	if start < 0 {
		t.Fatal("syncCatalog not found in processes-desktop.js: this pin lost its input; name the function that sends POST /api/payload-catalog/sync")
	}
	body := s[start+1:]
	if end := strings.Index(body, "\n}\n"); end >= 0 {
		body = body[:end]
	}
	// The ok sentence: the one notice syncCatalog sets that is not the reset.
	var said []string
	for _, m := range regexp.MustCompile(`S\.settingsNotice = '([^']*)'`).FindAllStringSubmatch(body, -1) {
		if m[1] != "" {
			said = append(said, m[1])
		}
	}
	if len(said) != 1 {
		t.Fatalf("syncCatalog sets %d non-empty S.settingsNotice sentences, want 1, its ok sentence: this pin lost its input; its body:\n%s",
			len(said), body)
	}
	if notice := strings.ToLower(said[0]); !strings.Contains(notice, "requested") || strings.Contains(notice, "synced") {
		t.Errorf("syncCatalog's ok sentence is %q: want it to say the sync was requested, not that the catalog was synced; "+
			"the door's ok means the Edge asked Core, and the catalog arrives with Core's answer", said[0])
	}
}

func TestApiConfig_TestKafka_EmptyBroker(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{"broker": ""}
	resp := doRequest(t, router, "POST", "/api/config/kafka/test", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "broker address required")
}

// ═══════════════════════════════════════════════════════════════════════
// Core Nodes — Get
// (no DB call, reads from engine.CoreNodes())
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_GetCoreNodes(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{
		"node-1": {Name: "node-1", NodeType: "storage"},
	}

	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var nodes []protocol.NodeInfo
	decodeJSON(t, resp, &nodes)
	if len(nodes) != 1 || nodes[0].Name != "node-1" {
		t.Errorf("expected 1 node 'node-1', got %v", nodes)
	}
}

// A group carries its members, so the positions picker can take a group as its
// nodes when it is picked; a plain node carries none.
func TestApiConfig_GetCoreNodes_GroupCarriesMembers(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	stub := h.engine.(*stubEngine)
	stub.core = map[string]protocol.NodeInfo{
		"UL · stage 1": {Name: "UL · stage 1", NodeType: "NGRP"},
		"FGN_001":      {Name: "FGN_001", NodeType: "storage"},
	}
	stub.groups = map[string][]string{"UL · stage 1": {"FGN_001", "FGN_002"}}

	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var rows []struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	decodeJSON(t, resp, &rows)
	got := map[string][]string{}
	for _, r := range rows {
		got[r.Name] = r.Members
	}
	if m := got["UL · stage 1"]; len(m) != 2 || m[0] != "FGN_001" || m[1] != "FGN_002" {
		t.Errorf("group members = %v, want [FGN_001 FGN_002]", m)
	}
	if m, ok := got["FGN_001"]; !ok || len(m) != 0 {
		t.Errorf("plain node: present %v members %v, want present with none", ok, m)
	}
}

// oneLoader answers LoaderForNode from one loader, everything else a clean miss.
type oneLoader struct{ l *domain.Loader }

func (o oneLoader) LoaderForNode(n domain.NodeID) (*domain.Loader, error) {
	if o.l.Contains(n) {
		return o.l, nil
	}
	return nil, nil
}

// A window of a Core-owned loader says so, because the positions checklist
// offers it even when another process holds it (SetNodes accepts a loader's
// node on several processes) and greys out every other held node.
func TestApiConfig_GetCoreNodes_LoaderWindowIsShared(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	loader, err := domain.NewSharedWindowLoader(domain.LoaderID("LD-SHARED"), domain.RoleConsume,
		domain.ReplenishmentOperator, []domain.Window{{Node: "SMN_001"}}, nil)
	if err != nil {
		t.Fatalf("NewSharedWindowLoader: %v", err)
	}
	stub := h.engine.(*stubEngine)
	stub.core = map[string]protocol.NodeInfo{
		"SMN_001": {Name: "SMN_001", NodeType: "storage"},
		"PLN_005": {Name: "PLN_005", NodeType: "storage"},
	}
	stub.loaders = oneLoader{l: loader}

	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)

	var rows []struct {
		Name         string `json:"name"`
		SharedLoader bool   `json:"shared_loader"`
	}
	decodeJSON(t, resp, &rows)
	got := map[string]bool{}
	for _, r := range rows {
		got[r.Name] = r.SharedLoader
	}
	if !got["SMN_001"] || got["PLN_005"] {
		t.Errorf("shared_loader = %v, want SMN_001 true and PLN_005 false", got)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Config endpoints — UpdateMessaging, UpdateStationID,
// UpdateAutoConfirm, UpdateWarLink
// Re-pointed by U3 at the one save door, PUT /api/config, each posting its
// old body as that door's section; the assertions are the same (lead ruling
// L5 keeps `status` and the top-level `error` on the new door).
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_UpdateMessaging(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"messaging": map[string]any{
		"brokers": []string{"broker1:9092", "broker2:9092"},
	}}
	resp := doRequest(t, router, "PUT", "/api/config", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateAutoConfirm(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	// Auto-confirm is in the Station section, which the page sends with no
	// Station UID (the uid is not set on this page).
	body := map[string]any{"station": map[string]any{"auto_confirm": true}}
	resp := doRequest(t, router, "PUT", "/api/config", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateWarLink(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"plc": map[string]any{
		"host":      "warlink.test",
		"port":      9090,
		"enabled":   true,
		"poll_rate": "2s",
		"mode":      "sse",
	}}
	resp := doRequest(t, router, "PUT", "/api/config", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")
}

func TestApiConfig_UpdateWarLink_InvalidMode(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"plc": map[string]any{"mode": "invalid"}}
	resp := doRequest(t, router, "PUT", "/api/config", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", `mode must be "poll" or "sse"`)
}

func TestApiConfig_UpdateWarLink_InvalidPollRate(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]any{"plc": map[string]any{"poll_rate": "not-a-duration"}}
	resp := doRequest(t, router, "PUT", "/api/config", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
}

// ═══════════════════════════════════════════════════════════════════════
// Change Password
// DB call sites: GetAdminUser, UpdateAdminPassword
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_ChangePassword(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	db := dbOf(h)
	cookie := authCookie(t, h)

	body := map[string]string{
		"old_password": "password",
		"new_password": "newpassword123",
	}
	resp := doRequest(t, router, "POST", "/api/config/password", body, cookie)
	assertStatus(t, resp, http.StatusOK)
	assertJSONPath(t, resp, "status", "ok")

	// Verify DB: hash should have changed
	u, err := db.GetAdminUser("testadmin")
	if err != nil {
		t.Fatalf("GetAdminUser: %v", err)
	}
	if u.PasswordHash == "$2a$10$N9qo8uLOickgx2ZMRZoMyeIjZAgcfl7p92ldGxad68LJZdL17lhWy" {
		t.Error("password hash should have changed after update")
	}
}

func TestApiConfig_ChangePassword_WrongOldPassword(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	body := map[string]string{
		"old_password": "wrongpassword",
		"new_password": "newpassword123",
	}
	resp := doRequest(t, router, "POST", "/api/config/password", body, cookie)
	assertStatus(t, resp, http.StatusBadRequest)
	assertJSONPath(t, resp, "error", "current password is incorrect")
}

// ═══════════════════════════════════════════════════════════════════════
// Auth gating — admin middleware rejects unauthenticated requests
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_AdminAuth_RequiresLogin(t *testing.T) {
	t.Parallel()
	_, router := newAdminRouter(t)

	endpoints := []struct {
		method string
		path   string
	}{
		{"GET", "/api/processes"},
		{"GET", "/api/styles"},
		{"GET", "/api/warlink/status"},
		{"PUT", "/api/config"},
	}

	for _, ep := range endpoints {
		t.Run(ep.method+"_"+ep.path, func(t *testing.T) {
			resp := doRequest(t, router, ep.method, ep.path, nil, nil)
			// adminMiddleware redirects (303) or returns 401 for HTMX
			if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusSeeOther {
				t.Errorf("unauthenticated %s %s: got status %d, want 401 or 303", ep.method, ep.path, resp.StatusCode)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Bad path parameters — parseID failures
// ═══════════════════════════════════════════════════════════════════════

func TestApiConfig_BadPathParams(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	cases := []struct {
		name   string
		method string
		path   string
	}{
		{"update_process_bad_id", "PUT", "/api/processes/abc"},
		{"delete_process_bad_id", "DELETE", "/api/processes/abc"},
		{"active_style_bad_id", "PUT", "/api/processes/abc/active-style"},
		{"process_styles_bad_id", "GET", "/api/processes/abc/styles"},
		{"update_style_bad_id", "PUT", "/api/styles/abc"},
		{"delete_style_bad_id", "DELETE", "/api/styles/abc"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := map[string]string{"name": "x"}
			resp := doRequest(t, router, tc.method, tc.path, body, cookie)
			assertStatus(t, resp, http.StatusBadRequest)
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// Test helpers (seeding, conversion)
// ═══════════════════════════════════════════════════════════════════════

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

// TestApiConfig_GetCoreNodes_ClaimedElsewhere: what the pickers are told of a
// node Core marks as claimed by another station: that station, as Core named
// it.
func TestApiConfig_GetCoreNodes_ClaimedElsewhere(t *testing.T) {
	t.Parallel()
	h, router := newAdminRouter(t)
	cookie := authCookie(t, h)

	var node protocol.NodeInfo
	if err := json.Unmarshal([]byte(`{"name":"CT-N1","node_type":"","claimed_by":"Line B"}`), &node); err != nil {
		t.Fatalf("decode: %v", err)
	}
	h.engine.(*stubEngine).core = map[string]protocol.NodeInfo{node.Name: node}

	resp := doRequest(t, router, "GET", "/api/core-nodes", nil, cookie)
	assertStatus(t, resp, http.StatusOK)
	var rows []struct {
		Name      string `json:"name"`
		ClaimedBy string `json:"claimed_by"`
	}
	decodeJSON(t, resp, &rows)
	if len(rows) != 1 || rows[0].ClaimedBy != "Line B" {
		t.Errorf("rows = %+v, want CT-N1 claimed by Line B", rows)
	}
}
