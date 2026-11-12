package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
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
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	"shingoedge/www"
)

// door_walk_test.go — every door that can change a station view says so.
//
// The station board does not poll (operator.js). It refetches when the
// stream brings it an event it takes and that names something it draws
// (operator-state.js eventConcerns), on every connect, on a gap and on
// becoming visible. So any write to an input of the view that reaches the
// glass with no such event leaves the board stale until something else
// happens. This walks every door through which such a write can arrive:
//
//   - every non-GET route of the REAL router (chi.Walk over www.NewRouter),
//   - every envelope type and every Data subject of the REAL inbound
//     dispatch table (buildInboundRouter, the one setupKafkaSubscribers runs),
//
// each through its fixture, against a real engine and the real stream. Before
// and after each door it reads every station's view ETag; if one moved, the
// frames the stream carried during the door must include one the station
// subscribes to (operator.js stationEvents) and accepts for the view it held
// (eventConcerns, run in node). A route or subject with no fixture fails, and
// so does a fixture whose door no longer exists: a new door cannot be added
// without being walked.
//
// Each fixture also declares the status it answers and whether it moves a
// station view, so a fixture that stops reaching its write fails rather than
// passing on nothing.

// The walk's router runs both of its coalescing windows at walkNudgeWindow
// (www.WithCoalescingWindows) instead of the shipped 250 ms, and every wait
// below is a multiple of it. What a door can send, and when:
//
//   - every frame its act broadcasts is in the hub's queue when the act
//     returns: the handlers, the inbound dispatch and the engine's bus are
//     synchronous, and the hub's queue, its fan-out and the one stream are
//     first in, first out, so those frames arrive in the order sent;
//   - a nudge is broadcast a window after the FIRST Nudge of its burst, so an
//     act whose nudges are more than a window apart sends two (a node list
//     nudges once for the loaders and again for the bin types). The last is
//     broadcast at most walkNudgeWindow after the act returned.
//
// So walkNudgeWindow after the act, everything the door sent is on its way and
// what is left is delivery: a few milliseconds here, measured with the rest of
// the Edge's unit tests running beside the walk. A door that nudged stops
// collecting once its nudge is in and walkSettle has passed (two windows of
// delivery for a second burst); a door that did not is waited walkWindow (four
// windows past the nudge's, the margin a nudge that never came would have had).
// The shipped windows had the same shape: a 250 ms nudge and a 450 ms wait.
// The plant-claims window sends no frame here (its hooks are unwired); it is
// short so that no loop the router runs outlasts a door.
const (
	walkNudgeWindow = 10 * time.Millisecond
	walkSettle      = 3 * walkNudgeWindow
	walkWindow      = 5 * walkNudgeWindow
)

// fileServerRoutes are mounted with r.Handle, so chi lists them under every
// method; each serves embedded files and writes nothing.
var fileServerRoutes = map[string]bool{"/favicon.ico": true, "/static/*": true, "/static/shared/*": true}

// routeFixture is one route's walk: prep (optional) runs before the views are
// read and returns the request; want is the status; moves is whether a station
// view's ETag moves.
type routeFixture struct {
	req   func(w *walk) (path string, body any)
	want  int
	moves bool
}

// subjectFixture is one inbound key's walk: the envelope to dispatch.
type subjectFixture struct {
	env   func(w *walk) *protocol.Envelope
	moves bool
}

type sseFrameW struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

type walkCase struct {
	Door    string          `json:"door"`
	Station int64           `json:"station"`
	View    json.RawMessage `json:"view"`
	Frames  []sseFrameW     `json:"frames"`
}

type walk struct {
	t       *testing.T
	db      *store.DB
	eng     *engine.Engine
	srv     *httptest.Server
	mux     *chi.Mux
	inbound interface {
		Dispatch(env *protocol.Envelope, key string)
	}
	cookie   *http.Cookie
	frames   chan sseFrameW
	stations []int64
	cases    []walkCase
	moved    map[string]bool
	prepped  bool

	// the plant
	press        int64 // Press A1's process
	pressStation int64
	pressNode    int64 // PLN_01, a position of the press station
	styleA       int64 // a style of the press, not active
	order        int64 // an order on the press station's node
	orderUUID    string
	loaderProc   int64
	loaderSt     int64
	loaderNode   int64
	seq          int
	routing      int
}

func (w *walk) uniq(prefix string) string {
	w.seq++
	return fmt.Sprintf("%s-%d", prefix, w.seq)
}

func TestDoorWalk_EveryWriteToAStationViewIsAnnounced(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH: the station's eventConcerns runs in node")
	}
	w := newWalk(t)

	// An idle Edge says nothing: the walk attributes each frame to the door
	// that ran, so a background frame would be credited to a door that sent
	// none. (It is also the board's idle cost: no frame, no fetch.)
	//
	// An absence, so the wait is set by what could speak, not by the walk's
	// windows: the engine's shortest loops are the CATID monitor's 500 ms poll
	// (plcPollInterval) and the PLC counter poll (cfg.PollRate, 1 s by
	// default), and 1.5 s sees each of them tick at least once. The router's
	// nudge loop speaks only after a Nudge, within walkNudgeWindow. The
	// release intent floor (15 s) and the 60 s lineside, stranded and demand
	// loops are longer than any wait a walk can afford; a frame from one of
	// them would be credited to the door it lands in.
	if got := w.collect(1500 * time.Millisecond); len(got) != 0 {
		t.Fatalf("the idle Edge sent %d frames in 1.5 s (%v): the walk cannot attribute frames", len(got), got)
	}

	// The screen's "online" is its stream's: a stream naming the station
	// touches it on connect (www.NewRouter wires the hub to StationService).
	w.touchedByStream(w.loaderSt)

	// ── the routes ──
	var routes []string
	if err := chi.Walk(w.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		if method == http.MethodGet || method == http.MethodHead || fileServerRoutes[route] {
			return nil
		}
		routes = append(routes, method+" "+route)
		return nil
	}); err != nil {
		t.Fatalf("chi.Walk: %v", err)
	}
	sort.Strings(routes)
	fixtures := routeFixtures()
	seen := map[string]bool{}
	for _, key := range routes {
		seen[key] = true
		fx, ok := fixtures[key]
		if !ok {
			t.Errorf("route %s has no fixture: add one to routeFixtures (door_walk_test.go) so its writes are walked", key)
			continue
		}
		method := strings.SplitN(key, " ", 2)[0]
		w.door(key, fx.moves, fx.want, func() func() int {
			path, body := fx.req(w)
			return func() int { return w.request(method, path, body) }
		})
	}
	for key := range fixtures {
		if !seen[key] {
			t.Errorf("fixture %s names no route of the router: delete it or fix its key", key)
		}
	}

	// ── the inbound dispatch table ──
	subjects := subjectFixtures()
	var keys []string
	for _, typ := range protocol.AllTypes() {
		if typ != protocol.TypeData {
			keys = append(keys, typ)
		}
	}
	for _, s := range protocol.EdgeInboundSubjects() {
		keys = append(keys, "data:"+s)
	}
	seenKeys := map[string]bool{}
	for _, key := range keys {
		seenKeys[key] = true
		fx, ok := subjects[key]
		if !ok {
			t.Errorf("inbound %s has no fixture: add one to subjectFixtures (door_walk_test.go)", key)
			continue
		}
		w.door("inbound "+key, fx.moves, 0, func() func() int {
			env := fx.env(w)
			return func() int { w.inbound.Dispatch(env, env.Type); return 0 }
		})
	}
	for key := range subjects {
		if !seenKeys[key] {
			t.Errorf("subject fixture %s names nothing the Edge receives: delete it or fix its key", key)
		}
	}

	w.judge()
	var moved []string
	for d := range w.moved {
		moved = append(moved, d)
	}
	sort.Strings(moved)
	t.Logf("%d routes and %d inbound keys walked; %d moved a station view: %s",
		len(routes), len(keys), len(moved), strings.Join(moved, ", "))
}

// door runs one door between two reads of every station's view. prep builds
// what the door needs (a fixture may write through the API: api) and returns
// the act; its own frames are let through before the first read.
func (w *walk) door(name string, wantMoves bool, want int, prep func() func() int) {
	w.t.Helper()
	w.prepped = false
	act := prep()
	if w.prepped {
		w.collect(walkWindow)
	}
	w.collect(0) // drop anything left from the door before
	before := w.views(nil)
	status := act()
	frames := w.collect(walkWindow)
	after := w.views(before)
	if want != 0 && status != want {
		w.t.Errorf("%s answered %d, its fixture says %d: the fixture no longer reaches what it walked", name, status, want)
	}
	moved := false
	for sid, b := range before {
		if after[sid].etag == b.etag {
			continue
		}
		moved = true
		w.cases = append(w.cases, walkCase{Door: name, Station: sid, View: b.body, Frames: frames})
	}
	if moved {
		w.moved[name] = true
	}
	w.t.Logf("door %s: status %d, moved %v, frames %d", name, status, moved, len(frames))
	if moved != wantMoves {
		w.t.Errorf("%s moved a station view = %v, its fixture says %v", name, moved, wantMoves)
	}
}

type viewSnap struct {
	etag string
	body json.RawMessage
}

// views reads every station's view. With prev, each read quotes the ETag prev
// holds, as the board's refetch does (operator.js): a view the door did not
// move must answer 304 with that ETag (and keeps prev's body), one it moved
// 200 with a new ETag. So every door that moves nothing also pins, on these
// populated boards and through the real router, that an unchanged board's
// refetch costs it no body.
func (w *walk) views(prev map[int64]viewSnap) map[int64]viewSnap {
	w.t.Helper()
	out := map[int64]viewSnap{}
	for _, sid := range w.stations {
		req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/operator-stations/%d/view", w.srv.URL, sid), nil)
		if err != nil {
			w.t.Fatalf("view %d request: %v", sid, err)
		}
		held, conditional := prev[sid]
		if conditional {
			req.Header.Set("If-None-Match", held.etag)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			w.t.Fatalf("view %d: %v", sid, err)
		}
		body, err := io.ReadAll(res.Body)
		if err != nil {
			w.t.Fatalf("read view %d: %v", sid, err)
		}
		res.Body.Close()
		etag := res.Header.Get("ETag")
		switch {
		case res.StatusCode == http.StatusNotFound:
			continue // a station a door deleted
		case res.StatusCode == http.StatusNotModified && conditional:
			if etag != held.etag || len(body) != 0 {
				w.t.Errorf("view %d answered 304 with ETag %q and %d bytes to If-None-Match %q", sid, etag, len(body), held.etag)
			}
			out[sid] = held
		case res.StatusCode == http.StatusOK:
			if conditional && etag == held.etag {
				w.t.Errorf("view %d answered 200 and a full body to If-None-Match of its own ETag %q", sid, etag)
			}
			out[sid] = viewSnap{etag: etag, body: body}
		default:
			w.t.Fatalf("view %d: %d %s", sid, res.StatusCode, body)
		}
	}
	return out
}

func (w *walk) collect(d time.Duration) []sseFrameW { return collectFrames(w.frames, d) }

// collectFrames returns every frame that arrives on ch within d of the call
// (0: only those already waiting). It ends early once a nudge has arrived and
// walkSettle has passed: the last nudge an act asked for is broadcast at most
// walkNudgeWindow after the act returned, and everything the act broadcast
// precedes it on the stream, so past both there is nothing more to come from
// that act. d is a door's or an update's wait (walkWindow) and must exceed
// walkSettle.
func collectFrames(ch <-chan sseFrameW, d time.Duration) []sseFrameW {
	var out []sseFrameW
	deadline := time.After(d)
	settle := time.After(walkSettle)
	nudged, settled := false, false
	for {
		select {
		case f := <-ch:
			out = append(out, f)
			nudged = nudged || f.Type == "material-refresh"
			continue
		default:
		}
		if d == 0 || (nudged && settled) {
			return out
		}
		select {
		case f := <-ch:
			out = append(out, f)
			nudged = nudged || f.Type == "material-refresh"
		case <-settle:
			settled = true
		case <-deadline:
			return out
		}
	}
}

func (w *walk) request(method, path string, body any) int {
	w.t.Helper()
	var rd io.Reader
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case url.Values:
		rd = strings.NewReader(b.Encode())
		contentType = "application/x-www-form-urlencoded"
	case string:
		rd = strings.NewReader(b)
	default:
		raw, err := json.Marshal(b)
		if err != nil {
			w.t.Fatalf("%s %s body: %v", method, path, err)
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, rd)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", contentType)
	if w.cookie != nil {
		req.AddCookie(w.cookie)
	}
	client := &http.Client{
		Timeout:       20 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	res, err := client.Do(req)
	if err != nil {
		w.t.Fatalf("%s %s: %v", method, path, err)
	}
	raw, err := io.ReadAll(res.Body)
	if err != nil {
		w.t.Fatalf("read %s %s: %v", method, path, err)
	}
	res.Body.Close()
	if res.StatusCode/100 != 2 {
		if len(raw) > 160 {
			raw = raw[:160]
		}
		w.t.Logf("%s %s answered %d: %s", method, path, res.StatusCode, strings.TrimSpace(string(raw)))
	}
	return res.StatusCode
}

// judge runs every case through the station's own predicate, in node.
func (w *walk) judge() {
	w.t.Helper()
	if len(w.cases) == 0 {
		w.t.Fatal("no door moved a station view: the walk walked nothing")
	}
	verdicts := judgeCases(w.t, w.cases)
	for i, ok := range verdicts {
		if ok {
			continue
		}
		c := w.cases[i]
		var names []string
		for _, f := range c.Frames {
			names = append(names, f.Type+" "+string(f.Data))
		}
		w.t.Errorf("%s moved station %d's view and the stream carried nothing that station takes for it "+
			"(frames: %v). The board would stay stale: give this door an event the station takes "+
			"(an /api write gets the nudge by answering 2xx; an engine door emits EventBoardNudge).",
			c.Door, c.Station, names)
	}
}

// judgeCases answers, per case, whether any of its frames is of a type the
// board takes (operator.js stationEvents) and concerns the view it held
// (operator-state.js eventConcerns): the board's own decision to rebuild.
func judgeCases(t *testing.T, cases []walkCase) []bool {
	t.Helper()
	dir := filepath.Join("..", "..", "www", "static", "operator-station")
	src, err := os.ReadFile(filepath.Join(dir, "operator.js"))
	if err != nil {
		t.Fatalf("read operator.js: %v", err)
	}
	types := stationEventTypes(t, string(src))
	input, err := json.Marshal(map[string]any{"types": types, "cases": cases})
	if err != nil {
		t.Fatalf("marshal cases: %v", err)
	}
	inFile := filepath.Join(t.TempDir(), "cases.json")
	if err := os.WriteFile(inFile, input, 0o600); err != nil {
		t.Fatalf("write cases: %v", err)
	}
	script := filepath.Join(t.TempDir(), "judge.js")
	if err := os.WriteFile(script, []byte(judgeJS), 0o600); err != nil {
		t.Fatalf("write judge: %v", err)
	}
	statePath, err := filepath.Abs(filepath.Join(dir, "operator-state.js"))
	if err != nil {
		t.Fatalf("state path: %v", err)
	}
	out, err := exec.Command("node", script, statePath, inFile).CombinedOutput()
	if err != nil {
		t.Fatalf("judge: %v: %s", err, out)
	}
	var verdicts []bool
	if err := json.Unmarshal(out, &verdicts); err != nil || len(verdicts) != len(cases) {
		t.Fatalf("judge answered %q (%v)", out, err)
	}
	return verdicts
}

// stationEventTypes reads the keys of operator.js's stationEvents, the types
// the board subscribes to.
func stationEventTypes(t *testing.T, src string) []string {
	t.Helper()
	block := regexp.MustCompile(`(?s)const stationEvents = \{(.*?)\n\};`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("operator.js has no stationEvents block: the walk cannot tell what the board takes")
	}
	var out []string
	for _, m := range regexp.MustCompile(`(?m)^\s*'([a-z-]+)':`).FindAllStringSubmatch(block[1], -1) {
		out = append(out, m[1])
	}
	if len(out) < 10 {
		t.Fatalf("read %d types from stationEvents (%v): the pattern no longer matches the file", len(out), out)
	}
	return out
}

// judgeJS loads operator-state.js as the board does (exports made plain) and
// answers, per case, whether any frame is of a type the board takes and
// concerns the view it held.
const judgeJS = `
const fs = require('fs'), vm = require('vm');
const src = fs.readFileSync(process.argv[2], 'utf8').replace(/^export\s+/gm, '');
const ctx = vm.createContext({ console, JSON, Object, Array, String, Number });
vm.runInContext(src + '\nthis.eventConcerns = eventConcerns;', ctx);
const input = JSON.parse(fs.readFileSync(process.argv[3], 'utf8'));
const takes = new Set(input.types);
const out = input.cases.map(c => (c.frames || []).some(f => takes.has(f.type) && ctx.eventConcerns(c.view, f.type, f.data)));
process.stdout.write(JSON.stringify(out));
`

// ── the plant ────────────────────────────────────────────────────────────────

type nopHeartbeater struct{}

func (nopHeartbeater) SendRegister()       {}
func (nopHeartbeater) RequestNodeSync()    {}
func (nopHeartbeater) RequestCatalogSync() {}

func newWalk(t *testing.T) *walk {
	t.Helper()
	fx := scenefixtures.A()
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, fx, "Press A1")

	cfg := config.Defaults()
	cfg.StationUID = "walk.edge"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	// Core's catalog has arrived (empty): the press-index changeover's gate
	// opens on it, as it opened on a configured Core address.
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)

	w := &walk{t: t, db: db, eng: eng, moved: map[string]bool{}, frames: make(chan sseFrameW, 4096)}
	w.press = seeded.ProcessID
	w.pressStation = seeded.Stations["screen-a4"]
	w.pressNode = seeded.Nodes["PLN_01"]
	for name, id := range seeded.Styles {
		p, err := db.GetProcess(w.press)
		if err != nil {
			t.Fatalf("read process: %v", err)
		}
		if p != nil && (p.ActiveStyleID == nil || *p.ActiveStyleID != id) {
			if w.styleA == 0 || name < w.styleName(w.styleA) {
				w.styleA = id
			}
		}
	}
	if w.pressStation == 0 || w.pressNode == 0 || w.styleA == 0 {
		t.Fatalf("seeded plant lacks the station, node or a spare style: %+v", seeded)
	}

	// A loader board beside the press: its tiles read Core's loader list.
	var err error
	w.loaderProc, err = db.CreateProcess("WALK-LOADER", "", "", "", false)
	if err != nil {
		t.Fatalf("create loader process: %v", err)
	}
	w.loaderSt, err = db.CreateOperatorStation(stations.Input{ProcessID: w.loaderProc, Code: "WALK-LDR", Name: "Walk Loader", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create loader station: %v", err)
	}
	w.loaderNode, err = db.CreateProcessNode(processes.NodeInput{ProcessID: w.loaderProc, OperatorStationID: &w.loaderSt,
		CoreNodeName: "SMN_W1", Code: "SMN_W1", Name: "SMN_W1", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create loader node: %v", err)
	}

	var nodes []protocol.NodeInfo
	for _, n := range fx.Nodes {
		name := n.Name
		if n.ParentName != nil && *n.ParentName != "" {
			name = *n.ParentName + "." + n.Name
		}
		nodes = append(nodes, protocol.NodeInfo{Name: name})
	}
	nodes = append(nodes, protocol.NodeInfo{Name: "SMN_W1"}, protocol.NodeInfo{Name: "SMN_W2"})
	eng.SetCoreNodes(nodes, false)
	if err := eng.SetCoreLoaders(walkLoaders("operator")); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	var pbt []protocol.PayloadBinTypeInfo
	for code, types := range fx.PayloadBinTypeCodes() {
		for _, bt := range types {
			pbt = append(pbt, protocol.PayloadBinTypeInfo{PayloadCode: code, BinTypeCode: bt})
		}
	}
	eng.SetPayloadBinTypes(pbt)

	// An order on the press station's first node.
	w.orderUUID = "walk-order-1"
	w.order, err = db.CreateOrder(w.orderUUID, protocol.OrderTypeRetrieve, &w.pressNode, false, 1, "PLN_01", "", "", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET status='queued' WHERE id=?`, w.order); err != nil {
		t.Fatalf("queue order: %v", err)
	}

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, handler, stop := www.NewRouter(eng, dbg, nil, www.WithCoalescingWindows(walkNudgeWindow, walkNudgeWindow))
	t.Cleanup(stop)
	mux, ok := handler.(*chi.Mux)
	if !ok {
		t.Fatalf("NewRouter returned %T, not a *chi.Mux", handler)
	}
	w.mux = mux
	w.srv = httptest.NewServer(handler)
	t.Cleanup(w.srv.Close)
	w.inbound = buildInboundRouter(eng, messaging.NewEdgeHandler(eng.OrderManager()), nopHeartbeater{}, func(string, ...any) {})
	w.stations = []int64{w.pressStation, w.loaderSt}

	// The first login creates the admin and signs it in.
	w.cookie = w.login()

	// The stream, as a board opens it but with every topic: the judge decides
	// what the board takes.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, w.srv.URL+"/events", nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open /events: %v", err)
	}
	connected := make(chan struct{})
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		var typ string
		var data []byte
		first := true
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if typ == "connected" && first {
					first = false
					close(connected)
				} else if typ != "" && typ != "heartbeat" && typ != "connected" {
					w.frames <- sseFrameW{Type: typ, Data: append(json.RawMessage(nil), data...)}
				}
				typ, data = "", nil
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = []byte(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	select {
	case <-connected:
	case <-time.After(5 * time.Second):
		t.Fatal("the stream never connected")
	}
	// Let what the setup sent land, then drop it; a frame that lands later is
	// the idle check's to find.
	time.Sleep(walkWindow)
	w.collect(0)
	return w
}

// touchedByStream opens a stream naming station sid and checks the station
// was marked online. The view requests the walk makes touch nothing.
func (w *walk) touchedByStream(sid int64) {
	w.t.Helper()
	if st, err := w.db.GetOperatorStation(sid); err != nil || st.LastSeenAt != nil {
		w.t.Fatalf("station %d before its stream: %+v, %v (want never seen)", sid, st, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/events?station=%d&topics=bins", w.srv.URL, sid), nil)
	if err != nil {
		w.t.Fatalf("stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("open a station's stream: %v", err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	for sc.Scan() && sc.Text() != "event: connected" {
	}
	st, err := w.db.GetOperatorStation(sid)
	if err != nil || st.LastSeenAt == nil || st.HealthStatus != "online" {
		w.t.Fatalf("station %d after its stream connected: %+v, %v (want online, seen)", sid, st, err)
	}
}

func (w *walk) styleName(id int64) string {
	s, err := w.db.GetStyle(id)
	if err != nil || s == nil {
		return ""
	}
	return s.Name
}

func (w *walk) login() *http.Cookie {
	w.t.Helper()
	form := url.Values{"username": {"walk"}, "password": {"walk-password"}}
	req, err := http.NewRequest(http.MethodPost, w.srv.URL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		w.t.Fatalf("login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		w.t.Fatalf("login: %v", err)
	}
	res.Body.Close()
	for _, c := range res.Cookies() {
		return c
	}
	w.t.Fatalf("login set no cookie (status %d)", res.StatusCode)
	return nil
}

// walkLoaders is Core's loader list: the walk's loader board, and a second
// loader with no screen on this Edge (what POST /api/loader-boards boards).
func walkLoaders(replenishment string) []protocol.LoaderInfo {
	return []protocol.LoaderInfo{{
		Name: "Walk loader", LoaderKey: "loader:walk", Role: "produce", Layout: "shared_window",
		Replenishment: replenishment, OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "SMN_W1", Ordinal: 1}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-W"}},
	}, {
		Name: "Walk loader two", LoaderKey: "loader:walk-2", Role: "produce", Layout: "shared_window",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
		Positions: []protocol.LoaderPosition{{CoreNodeName: "SMN_W2", Ordinal: 1}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-W"}},
	}}
}

// coreEnv is an envelope from Core to this Edge.
func (w *walk) coreEnv(typ string, payload any) *protocol.Envelope {
	w.t.Helper()
	env, err := protocol.NewEnvelope(typ, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "walk.edge"}, payload)
	if err != nil {
		w.t.Fatalf("envelope %s: %v", typ, err)
	}
	return env
}

func (w *walk) dataEnv(subject string, body any) *protocol.Envelope {
	w.t.Helper()
	env, err := protocol.NewDataEnvelope(subject, protocol.Address{Role: protocol.RoleCore},
		protocol.Address{Role: protocol.RoleEdge, Station: "walk.edge"}, body)
	if err != nil {
		w.t.Fatalf("data envelope %s: %v", subject, err)
	}
	return env
}

func id(v int64) string { return fmt.Sprint(v) }

// api is a fixture's own request, made before the door runs: it answers the
// status and the decoded body, and fails the test on a non-2xx.
func (w *walk) api(method, path string, body any) map[string]any {
	w.t.Helper()
	w.prepped = true
	raw, err := json.Marshal(body)
	if err != nil {
		w.t.Fatalf("prep %s %s: %v", method, path, err)
	}
	req, err := http.NewRequest(method, w.srv.URL+path, bytes.NewReader(raw))
	if err != nil {
		w.t.Fatalf("request %s %s: %v", method, path, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(w.cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		w.t.Fatalf("prep %s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	out, err := io.ReadAll(res.Body)
	if err != nil {
		w.t.Fatalf("read %s %s: %v", method, path, err)
	}
	if res.StatusCode/100 != 2 {
		w.t.Fatalf("prep %s %s: %d %s", method, path, res.StatusCode, out)
	}
	var m map[string]any
	_ = json.Unmarshal(out, &m)
	return m
}

// num reads an id out of a decoded body.
func num(w *walk, m map[string]any, key string) int64 {
	w.t.Helper()
	v, ok := m[key].(float64)
	if !ok {
		w.t.Fatalf("no %q in %v", key, m)
	}
	return int64(v)
}
