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
// its JSON twin show, and where each value comes from.
//
// At the base every request read Core live: /api/containment, a children read
// per group destination and a node-bins read per destination. F2 flipped the
// cases: the page builds from the Edge's held copy of the containment feed
// (LocalContainment) and calls Core zero times. Each case asserts the F2 value
// and carries the base value beside it as `base`.

// containmentCoreStub answers the three Core reads the page used to make and
// counts them per kind, so a pin can say how many calls a render costs.
type containmentCoreStub struct {
	mu       sync.Mutex
	calls    map[string]int
	srv      *httptest.Server
	stateMu  sync.Mutex
	status   int    // /api/containment status
	body     string // /api/containment body
	children map[string][]protocol.NodeChildInfo
	bins     map[string][]map[string]any
}

func newContainmentCoreStub(t *testing.T) *containmentCoreStub {
	t.Helper()
	s := &containmentCoreStub{
		calls:    map[string]int{},
		status:   http.StatusOK,
		children: map[string][]protocol.NodeChildInfo{},
		bins:     map[string][]map[string]any{},
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
		case strings.HasPrefix(r.URL.Path, "/api/telemetry/node/") && strings.HasSuffix(r.URL.Path, "/children"):
			s.count("children")
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/telemetry/node/"), "/children")
			_ = json.NewEncoder(w).Encode(s.children[name])
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

// containmentPinEngine is the shared stub with a real CoreClient pointed at the
// Core stub. The shared stub's CoreAPI returns nil, which the containment page
// cannot survive.
type containmentPinEngine struct {
	*stubEngine
	core *engine.CoreClient
}

func (e *containmentPinEngine) CoreAPI() *engine.CoreClient { return e.core }

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

// containmentPinHeld is the same picture as the Edge holds it after F2: the
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

// newContainmentPinHandlers wires Handlers over the shared stub with a
// CoreClient aimed at a fresh Core stub (answering as before, so a call to it
// would still be counted), the held copy in the stub engine, the claims
// seeded, the group node in the Edge's Core node map, and the real templates.
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
	core.children["FP-GRP"] = []protocol.NodeChildInfo{
		{Name: "FP-GRP.A", NodeType: "STG"},
		{Name: "FP-GRP.SUB", NodeType: "NGRP"}, // a nested group is skipped
		{Name: "FP-GRP.B", NodeType: "STG"},
	}
	core.bins["FP-GRP.A"] = []map[string]any{{"node_name": "FP-GRP.A", "bin_id": 11, "bin_label": "FP-BIN-11", "payload_code": "FP-PART", "uop_remaining": 7, "occupied": true}}
	core.bins["FP-GRP.B"] = []map[string]any{{"node_name": "FP-GRP.B", "bin_id": 13, "bin_label": "FP-BIN-13", "occupied": false}}
	core.bins["FP-PLAIN"] = []map[string]any{{"node_name": "FP-PLAIN", "bin_id": 12, "bin_label": "FP-BIN-12", "payload_code": "FP-PART", "uop_remaining": 3, "occupied": true}}
	h.engine = &containmentPinEngine{stubEngine: stub, core: engine.NewCoreClient(core.srv.URL)}
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
// outbounds, a group destination carries its non-group children by suffix and
// its bins across them, and only occupied bins render. No call reaches Core.
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
		name, got, want, base, label string
	}{
		{"flags", flag, "FP-PART/burr", "same, from Core's body", "F2"},
		{"held bins", held, "FP-BIN-9@", "same, from Core's body", "F2"},
		{"group children (suffix, nested group skipped)", strings.Join(grp.Children, ","), "A,B", "same, from a children read", "F2"},
		{"group outbounds", strings.Join(grp.Outbounds, ","), "FP-OUT-1", "same (Edge's own claims)", "F2"},
		{"group bins (occupied only, across children)", binsOf(grp), "A:FP-BIN-11:FP-PART", "same, from a node-bins read", "F2"},
		{"concrete children", strings.Join(plain.Children, ","), "", "same", "F2"},
		{"concrete bins", binsOf(plain), "FP-PLAIN:FP-BIN-12:FP-PART", "same, from a node-bins read", "F2"},
		{"no notice with a copy held", st.Notice, "", "no such key", "F2"},
		// F2: the three below were 1, 1 and >= 2 (one node-bins read per section).
		{"Core /api/containment calls", itoa(int64(calls["containment"])), "0", "1", "F2"},
		{"Core children calls", itoa(int64(calls["children"])), "0", "1", "F2"},
		{"Core node-bins calls", itoa(int64(calls["node-bins"])), "0", ">= 2", "F2"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q (base: %s, %s)", c.name, c.got, c.want, c.base, c.label)
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
		base, label  string
	}{
		{"flag row with Recall", `data-payload="FP-PART"`, true, "same", "F2"},
		{"flag since, as Core's JSON wrote it", `2026-10-01T08:00:00Z`, true, "same", "F2"},
		{"group section name", `<strong class="mono">FP-GRP</strong>`, true, "same", "F2"},
		{"group child A", `<span class="mono">A</span>`, true, "same", "F2"},
		{"release target", `<span class="mono">FP-OUT-1</span>`, true, "same", "F2"},
		{"bin with Verify Good", `data-node="A" data-bin-id="11"`, true, "same", "F2"},
		{"held bin", `FP-BIN-9`, true, "same", "F2"},
		{"held bin in transit", `in transit`, true, "same", "F2"},
		{"no 'as of' line (fresh)", `as of `, false, "same", "F2"},
		{"no notice", containmentNoData, false, "same", "F2"},
	}
	for _, c := range cases {
		if got := strings.Contains(body, c.needle); got != c.want {
			t.Errorf("%s: contains %q = %v, want %v (base: %s, %s)", c.name, c.needle, got, c.want, c.base, c.label)
		}
	}
	// base: 1   (label F2)
	if n := core.snapshot()["containment"]; n != 0 {
		t.Errorf("page render read Core's /api/containment %d times, want 0 (base: 1, F2)", n)
	}
}

// TestContainmentState_CoreDown_FeedsPin pins the page with Core not
// answering. At the base the read was live, so an unreachable Core was a 500 on
// both the page and the JSON twin, and a Core answering non-200 rendered as
// "nothing contained" because GetContainment decoded the error body as state.
// After F2 neither reads Core: with nothing held the page says "No data from
// Core yet"; with a held copy it renders that copy.
func TestContainmentState_CoreDown_FeedsPin(t *testing.T) {
	t.Run("unreachable, nothing held", func(t *testing.T) {
		h, core := newContainmentPinHandlers(t)
		core.srv.Close()
		h.engine.(*containmentPinEngine).containment = nil

		code, st, raw := getContainmentPinState(t, h)
		// base: 500 "containment read failed"   (label F2)
		if code != http.StatusOK || st.Notice != containmentNoData || len(st.Containment) != 0 {
			t.Errorf("state with Core unreachable = %d %q, want 200 with notice %q (base: 500 containment read failed, F2)",
				code, raw, containmentNoData)
		}
		if st.section("FP-GRP") == nil {
			t.Errorf("sections = %+v, want the claims' sections still rendered", st.Sections)
		}

		rec := httptest.NewRecorder()
		h.handleContainmentPage(rec, httptest.NewRequest(http.MethodGet, "/containment", nil))
		// base: 500 and no such text   (label F2)
		if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "No data from Core yet") {
			t.Errorf("page with Core unreachable = %d, want 200 with %q (base: 500, F2)", rec.Code, containmentNoData)
		}
	})

	t.Run("non-200, copy held", func(t *testing.T) {
		h, core := newContainmentPinHandlers(t)
		core.stateMu.Lock()
		core.status, core.body = http.StatusInternalServerError, `{"error":"database unavailable"}`
		core.stateMu.Unlock()

		code, st, raw := getContainmentPinState(t, h)
		// base: 200 with no flags and no held bins (the error body decoded as
		// state). Now the Core status is never read (F2), and GetContainment
		// itself errors on a non-200 (X3, pinned in engine)   (label F2, X3)
		if code != http.StatusOK || len(st.Containment) != 1 || len(st.HeldBins) != 1 {
			t.Errorf("state with Core 500 = %d %s, want 200 with the held flag and held bin (base: none of either, F2)", code, raw)
		}
		if st.section("FP-GRP") == nil {
			t.Errorf("sections = %+v, want the claims' sections still rendered", st.Sections)
		}
		if n := core.snapshot()["containment"]; n != 0 {
			t.Errorf("Core read %d times, want 0 (base: 1, F2)", n)
		}
	})
}
