package www

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/engine"
	"shingoedge/store/catalog"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

var updateEdgePagesGolden = flag.Bool("update-edge-pages-golden", false,
	"rewrite testdata/edge-pages/*.txt from the current Manual order, Production and Kafka Direct pages")

// TestEdgePagePickerPins characterises the node and part pickers on the Edge
// pages other than Processes — Manual order (manual-order.js), Production
// (production.js) and Kafka Direct (manual-message.js) — at the tree BEFORE a
// later phase moves them onto one shared picker, so every later diff shows up
// as the option, label or request that moved. Census rows 38–44 (E14–E19).
//
//   - <page>-options.txt: every option list the page builds, from the
//     synthetic plant below: node groups with members, a member whose bare
//     name collides (kept qualified, as Engine.SetCoreNodes keeps it), a lane,
//     plain nodes, _TRANSIT, a robot carrier node, a name carrying markup, and
//     process nodes Core does not list. The SSE `core-nodes` rebuild is drawn
//     too (census bug 8: it drops the group/lane tag and the "↳"). NodeInfo
//     carries no disabled flag, so no option list can show one.
//   - <page>-requests.txt: what each pick sends, method, url and body, and
//     every answer its handler can give: the refusal ({"error"} from
//     writeError), the 202 pending ask (writeAskPending), a 5xx, and a fetch
//     that rejects — with what the page shows and does next.
//
// THE PAGES ARE THE REAL ONES, rendered by their own handlers
// (handleManualOrder, handleProduction, handleManualMessage) through the real
// templates over the shared stub, so a change to the handler's page data, the
// template's markup or the page script all show here. The answers are the
// real handlers' and writers' bytes (apiListPayloadCatalog,
// apiPayloadManifest, writeError, writeAskPending, chi's Recoverer). The
// harness that boots each page under node, and what it stubs, is
// testdata/edge-pages-harness/pins.js.
//
// The bugs pinned as they are, each named in its golden: the SSE rebuild's
// lost tags (manual-order.js and production.js onCoreNodes), Kafka Direct's
// unescaped innerHTML (manual-message.js renderField) and its order.request
// with no payload_code field, and the Production "compatible with" picker
// sending its first allowed part by default (production.js
// openRequestEmptyModal).
//
// Regenerate with
//
//	go test ./www -run TestEdgePagePickerPins -update-edge-pages-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestEdgePagePickerPins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the Edge page picker pins")
	}
	h, _ := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	seedEdgePagesPlant(t, h)
	h.tmpl = template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	in := map[string]any{
		"pages": map[string]string{
			"manual-order":   renderEdgePage(t, h.handleManualOrder, "/manual-order"),
			"production":     renderEdgePage(t, h.handleProduction, "/production"),
			"manual-message": renderEdgePage(t, h.handleManualMessage, "/manual-message"),
		},
		// The node list as the engine emits it on a sync (CoreNodesUpdatedEvent,
		// the SSE `core-nodes` frame), in a fixed order: the engine's own order is
		// Core's, which a pin cannot hold still.
		"coreNodesEvent": engine.CoreNodesUpdatedEvent{Nodes: sortedNodeInfos(stub.core)},
		"answers":        edgePageAnswers(t, h),
	}
	raw, err := json.Marshal(in)
	testutil.MustNoErr(t, err, "encode the pages")
	input := filepath.Join(t.TempDir(), "pages.json")
	testutil.MustNoErr(t, os.WriteFile(input, raw, 0o644), "write the pages")

	cmd := exec.Command(nodePath, filepath.Join("testdata", "edge-pages-harness", "pins.js"), input)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the Edge page pins: %v\n%s", err, stderr.String())
	}
	var got map[string]string
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(got) == 0 {
		t.Fatal("the pins produced no golden; the harness has drifted")
	}
	checkEdgePagesGoldens(t, got)
}

// seedEdgePagesPlant is the synthetic plant: Core's node list as the engine
// holds it after a sync, one process with a screen, three process nodes (one
// Core does not list), two catalog parts, and one live order whose uuid
// carries markup for the Kafka Direct page's order list.
func seedEdgePagesPlant(t *testing.T, h *Handlers) {
	t.Helper()
	stub := h.engine.(*stubEngine)
	db := stub.db
	stub.core = map[string]protocol.NodeInfo{}
	for _, n := range []protocol.NodeInfo{
		{Name: "GRP-A", NodeType: protocol.NodeClassNGRP},
		{Name: "SLOT-A1"}, {Name: "SLOT-A2"}, // GRP-A's members, bare
		{Name: "GRP-B", NodeType: protocol.NodeClassNGRP},
		{Name: "GRP-B.SLOT-A1"}, // a member whose bare name is taken stays qualified
		{Name: "LANE-1", NodeType: protocol.NodeClassLANE},
		{Name: "PRESS-1"}, {Name: "DOCK-1"},
		{Name: "_TRANSIT"}, {Name: "_ROBOT:AMR-01"},
		{Name: `NODE <b>&"'`}, // census bug 9: a name carrying markup
	} {
		stub.core[n.Name] = n
	}

	pid, err := db.CreateProcess("Line A", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: "scr-1", Name: "Screen 1", Enabled: true})
	testutil.MustNoErr(t, err, "create station")
	for i, n := range []processes.NodeInput{
		{ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "PRESS-1", Name: "Press one"},
		{ProcessID: pid, OperatorStationID: &sid, CoreNodeName: "SLOT-A1", Name: "Slot one"},
		{ProcessID: pid, CoreNodeName: "LOCAL-9", Name: "Local spot"}, // not on Core's list; no screen
	} {
		n.Sequence, n.Enabled = i+1, true
		_, err := db.CreateProcessNode(n)
		testutil.MustNoErr(t, err, "create process node "+n.Name)
	}

	for _, e := range []*catalog.CatalogEntry{
		{ID: 1, Name: "Part A", Code: "PART-A", UOPCapacity: 24,
			Manifest: []protocol.ManifestItem{{PartNumber: "PN-A1", PartsPerCycle: 2, Description: "left"}}},
		{ID: 2, Name: "Part <b>B</b>", Code: "PART-B", UOPCapacity: 10},
	} {
		testutil.MustNoErr(t, db.UpsertPayloadCatalog(e), "seed catalog "+e.Code)
	}

	_, err = db.CreateOrder(`ab<b>&"'-0001`, protocol.OrderTypeRetrieve, nil, false, 1, "PRESS-1", "", "",
		protocol.Height{}, false, "PART-A", "", "")
	testutil.MustNoErr(t, err, "seed the live order")
}

// sortedNodeInfos is the node map as a list in name order.
func sortedNodeInfos(m map[string]protocol.NodeInfo) []protocol.NodeInfo {
	out := make([]protocol.NodeInfo, 0, len(m))
	for _, n := range m {
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// renderEdgePage renders one page through its handler and the real templates.
func renderEdgePage(t *testing.T, handler http.HandlerFunc, path string) string {
	t.Helper()
	w := httptest.NewRecorder()
	handler(w, httptest.NewRequest("GET", path, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d\n%s", path, w.Code, w.Body.String())
	}
	return w.Body.String()
}

// edgePageAnswer is one HTTP answer as the harness's fetch replays it.
type edgePageAnswer struct {
	Status int    `json:"status"`
	Body   string `json:"body"`
}

func answerOf(w *httptest.ResponseRecorder) edgePageAnswer {
	return edgePageAnswer{Status: w.Code, Body: w.Body.String()}
}

// edgePageAnswers are the bytes each answer the pins replay is made of, from
// the code that writes them: the catalog and manifest reads through their
// handlers, and each refusal shape through the writer its handler calls.
func edgePageAnswers(t *testing.T, h *Handlers) map[string]edgePageAnswer {
	t.Helper()
	get := func(pattern, path string, fn http.HandlerFunc) edgePageAnswer {
		r := chi.NewRouter()
		r.Get(pattern, fn)
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		return answerOf(w)
	}
	shape := func(fn func(w http.ResponseWriter)) edgePageAnswer {
		w := httptest.NewRecorder()
		fn(w)
		return answerOf(w)
	}
	pending := httptest.NewRecorder()
	if !writeAskPending(pending, httptest.NewRequest("POST", "/", nil), engine.ErrAskPending) {
		t.Fatal("writeAskPending wrote nothing for ErrAskPending")
	}
	// A panic in a handler, as the router's Recoverer answers it: the one 5xx
	// the request-empty and load-bin doors can give (they write no 500).
	panicked := httptest.NewRecorder()
	middleware.Recoverer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic(errors.New("pinned panic"))
	})).ServeHTTP(panicked, httptest.NewRequest("POST", "/", nil))
	return map[string]edgePageAnswer{
		"catalog":         get("/api/payload-catalog", "/api/payload-catalog", h.apiListPayloadCatalog),
		"manifest:PART-A": get("/api/payload/{code}/manifest", "/api/payload/PART-A/manifest", h.apiPayloadManifest),
		"manifest:PART-B": get("/api/payload/{code}/manifest", "/api/payload/PART-B/manifest", h.apiPayloadManifest),
		"pending":         answerOf(pending),
		"panic":           answerOf(panicked),
		// apiCreateRetrieveOrder: Core made none at the loader's windows
		// (engine.LoaderRetrieveRefused -> 409 with Core's sentence).
		"retrieve-refused": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusConflict, "node SLOT-A1: no empty ordered — SLOT-A1: window-reserved")
		}),
		"retrieve-500": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusInternalServerError, "create retrieve: database is locked")
		}),
		// apiCreateMoveOrder: an engine refusal is a 500.
		"move-500": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusInternalServerError, "create move order: database is locked")
		}),
		"complex-refused": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, "steps are required")
		}),
		// apiRequestEmptyBin / apiLoadBin: every engine refusal is a 400.
		"request-empty-refused": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, `payload "PART-B" not in allowed list for node Slot one`)
		}),
		"load-bin-refused": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, "node Slot one: a bin is already loaded")
		}),
		// apiSendManualMessage.
		"message-refused": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusBadRequest, "invalid payload: json: cannot unmarshal string into Go struct field OrderRequest.quantity of type int64")
		}),
		"message-500": shape(func(w http.ResponseWriter) {
			writeError(w, http.StatusInternalServerError, "send failed: kafka: client has run out of available brokers")
		}),
		"message-ok": shape(func(w http.ResponseWriter) {
			writeJSON(w, map[string]any{"status": "ok", "msg_id": "0f0e0d0c-0000-4000-8000-000000000001", "timestamp": "2026-11-11T12:00:00Z"})
		}),
		"order-ok": shape(func(w http.ResponseWriter) { writeJSON(w, map[string]any{"id": 41, "uuid": "u-41"}) }),
		"act-ok": shape(func(w http.ResponseWriter) {
			writeActionOK(w, httptest.NewRequest("POST", "/", nil), "refreshMaterial")
		}),
	}
}

// checkEdgePagesGoldens compares got with testdata/edge-pages, or rewrites it
// under -update-edge-pages-golden.
func checkEdgePagesGoldens(t *testing.T, got map[string]string) {
	t.Helper()
	// ITS OWN DIRECTORY, because the stale check below owns every file in it
	// (the trap processes_desktop_setup_pins_test.go's comment records).
	dir := filepath.Join("testdata", "edge-pages")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateEdgePagesGolden {
		testutil.MustNoErr(t, os.MkdirAll(dir, 0o755), "mkdir "+dir)
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateEdgePagesGolden {
			testutil.MustNoErr(t, os.WriteFile(path, []byte(got[n]), 0o644), "write "+path)
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-edge-pages-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := firstDiffLine(string(want), got[n]); !ok {
			t.Errorf("%s: the page moved at line %d; regenerate only for a named change\n  golden: %s\n  page:   %s",
				path, line, w, g)
		}
	}
	entries, err := os.ReadDir(dir)
	testutil.MustNoErr(t, err, "read "+dir)
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if *updateEdgePagesGolden {
			testutil.MustNoErr(t, os.Remove(filepath.Join(dir, e.Name())), "remove stale "+e.Name())
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
	t.Logf("edge page pins: %s", strings.Join(names, ", "))
}
