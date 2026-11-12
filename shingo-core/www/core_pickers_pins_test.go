package www

import (
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"html"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/gorilla/sessions"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/service"
)

var updateCorePickersGolden = flag.Bool("update-core-pickers-golden", false,
	"rewrite testdata/core-pickers/*.txt from the Core pages as they are")

// TestCorePickerPins characterises every Core page that chooses a node, a node
// group or a part (census rows #50-#71: Bins, Nodes, Orders, Test orders, RDS
// explorer) at the tree BEFORE the shared picker replaces them, so each later
// diff shows as the option, request or refusal that moved:
//
//   - bins-template.txt / test-orders-template.txt: the two pages' templates
//     rendered as router.go renders them: the hosts the shared node and part
//     pickers mount on, and the lists the page carries for them;
//   - refusal-shapes.txt: what the handlers behind these pages write when they
//     refuse, taken from the handlers themselves where they refuse before
//     reading the database, and from the writers they call otherwise;
//   - the rest (*-page.txt, kanban.txt, loaders.txt, ...): the pages run under
//     node, unmodified, through their own functions: the option lists each
//     builds, the request each pick sends, and what each shows when the answer
//     is a refusal (the shapes above, fed back), a 5xx, a non-JSON page or a
//     fetch that never connects.
//
// The harness is testdata/core-pickers-harness (pins.test.js says what each
// golden covers and how each act is driven). Bugs it records are pinned as
// they are and named in the goldens; nothing here or there changes a page.
//
// Regenerate with
//
//	go test ./www -run TestCorePickerPins -update-core-pickers-golden
//
// and read the diff: a regenerated golden pins nothing unless someone read it.
func TestCorePickerPins(t *testing.T) {
	t.Parallel()
	nodePath, err := exec.LookPath("node")
	if err != nil {
		t.Skipf("node not on PATH; skipping the Core picker pins")
	}
	fx := corePickerFixture()
	fx.Refusals = corePickerRefusals(t)
	raw, err := json.MarshalIndent(fx, "", "  ")
	if err != nil {
		t.Fatalf("encode the fixture: %v", err)
	}
	in := filepath.Join(t.TempDir(), "fixture.json")
	if err := os.WriteFile(in, raw, 0o644); err != nil {
		t.Fatalf("write the fixture: %v", err)
	}

	cmd := exec.Command(nodePath, filepath.Join("testdata", "core-pickers-harness", "pins.test.js"), in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.Output()
	if err != nil {
		t.Fatalf("run the Core picker pins: %v\n%s", err, stderr.String())
	}
	var out struct {
		Goldens map[string]string `json:"goldens"`
	}
	if err := json.Unmarshal(stdout, &out); err != nil {
		t.Fatalf("decode the pins' output: %v\n%s", err, stderr.String())
	}
	if len(out.Goldens) == 0 {
		t.Fatalf("the harness produced no goldens; it has drifted\n%s", stderr.String())
	}
	got := out.Goldens
	got["bins-template.txt"] = corePickerBinsTemplate(t, fx)
	got["test-orders-template.txt"] = corePickerTestOrdersTemplate(t, fx)
	got["refusal-shapes.txt"] = corePickerRefusalGolden(fx.Refusals)

	corePickerCensusBugs(t, got)

	// ITS OWN DIRECTORY: the stale check below owns every file in it.
	dir := filepath.Join("testdata", "core-pickers")
	names := make([]string, 0, len(got))
	for n := range got {
		names = append(names, n)
	}
	sort.Strings(names)
	if *updateCorePickersGolden {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	for _, n := range names {
		path := filepath.Join(dir, n)
		if *updateCorePickersGolden {
			if err := os.WriteFile(path, []byte(got[n]), 0o644); err != nil {
				t.Fatalf("write %s: %v", path, err)
			}
			continue
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: no golden (%v); regenerate with -update-core-pickers-golden and read it", n, err)
			continue
		}
		if line, w, g, ok := corePickerFirstDiff(string(want), got[n]); !ok {
			t.Errorf("%s: the page moved at line %d; regenerate only for a named change\n  golden: %s\n  page:   %s",
				path, line, w, g)
		}
	}

	// A golden no pin writes any more pins nothing and reads as if it did.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	for _, e := range entries {
		if _, ok := got[e.Name()]; ok || e.IsDir() {
			continue
		}
		if *updateCorePickersGolden {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil {
				t.Fatalf("remove stale %s: %v", e.Name(), err)
			}
			continue
		}
		t.Errorf("%s: a golden no pin writes; delete it, or restore the pin that wrote it", filepath.Join(dir, e.Name()))
	}
	if stderr.Len() > 0 {
		t.Logf("harness stderr:\n%s", stderr.String())
	}
}

// corePickerCensusBugs holds the census bugs the shared adapters fixed
// (census §4 #6, #7, #11; the P0 report's assignReason and otherGroups),
// beside the goldens that show them, so a change that brings one back says so.
// Each check reads the page's own output.
func corePickerCensusBugs(t *testing.T, got map[string]string) {
	t.Helper()
	// #11, fixed: Create bin's Node list no longer offers _TRANSIT, and shows
	// the robot deck and a disabled node greyed with the reason (was
	// templates/bins.html:342, {{range .Nodes}}).
	bins := got["bins-page.txt"]
	create := bins[:strings.Index(bins, "== (end of Create bin)")]
	for _, line := range []string{
		`"11" | _ROBOT:AMR-01 · robot  (off: a robot, not a place)`,
		`"12" | OLD-SLOT  (off: disabled)`,
		`pick "10": not taken: not offered`,
	} {
		if !strings.Contains(create, line) {
			t.Errorf("census #11: Create bin's Node list no longer holds %q", line)
		}
	}
	if strings.Contains(create, `| _TRANSIT`) {
		t.Errorf("census #11: Create bin offers _TRANSIT")
	}
	// The twins, fixed: the Test orders page and the Orders page draw one node
	// list through one builder (was buildNodePickerOptions in Go, byte order,
	// beside orders.js buildNodeOptionsHTML, localeCompare).
	if !strings.Contains(got["test-orders-page.txt"], "the same rows as the Orders page's manual order: yes") {
		t.Errorf("the twins: the Test orders page no longer draws the Orders page's node rows")
	}
	// otherGroups, fixed: the kanban section's Overflow and Sources show a
	// disabled group greyed and do not take it (was nodes-kanban.js:170, which
	// offered GRP-OFF in both).
	kanban := got["kanban-page.txt"]
	for _, line := range []string{
		`"GRP-OFF" | GRP-OFF · group  (off: disabled)`,
		`pick "14" (GRP-OFF): not taken: offered greyed: disabled`,
	} {
		if !strings.Contains(kanban, line) {
			t.Errorf("otherGroups: the kanban section no longer holds %q", line)
		}
	}
	// assignReason, fixed: an armed place slot dims the robot deck, a
	// synthetic node that is no group, and a disabled node or group, each with
	// its reason, and a Windows slot dims a disabled node (was
	// static/pages/loaders.js:1172, which lit all four).
	stations := got["stations-page.txt"]
	for _, re := range []string{
		`(?s)armed: UNL-A "Fulls come from".*?tile  _ROBOT:AMR-01 +dim "a robot, not a place"`,
		`(?s)armed: UNL-A "Fulls come from".*?tile  OLD-SLOT +dim "disabled"`,
		`(?s)armed: UNL-A "Fulls come from".*?tile  VIRT-1 +dim "synthetic, not a place"`,
		`(?s)armed: UNL-A "Fulls come from".*?group GRP-OFF +dim "disabled"`,
		`(?s)armed: UNL-A Windows;.*?tile  OLD-SLOT +dim "disabled"`,
	} {
		if !regexp.MustCompile(re).MatchString(stations) {
			t.Errorf("assignReason: the stations grid no longer matches %s", re)
		}
	}
	// #6, fixed: a group whose children are lanes sits in its slots' zone with
	// its lane and the lane's slots under it, and a disabled group's live
	// member is offered (was orders.js:843 zoneOf one level, :864 one level
	// drawn, :859 the disabled parent dropping its member).
	orders := got["orders-manual-order-page.txt"]
	for _, line := range []string{
		"  [ZONE-1]\n    \"GRP-A\" | GRP-A · group\n      \"GRP-A-L1\" | GRP-A-L1 · lane\n      \"GRP-A-L1-S1\" | ↳ GRP-A-L1-S1\n",
		"      \"GRP-OFF-N1\" | GRP-OFF-N1\n",
	} {
		if !strings.Contains(orders, line) {
			t.Errorf("census #6: the manual order's node list no longer holds %q", line)
		}
	}
	// #7, fixed: a failed first /api/nodes read says so in the list, and the
	// next open asks again (was orders.js:805, the flag set before the read).
	if !strings.Contains(orders, "-- second open: asks again\n  GET /api/nodes\n") ||
		!strings.Contains(orders, "Could not read the node list") {
		t.Errorf("census #7: a failed first node read is not said, or not asked again on the next open")
	}
}

// ── the synthetic plant ──────────────────────────────────────────────────────

// corePickerFixtureData is what the harness reads: the plant as the server
// serialises it (domain structs through encoding/json, the shape /api/nodes,
// /api/payloads and the templates' *JSON values carry), plus the refusals.
type corePickerFixtureData struct {
	Nodes           []*domain.Node              `json:"nodes"`
	Payloads        []*domain.Payload           `json:"payloads"`
	BinTypes        []*domain.BinType           `json:"binTypes"`
	PayloadBinTypes map[string][]int64          `json:"payloadBinTypes"`
	AvailableBins   []map[string]string         `json:"availableBins"`
	Refusals        map[string]corePickerAnswer `json:"refusals"`
}

func corePickerID(v int64) *int64 { return &v }

// corePickerFixture is one plant with every kind of node a picker has to
// decide about: a group of lanes (GRP-A), a group of plain members (GRP-B), a
// disabled group whose member is live (GRP-OFF), a lane, plain nodes with and
// without a zone (one with a display name), _TRANSIT, a robot deck, a
// disabled node and a synthetic node that is no group. Names are synthetic.
func corePickerFixture() corePickerFixtureData {
	ngrp, lane := protocol.NodeClassNGRP, protocol.NodeClassLANE
	nodes := []*domain.Node{
		{ID: 1, Name: "GRP-A", IsSynthetic: true, Enabled: true, NodeTypeCode: ngrp},
		{ID: 2, Name: "GRP-A-L1", IsSynthetic: true, Enabled: true, NodeTypeCode: lane, ParentID: corePickerID(1), ParentName: "GRP-A"},
		{ID: 3, Name: "GRP-A-L1-S1", Enabled: true, Zone: "ZONE-1", ParentID: corePickerID(2), ParentName: "GRP-A-L1"},
		{ID: 4, Name: "GRP-A-L1-S2", Enabled: true, Zone: "ZONE-1", ParentID: corePickerID(2), ParentName: "GRP-A-L1"},
		{ID: 5, Name: "GRP-B", IsSynthetic: true, Enabled: true, NodeTypeCode: ngrp},
		{ID: 6, Name: "GRP-B-N1", Enabled: true, Zone: "ZONE-2", ParentID: corePickerID(5), ParentName: "GRP-B"},
		{ID: 7, Name: "GRP-B-N2", Enabled: true, Zone: "ZONE-2", ParentID: corePickerID(5), ParentName: "GRP-B"},
		{ID: 8, Name: "LINE-1", DisplayName: "Line one", Enabled: true, Zone: "ZONE-2"},
		{ID: 9, Name: "SPOT-9", Enabled: true},
		{ID: 10, Name: domain.TransitNodeName, IsSynthetic: true, Enabled: true},
		{ID: 11, Name: "_ROBOT:AMR-01", IsSynthetic: true, Enabled: true},
		{ID: 12, Name: "OLD-SLOT", Enabled: false, Zone: "ZONE-1"},
		{ID: 13, Name: "VIRT-1", IsSynthetic: true, Enabled: true},
		{ID: 14, Name: "GRP-OFF", IsSynthetic: true, Enabled: false, NodeTypeCode: ngrp},
		{ID: 15, Name: "GRP-OFF-N1", Enabled: true, Zone: "ZONE-2", ParentID: corePickerID(14), ParentName: "GRP-OFF"},
	}
	payloads := []*domain.Payload{
		{ID: 1, Code: "PART-B", Description: "Part B", UOPCapacity: 10},
		{ID: 2, Code: "PART-A", Description: "Part A", UOPCapacity: 20},
		{ID: 3, Code: "PART-C", Description: "Part C", UOPCapacity: 5},
	}
	binTypes := []*domain.BinType{
		{ID: 1, Code: "CART-S", Description: "small cart"},
		{ID: 2, Code: "CART-L", Description: "large cart"},
	}
	return corePickerFixtureData{
		Nodes:    nodes,
		Payloads: payloads,
		BinTypes: binTypes,
		// As handleBins builds it: an empty list is unrestricted.
		PayloadBinTypes: map[string][]int64{"PART-A": {}, "PART-B": {2}, "PART-C": {1}},
		// As apiListAvailableBins answers (handlers_orders.go:1125).
		AvailableBins: []map[string]string{
			{"label": "BIN-1", "node_name": "GRP-A-L1-S1", "zone": "ZONE-1", "payload_code": "PART-A"},
			{"label": "BIN-2", "node_name": "GRP-B-N1", "zone": "ZONE-2", "payload_code": "PART-B"},
			{"label": "BIN-3", "node_name": "LINE-1", "zone": "ZONE-2", "payload_code": ""},
			{"label": "BIN-4", "node_name": "GRP-B-N2", "zone": "ZONE-2", "payload_code": "PART-C"},
			{"label": "BIN-5", "node_name": "GRP-B-N2", "zone": "ZONE-2", "payload_code": "PART-C"},
		},
	}
}

// ── refusal shapes ───────────────────────────────────────────────────────────

// corePickerAnswer is one HTTP answer the harness hands a page's fetch.
type corePickerAnswer struct {
	Status      int    `json:"status"`
	ContentType string `json:"contentType"`
	Body        string `json:"body"`
	From        string `json:"from"`
}

func corePickerTake(rec *httptest.ResponseRecorder, from string) corePickerAnswer {
	return corePickerAnswer{Status: rec.Code, ContentType: rec.Header().Get("Content-Type"), Body: rec.Body.String(), From: from}
}

// corePickerRefusals collects the refusals the pages are fed. A handler that
// refuses before it reads anything is called as it is, on a zero Handlers; the
// rest come from the writer the handler calls (jsonError, kanbanRefused,
// kanbanHeld), with a synthetic sentence, because the sentence is the
// service's and the page shows it unchanged.
func corePickerRefusals(t *testing.T) map[string]corePickerAnswer {
	t.Helper()
	h := &Handlers{}
	post := func(fn http.HandlerFunc, path, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		fn(rec, httptest.NewRequest(http.MethodPost, path, strings.NewReader(body)))
		return rec
	}
	out := map[string]corePickerAnswer{
		"loader.set-home.400":     corePickerTake(post(h.apiSetLoaderHome, "/api/loader/set-home", `{}`), "apiSetLoaderHome, a missing id"),
		"loader.update.400":       corePickerTake(post(h.apiUpdateLoader, "/api/loader/update", `{}`), "apiUpdateLoader, a missing id"),
		"loader.set-payload.400":  corePickerTake(post(h.apiSetLoaderPayload, "/api/loader/set-payload", `{}`), "apiSetLoaderPayload, a missing id"),
		"orders.spot.400":         corePickerTake(post(h.apiManualOrderSubmit, "/api/orders/spot", `{"order_type":"staged"}`), "apiManualOrderSubmit, staged with no source"),
		"robots.move.400":         corePickerTake(post(h.apiRobotMoveTo, "/api/robots/move", `{}`), "apiRobotMoveTo, no destination"),
		"nodes.reparent.400":      corePickerTake(post(h.apiReparentNode, "/api/nodes/reparent", `{}`), "apiReparentNode, no node_id"),
		"kanban.sources.400":      corePickerTake(post(h.apiKanbanSourcesSet, "/api/nodes/kanban/sources", `{}`), "apiKanbanSourcesSet, no group"),
		"kanban.settings.400":     corePickerTake(post(h.apiKanbanSettingsSet, "/api/nodes/kanban/settings", `{}`), "apiKanbanSettingsSet, no group"),
		"test-orders.submit.400":  corePickerTake(post(h.apiTestOrderSubmit, "/api/test-orders/submit", `{"order_type":"move"}`), "apiTestOrderSubmit, no payload"),
		"test-orders.complex.400": corePickerTake(post(h.apiKafkaComplexOrderSubmit, "/api/test-orders/submit/complex", `{}`), "apiKafkaComplexOrderSubmit, no location"),
		"json.invalid.400":        corePickerTake(post(h.apiBinAction, "/api/bins/action", `not json`), "parseJSON, any handler"),
	}

	// The auth wall every /api/ write sits behind (auth.go requireAuth).
	authed := &Handlers{sessions: sessions.NewCookieStore([]byte("core-picker-pins-key-0123456789"))}
	rec := httptest.NewRecorder()
	authed.requireAuth(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).
		ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/loader/update", strings.NewReader(`{}`)))
	out["auth.401"] = corePickerTake(rec, "requireAuth, an expired session")

	// Writers with the service's sentence standing in.
	rec = httptest.NewRecorder()
	h.jsonError(rec, "refused: the service's sentence", http.StatusBadRequest)
	out["json.400"] = corePickerTake(rec, "jsonError 400 (loaderWriteStatus, binMoveStatus, a bin action's refusal)")
	rec = httptest.NewRecorder()
	h.jsonError(rec, "refused: the service's sentence", http.StatusConflict)
	out["json.409"] = corePickerTake(rec, "jsonError 409 (binMoveStatus conflict)")
	rec = httptest.NewRecorder()
	h.jsonError(rec, "failed: the store's error", http.StatusInternalServerError)
	out["json.500"] = corePickerTake(rec, "jsonError 500 (any handler's store error)")
	rec = httptest.NewRecorder()
	h.kanbanRefused(rec, service.KanbanCheck{Refusals: []string{"GRP-B is already the staging group for loader LDR-B"}})
	out["kanban.409"] = corePickerTake(rec, "kanbanRefused (a rule refusal)")
	rec = httptest.NewRecorder()
	h.kanbanHeld(rec, service.KanbanGuard{Blocked: "2 carriers are standing in GRP-A", Drain: []string{"order 41 to LINE-1"}})
	out["kanban.409.needs_force"] = corePickerTake(rec, "kanbanHeld (the holds-bins guard)")

	// The reparent guard writes its 409 inline (apiReparentNode, the
	// order_ids answer); this is that shape, field for field.
	reparent, _ := json.Marshal(map[string]any{
		"error":     fmt.Sprintf("cannot reparent: %d active order(s) reference group %q as source", 1, "GRP-A"),
		"order_ids": []int64{41},
	})
	out["nodes.reparent.409"] = corePickerAnswer{Status: http.StatusConflict, ContentType: "application/json",
		Body: string(reparent) + "\n", From: "apiReparentNode's inline 409 (built here in its shape)"}

	// Create bin is a form post: http.Error text, read by the browser, not
	// by a script.
	form := url.Values{"bin_type_id": {"not-a-number"}}
	req := httptest.NewRequest(http.MethodPost, "/bins/create", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec = httptest.NewRecorder()
	h.handleBinCreate(rec, req)
	out["bins.create.400"] = corePickerTake(rec, "handleBinCreate, a bad bin type")

	// Not Core's: what a proxy or a dead upstream answers in front of it.
	out["proxy.502"] = corePickerAnswer{Status: http.StatusBadGateway, ContentType: "text/html",
		Body: "<html><body>502 Bad Gateway</body></html>", From: "a proxy in front of Core (not a Core shape)"}
	return out
}

func corePickerRefusalGolden(r map[string]corePickerAnswer) string {
	var b strings.Builder
	b.WriteString("# What the handlers behind the Core pickers write when they refuse.\n")
	b.WriteString("# Read from the handlers (zero Handlers, before any store read) or the writers\n")
	b.WriteString("# they call; the harness feeds these to the pages' fetch.\n\n")
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		a := r[k]
		fmt.Fprintf(&b, "%s  (%s)\n  %d %s\n  %q\n", k, a.From, a.Status, a.ContentType, a.Body)
	}
	b.WriteString("\nhandleBinCreate's other refusals, by httpStatusForCreate (the service's sentence decides):\n")
	for _, msg := range []string{"node 99 not found", "cannot create multiple bins at a physical node", "node LINE-1 already has a bin", "label BIN-1 already exist", "disk full"} {
		fmt.Fprintf(&b, "  %-50q -> %d\n", msg, httpStatusForCreate(errors.New(msg)))
	}
	return b.String()
}

// ── template-rendered lists ──────────────────────────────────────────────────

var (
	corePickerSelectRe       = regexp.MustCompile(`(?s)<select([^>]*)>(.*?)</select>`)
	corePickerOptionRe       = regexp.MustCompile(`(?s)<option([^>]*)>(.*?)</option>`)
	corePickerAttrRe         = regexp.MustCompile(`([a-z_-]+)="([^"]*)"`)
	corePickerNameRe         = regexp.MustCompile(`<(?:input|select|textarea)[^>]*\bname="([^"]+)"`)
	corePickerNumericValueRe = regexp.MustCompile(`^value="[0-9]+"`)
)

func corePickerAttrs(s string) map[string]string {
	out := map[string]string{}
	for _, m := range corePickerAttrRe.FindAllStringSubmatch(s, -1) {
		out[m[1]] = m[2]
	}
	return out
}

// corePickerOptions lists a select's options as `value="v" label="l"`,
// whitespace collapsed, entities as rendered.
func corePickerOptions(inner string) []string {
	var out []string
	for _, m := range corePickerOptionRe.FindAllStringSubmatch(inner, -1) {
		a := corePickerAttrs(m[1])
		label := strings.Join(strings.Fields(m[2]), " ")
		out = append(out, fmt.Sprintf("value=%q label=%q", a["value"], label))
	}
	return out
}

func corePickerSelect(t *testing.T, page, key, want string) (string, []string) {
	t.Helper()
	for _, m := range corePickerSelectRe.FindAllStringSubmatch(page, -1) {
		if corePickerAttrs(m[1])[key] == want {
			return m[1], corePickerOptions(m[2])
		}
	}
	t.Fatalf("no <select %s=%q> in the rendered page", key, want)
	return "", nil
}

// corePickerBinsTemplate renders bins.html with the plant and reads census
// C1, the Create bin form's Node select, with the fields the form posts.
func corePickerBinsTemplate(t *testing.T, fx corePickerFixtureData) string {
	t.Helper()
	nodesJSON, _ := json.Marshal(fx.Nodes)
	payloadsJSON, _ := json.Marshal(fx.Payloads)
	binTypesJSON, _ := json.Marshal(fx.BinTypes)
	pbtJSON, _ := json.Marshal(fx.PayloadBinTypes)
	page := renderPage(t, "bins.html", map[string]any{
		"Page": "bins", "Bins": []binRow{}, "BinTypes": fx.BinTypes, "CarrierFamilies": []string{},
		"Payloads": fx.Payloads, "BinHasNotes": map[int64]bool{},
		"NodesJSON": template.JS(nodesJSON), "PayloadsJSON": template.JS(payloadsJSON),
		"BinTypesJSON": template.JS(binTypesJSON), "PayloadBinTypesJSON": template.JS(pbtJSON),
	})
	var b strings.Builder
	b.WriteString("# Core Bins, Create bin (census #50, C1): templates/bins.html rendered as router.go renders it.\n")
	b.WriteString("# The Node field is a host bins.js mounts the shared node picker on, over PAGE_NODES (its rows\n")
	b.WriteString("# and the hidden node_id input it adds: bins-page.txt).\n\n")
	form := page[strings.Index(page, `action="/bins/create"`):]
	form = form[:strings.Index(form, "</form>")]
	if !strings.Contains(form, `<div id="bin-create-node"></div>`) {
		t.Errorf("bins.html's Create bin form no longer holds the Node picker's host")
	}
	b.WriteString("Node (optional): <div id=\"bin-create-node\"> inside the form\n")
	var fields []string
	for _, m := range corePickerNameRe.FindAllStringSubmatch(form, -1) {
		fields = append(fields, m[1])
	}
	b.WriteString("\nThe form posts (a native POST /bins/create, gated by bins.js confirmBinCreate; the body is the\n")
	b.WriteString("browser's): " + strings.Join(fields, ", ") + ", and node_id from the picker's hidden input\n")
	b.WriteString("On success: 303 to /bins. On a refusal: http.Error text, which the browser shows as the page\n")
	b.WriteString("(see refusal-shapes.txt bins.create.400); no script reads it.\n")
	for _, v := range []string{"PAGE_NODES", "PAGE_PAYLOADS", "PAGE_BIN_TYPES", "PAGE_PAYLOAD_BIN_TYPES"} {
		if !strings.Contains(page, "var "+v+" = ") {
			t.Errorf("bins.html no longer sets %s; the page script reads it", v)
		}
	}
	b.WriteString("The node list is serialised once, as PAGE_NODES for bins.js (Create bin, Move, the label check).\n")
	return b.String()
}

// corePickerTestOrdersTemplate renders test-orders.html (census #58-#62, C7
// and C8): the hosts its node and part pickers mount on, and the two lists
// the page carries for them.
func corePickerTestOrdersTemplate(t *testing.T, fx corePickerFixtureData) string {
	t.Helper()
	nodesJSON, _ := json.Marshal(fx.Nodes)
	payloadsJSON, _ := json.Marshal(fx.Payloads)
	page := renderPage(t, "test-orders.html", map[string]any{
		"Page": "test-orders", "NodesJSON": string(nodesJSON), "PayloadsJSON": string(payloadsJSON),
	})
	var b strings.Builder
	b.WriteString("# Core Test orders (census #58-#62, C7 + C8): test-orders.html as served. Each node and payload\n")
	b.WriteString("# field is a host test-orders.js mounts the shared picker on (its rows: test-orders-page.txt);\n")
	b.WriteString("# the page carries the node and part lists once, on #page-data.\n\n")
	for _, m := range regexp.MustCompile(`<div id="([a-z0-9-]+)" data-placeholder="([^"]*)"></div>`).FindAllStringSubmatch(page, -1) {
		fmt.Fprintf(&b, "host %-17s placeholder %q\n", m[1], m[2])
	}
	pd := regexp.MustCompile(`<div id="page-data"([^>]*)>`).FindStringSubmatch(page)
	if pd == nil {
		t.Fatalf("test-orders.html has no #page-data")
	}
	a := corePickerAttrs(pd[1])
	var nodes, parts []map[string]any
	if err := json.Unmarshal([]byte(html.UnescapeString(a["data-nodes"])), &nodes); err != nil {
		t.Errorf("#page-data data-nodes is not the node list: %v", err)
	}
	if err := json.Unmarshal([]byte(html.UnescapeString(a["data-payloads"])), &parts); err != nil {
		t.Errorf("#page-data data-payloads is not the part list: %v", err)
	}
	fmt.Fprintf(&b, "\n#page-data carries %d nodes (data-nodes) and %d payloads (data-payloads), as the server read them.\n", len(nodes), len(parts))
	if strings.Contains(page, "<select") && regexp.MustCompile(`<select[^>]*id="(k|kc|d|cx)-[a-z0-9-]*(node|location|source|staging[12]|outgoing|payload)"`).MatchString(page) {
		t.Errorf("test-orders.html still draws a node or payload <select>")
	}
	return b.String()
}

func corePickerFirstDiff(want, got string) (line int, w, g string, ok bool) {
	if want == got {
		return 0, "", "", true
	}
	wl, gl := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(wl) || i < len(gl); i++ {
		var a, c string
		if i < len(wl) {
			a = wl[i]
		}
		if i < len(gl) {
			c = gl[i]
		}
		if a != c {
			return i + 1, a, c, false
		}
	}
	return len(wl), "", "", false
}
