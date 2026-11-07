package www

import (
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/store/processes"
)

// handlers_containment_feedspin_test.go — what the Edge containment page and
// its JSON twin show: the flags, held bins and destinations of the Edge's held
// copy of the containment feed (LocalContainment), sectioned by the Edge's own
// claims, with nothing read from Core.
//
// Before "feat(core,edge): Core sends containment, the Edge reads its own
// copy" every request read Core live: /api/containment, a children read per
// group destination and a node-bins read per destination. Since
// "refactor(edge): every act to Core is an ask; the Core HTTP client goes
// (floor-visible)" the Edge has no HTTP client to Core, and nothing is given
// the stub's URL: every zero call count below, and closing the stub or making
// it answer 500, holds by construction.

// containmentCoreStub answers two of the Core reads the page made before it
// read the feed (/api/containment and node-bins; the children read went in
// "refactor(edge,core): group members come from Core's group rows") and
// counts them per kind. Nothing reaches it (see the file header).
type containmentCoreStub struct {
	mu      sync.Mutex
	calls   map[string]int
	srv     *httptest.Server
	stateMu sync.Mutex
	status  int    // /api/containment status
	body    string // /api/containment body
	bins    map[string][]map[string]any
}

func newContainmentCoreStub(t *testing.T) *containmentCoreStub {
	t.Helper()
	s := &containmentCoreStub{
		calls:  map[string]int{},
		status: http.StatusOK,
		bins:   map[string][]map[string]any{},
	}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/containment":
			s.count("containment")
			s.stateMu.Lock()
			status, body := s.status, s.body
			s.stateMu.Unlock()
			w.WriteHeader(status)
			if _, err := w.Write([]byte(body)); err != nil {
				t.Errorf("stub Core: write /api/containment body: %v", err)
			}
		case r.URL.Path == "/api/telemetry/node-bins":
			s.count("node-bins")
			out := []map[string]any{}
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				out = append(out, s.bins[n]...)
			}
			_ = json.NewEncoder(w).Encode(out)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(s.srv.Close)
	return s
}

func (s *containmentCoreStub) count(kind string) {
	s.mu.Lock()
	s.calls[kind]++
	s.mu.Unlock()
}

func (s *containmentCoreStub) snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]int, len(s.calls))
	for k, v := range s.calls {
		out[k] = v
	}
	return out
}

// containmentPinEngine is the shared stub the containment pins drive.
type containmentPinEngine struct {
	*stubEngine
}

// seedContainmentPinClaims declares two containment destinations on produce
// claims: FP-GRP (a group node in the Edge's Core node map) releasing to
// FP-OUT-1, and FP-PLAIN (a concrete node) releasing to FP-OUT-2. The style is
// retired on cleanup so the rows leave ListAllContainmentClaims for the rest of
// the package (testDB is shared).
func seedContainmentPinClaims(t *testing.T) {
	t.Helper()
	pid, err := testDB.CreateProcess("FeedsPinContProc-"+t.Name(), "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	styleID, err := testDB.CreateStyle("FEEDSPIN-CONT-"+t.Name(), "", pid)
	testutil.MustNoErr(t, err, "create style")
	t.Cleanup(func() { _ = testDB.DeleteStyle(styleID) })
	for _, c := range []struct{ node, out, dest string }{
		{"FP-PLN-1", "FP-OUT-1", "FP-GRP"},
		{"FP-PLN-2", "FP-OUT-2", "FP-PLAIN"},
	} {
		_, err := testDB.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: styleID, CoreNodeName: c.node, Role: "produce",
			SwapMode: "two_robot_press_index", PayloadCode: "FP-PART", UOPCapacity: 10,
			PairedCoreNode: c.node + "-B", OutboundDestination: c.out,
			ContainmentDestination: c.dest,
		})
		testutil.MustNoErr(t, err, "upsert claim "+c.node)
	}
}

const containmentPinStateBody = `{
 "containment":[{"payload_code":"FP-PART","active":true,"reason":"burr","activated_by":"qa","activated_at":"2026-10-01T08:00:00Z","deactivated_by":"","deactivated_at":null}],
 "held_bins":[{"bin_id":9,"label":"FP-BIN-9","payload_code":"FP-PART","node_name":"","hold_by":"edge.test","hold_at":"2026-10-01T08:05:00Z"}]
}`

// containmentPinHeld is the same picture as the Edge holds it: the
// flags and held bins of containmentPinStateBody, and the two destinations as
// Core's snapshot builds them (the group's non-group children, the occupied
// bins only), confirmed just now.
func containmentPinHeld(t *testing.T) *engine.ContainmentState {
	t.Helper()
	at := func(s string) *time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return &v
	}
	now := time.Now()
	return &engine.ContainmentState{
		Containment: []engine.ContainmentRow{{PayloadCode: "FP-PART", Active: true, Reason: "burr",
			ActivatedBy: "qa", ActivatedAt: at("2026-10-01T08:00:00Z")}},
		HeldBins: []engine.HeldBinRow{{BinID: 9, Label: "FP-BIN-9", PayloadCode: "FP-PART",
			HoldBy: "edge.test", HoldAt: at("2026-10-01T08:05:00Z")}},
		Destinations: []protocol.ContainmentDestination{
			{Node: "FP-GRP", Children: []string{"FP-GRP.A", "FP-GRP.B"}, Bins: []protocol.ContainmentBin{
				{Node: "FP-GRP.A", BinID: 11, Label: "FP-BIN-11", PayloadCode: "FP-PART", UOP: 7}}},
			{Node: "FP-PLAIN", Children: []string{}, Bins: []protocol.ContainmentBin{
				{Node: "FP-PLAIN", BinID: 12, Label: "FP-BIN-12", PayloadCode: "FP-PART", UOP: 3}}},
		},
		ReceivedAt: now, ConfirmedAt: now,
	}
}

// newContainmentPinHandlers wires Handlers over the shared stub with the held
// copy in the stub engine, the claims seeded, the group node in the Edge's Core
// node map, and the real templates, and returns a fresh Core stub answering as
// Core did. No client is aimed at the stub (the Edge has none), so its counts
// stay zero whatever the page does.
func newContainmentPinHandlers(t *testing.T) (*Handlers, *containmentCoreStub) {
	t.Helper()
	h, _ := newTestHandlers(t)
	stub := h.engine.(*stubEngine)
	stub.containment = containmentPinHeld(t)
	stub.core = map[string]protocol.NodeInfo{
		"FP-GRP":   {Name: "FP-GRP", NodeType: "NGRP"},
		"FP-PLAIN": {Name: "FP-PLAIN", NodeType: "STG"},
	}
	core := newContainmentCoreStub(t)
	core.body = containmentPinStateBody
	core.bins["FP-GRP.A"] = []map[string]any{{"node_name": "FP-GRP.A", "bin_id": 11, "bin_label": "FP-BIN-11", "payload_code": "FP-PART", "uop_remaining": 7, "occupied": true}}
	core.bins["FP-GRP.B"] = []map[string]any{{"node_name": "FP-GRP.B", "bin_id": 13, "bin_label": "FP-BIN-13", "occupied": false}}
	core.bins["FP-PLAIN"] = []map[string]any{{"node_name": "FP-PLAIN", "bin_id": 12, "bin_label": "FP-BIN-12", "payload_code": "FP-PART", "uop_remaining": 3, "occupied": true}}
	h.engine = &containmentPinEngine{stubEngine: stub}
	h.tmpl = template.Must(template.New("").Funcs(templateFuncs()).
		ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))
	seedContainmentPinClaims(t)
	return h, core
}

// containmentPinState is the JSON twin's body as the pin reads it.
type containmentPinState struct {
	Containment []containmentFlagRow `json:"containment"`
	Sections    []containmentSection `json:"sections"`
	HeldBins    []containmentHeldRow `json:"held_bins"`
	Notice      string               `json:"notice"`
}

func (s containmentPinState) section(name string) *containmentSection {
	for i := range s.Sections {
		if s.Sections[i].NodeName == name {
			return &s.Sections[i]
		}
	}
	return nil
}

func getContainmentPinState(t *testing.T, h *Handlers) (int, containmentPinState, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.apiGetContainmentState(rec, httptest.NewRequest(http.MethodGet, "/api/containment/state", nil))
	var st containmentPinState
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &st); err != nil {
			t.Fatalf("decode state: %v (%s)", err, rec.Body.String())
		}
	}
	return rec.Code, st, rec.Body.String()
}

// TestContainmentState_FeedsPin pins GET /api/containment/state: the flags and
// held bins are the held copy's, each destination is a section with its claims'
// outbounds, and a group destination carries its children by suffix and its
// bins across them. Which children and bins the copy holds (non-group
// children, occupied bins) is Core's snapshot's choice; the fixture is built
// that way. No call reaches Core.
func TestContainmentState_FeedsPin(t *testing.T) {
	h, core := newContainmentPinHandlers(t)

	code, st, raw := getContainmentPinState(t, h)
	if code != http.StatusOK {
		t.Fatalf("GET /api/containment/state = %d (%s)", code, raw)
	}
	grp, plain := st.section("FP-GRP"), st.section("FP-PLAIN")
	if grp == nil || plain == nil {
		t.Fatalf("sections = %+v, want FP-GRP and FP-PLAIN", st.Sections)
	}
	flag := ""
	if len(st.Containment) == 1 && st.Containment[0].Active {
		flag = st.Containment[0].PayloadCode + "/" + st.Containment[0].Reason
	}
	held := ""
	if len(st.HeldBins) == 1 {
		held = st.HeldBins[0].Label + "@" + st.HeldBins[0].NodeName
	}
	binsOf := func(sec *containmentSection) string {
		var parts []string
		for _, b := range sec.Bins {
			parts = append(parts, b.NodeName+":"+b.Label+":"+b.PayloadCode)
		}
		return strings.Join(parts, ",")
	}
	calls := core.snapshot()

	cases := []struct {
		name, got, want string
	}{
		{"flags", flag, "FP-PART/burr"},
		{"held bins", held, "FP-BIN-9@"},
		{"group children (suffix, nested group skipped)", strings.Join(grp.Children, ","), "A,B"},
		{"group outbounds", strings.Join(grp.Outbounds, ","), "FP-OUT-1"},
		{"group bins (occupied only, across children)", binsOf(grp), "A:FP-BIN-11:FP-PART"},
		{"concrete children", strings.Join(plain.Children, ","), ""},
		{"concrete bins", binsOf(plain), "FP-PLAIN:FP-BIN-12:FP-PART"},
		{"no notice with a copy held", st.Notice, ""},
		// Each was nonzero when the page read Core live: 1, 1 and >= 2 (one
		// node-bins read per section). Zero by construction now (file header).
		{"Core /api/containment calls", itoa(int64(calls["containment"])), "0"},
		{"Core children calls", itoa(int64(calls["children"])), "0"},
		{"Core node-bins calls", itoa(int64(calls["node-bins"])), "0"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.name, c.got, c.want)
		}
	}
}

// TestContainmentPage_FeedsPin pins the server-rendered page over the same
// picture: the active flag with its Recall verb, the group destination with
// its children, the bin with its Verify Good verb, and the held bin.
func TestContainmentPage_FeedsPin(t *testing.T) {
	h, core := newContainmentPinHandlers(t)

	rec := httptest.NewRecorder()
	h.handleContainmentPage(rec, httptest.NewRequest(http.MethodGet, "/containment", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /containment = %d (%s)", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	cases := []struct {
		name, needle string
		want         bool
	}{
		{"flag row with Recall", `data-payload="FP-PART"`, true},
		{"flag since, as Core's JSON wrote it", `2026-10-01T08:00:00Z`, true},
		{"group section name", `<strong class="mono">FP-GRP</strong>`, true},
		{"group child A", `<span class="mono">A</span>`, true},
		{"release target", `<span class="mono">FP-OUT-1</span>`, true},
		{"bin with Verify Good", `data-node="A" data-bin-id="11"`, true},
		{"held bin", `FP-BIN-9`, true},
		{"held bin in transit", `in transit`, true},
		{"no 'as of' line (fresh)", `as of `, false},
		{"no notice", containmentNoData, false},
	}
	for _, c := range cases {
		if got := strings.Contains(body, c.needle); got != c.want {
			t.Errorf("%s: contains %q = %v, want %v", c.name, c.needle, got, c.want)
		}
	}
	// 1 when the page read Core live; zero by construction now (file header).
	if n := core.snapshot()["containment"]; n != 0 {
		t.Errorf("page render read Core's /api/containment %d times, want 0", n)
	}
}

// TestContainmentState_CoreDown_FeedsPin pins the page and its JSON twin by
// what the Edge holds, not by Core: with nothing held both answer 200, say
// "No data from Core yet" and still render the claims' sections; with a copy
// held they render that copy. Closing the Core stub or making it answer 500,
// as each case does, changes nothing: nothing reaches the stub (file header).
// The name is the Core outage it was written for. Before "feat(core,edge): Core
// sends containment, the Edge reads its own copy" the read was live, so an
// unreachable Core was a 500 on both, and a Core answering non-200 rendered as
// "nothing contained" because the Edge decoded the error body as state.
func TestContainmentState_CoreDown_FeedsPin(t *testing.T) {
	t.Run("unreachable, nothing held", func(t *testing.T) {
		h, core := newContainmentPinHandlers(t)
		core.srv.Close()
		h.engine.(*containmentPinEngine).containment = nil

		code, st, raw := getContainmentPinState(t, h)
		// Was 500 "containment read failed".
		if code != http.StatusOK || st.Notice != containmentNoData || len(st.Containment) != 0 {
			t.Errorf("state with Core unreachable = %d %q, want 200 with notice %q",
				code, raw, containmentNoData)
		}
		if st.section("FP-GRP") == nil {
			t.Errorf("sections = %+v, want the claims' sections still rendered", st.Sections)
		}

		rec := httptest.NewRecorder()
		h.handleContainmentPage(rec, httptest.NewRequest(http.MethodGet, "/containment", nil))
		// Was 500, with no such text.
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No data from Core yet") {
			t.Errorf("page with Core unreachable = %d, want 200 with %q", rec.Code, containmentNoData)
		}
	})

	t.Run("non-200, copy held", func(t *testing.T) {
		h, core := newContainmentPinHandlers(t)
		core.stateMu.Lock()
		core.status, core.body = http.StatusInternalServerError, `{"error":"database unavailable"}`
		core.stateMu.Unlock()

		code, st, raw := getContainmentPinState(t, h)
		// Was 200 with no flags and no held bins (the error body decoded as
		// state).
		if code != http.StatusOK || len(st.Containment) != 1 || len(st.HeldBins) != 1 {
			t.Errorf("state with Core 500 = %d %s, want 200 with the held flag and held bin", code, raw)
		}
		if st.section("FP-GRP") == nil {
			t.Errorf("sections = %+v, want the claims' sections still rendered", st.Sections)
		}
		if n := core.snapshot()["containment"]; n != 0 {
			t.Errorf("Core read %d times, want 0", n)
		}
	})
}
