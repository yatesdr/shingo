package engine

// release_harness_test.go — the release characterisation harness.
//
// The release doors are characterised here the way a plant meets them: orders
// built by the real step builders (RequestProduceSwap, RequestNodeMaterial,
// StartProcessChangeover), a runtime row stamped as a node that has completed
// an order, the production uop mutator writing real piles and real
// capture_reduction deltas into the outbox, and Core's replies delivered through
// the real EdgeHandler. A door is then driven and ONE outcome string records
// everything that changed: the verdict, each leg's status, which legs got a
// release envelope, and the paperwork that shipped.
//
// ── WHY ONE STRING PER CELL ───────────────────────────────────────────────
//
// Every later step of the release work must be able to say "this cell changed,
// and here is the ruling or the bug that changed it; nothing else did". A cell
// that asserts one field at a time can move in a field nobody asserted. The
// outcome string is the whole observable, so a diff anywhere in it is a diff
// in the test.
//
// ── KNOWN-BAD CELLS (pinOutcome) ──────────────────────────────────────────
//
// The harness lands green at the base. A cell whose behaviour is a known defect
// records today's outcome and names the bug; the commit that fixes it deletes
// the tag, and from then on the cell asserts the corrected outcome. So a fix
// flips exactly the cells that name it, and the diff says which.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/messaging"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/release"
	"shingoedge/store"
	storemsg "shingoedge/store/messaging"
	storeorders "shingoedge/store/orders"
	"shingoedge/uop"
)

// pinOutcome asserts one characterised outcome. While bug names a known defect,
// the cell holds today's outcome and says what it should be; the fix commit
// deletes the tag, and from then on the cell asserts want.
func pinOutcome(t *testing.T, bug, got, today, want string) {
	t.Helper()
	if bug != "" {
		if got != today {
			t.Fatalf("bug:%s characterisation moved:\n got   %q\n today %q\n want  %q (after the fix)", bug, got, today, want)
		}
		t.Logf("bug:%s RED as expected:\n got  %q\n want %q", bug, got, want)
		return
	}
	if got != want {
		t.Fatalf("\n got  %q\n want %q", got, want)
	}
}

// ── Fake Core (HTTP) ─────────────────────────────────────────────────────

// fakeCore answers the Core HTTP reads a release makes and counts them per
// path. node-bins (BinAtLineside) answers with binAt when set; everything else
// is a 404, which every Core read on the release path treats as "no answer".
type fakeCore struct {
	srv   *httptest.Server
	mu    sync.Mutex
	calls map[string]int
	binAt *NodeBinInfo
	// binTypes answers the payload manifest's bin_type_code per payload — what
	// the changeover planner reads to decide a press-index fan-out.
	binTypes map[string]string
	// points answers releasePoints (release_fake_points_test.go); pointsDown
	// makes Core unreachable for it (G3).
	points     func(protocol.ReleasePointsRequest) protocol.ReleasePointsResponse
	pointsDown bool
}

func newFakeCore(t *testing.T) *fakeCore {
	t.Helper()
	fc := &fakeCore{calls: map[string]int{}}
	fc.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fc.mu.Lock()
		fc.calls[r.URL.Path]++
		bin := fc.binAt
		binTypes := fc.binTypes
		points, down := fc.points, fc.pointsDown
		fc.mu.Unlock()
		if r.URL.Path == "/api/release/points" {
			if down || points == nil {
				http.Error(w, "core unreachable", http.StatusServiceUnavailable)
				return
			}
			var req protocol.ReleasePointsRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(points(req))
			return
		}
		if r.URL.Path == "/api/telemetry/node-bins" && bin != nil {
			_ = json.NewEncoder(w).Encode([]NodeBinInfo{*bin})
			return
		}
		if payload, ok := strings.CutPrefix(r.URL.Path, "/api/telemetry/payload/"); ok && binTypes != nil {
			payload = strings.TrimSuffix(payload, "/manifest")
			if bt := binTypes[payload]; bt != "" {
				_ = json.NewEncoder(w).Encode(PayloadManifestResponse{UOPCapacity: 100, BinTypeCode: bt})
				return
			}
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(fc.srv.Close)
	return fc
}

// total is every Core HTTP round trip since the last reset.
func (fc *fakeCore) total() int {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	n := 0
	for _, c := range fc.calls {
		n += c
	}
	return n
}

func (fc *fakeCore) reset() {
	fc.mu.Lock()
	fc.calls = map[string]int{}
	fc.mu.Unlock()
}

// ── Scripted WarLink (the curtain tag) ───────────────────────────────────

// scriptedWarLink answers every direct tag read with the current value (or the
// scripted sequence, when one is set) and counts the reads. The curtain gate
// reads DIRECTLY, so the count is the number of WarLink round trips a door made.
type scriptedWarLink struct {
	curtainStubClient
	mu    sync.Mutex
	value any
	err   error
	seq   []any
	reads int
}

func (s *scriptedWarLink) ReadTagValue(ctx context.Context, plcName, tagName string) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reads++
	if s.err != nil {
		return nil, s.err
	}
	if len(s.seq) > 0 {
		i := s.reads - 1
		if i >= len(s.seq) {
			i = len(s.seq) - 1
		}
		return s.seq[i], nil
	}
	return s.value, nil
}

func (s *scriptedWarLink) set(v any) {
	s.mu.Lock()
	s.value, s.err, s.seq = v, nil, nil
	s.mu.Unlock()
}

func (s *scriptedWarLink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reads
}

func (s *scriptedWarLink) reset() {
	s.mu.Lock()
	s.reads = 0
	s.mu.Unlock()
}

// Curtain tag values. The plant polarity is FALSE = the release state (the
// bypass button is held); TRUE = the curtain is live.
const (
	curtainSafe = false
	curtainLive = true
)

// ── The harness ──────────────────────────────────────────────────────────

// relHarness is one engine under characterisation plus everything needed to
// observe it.
type relHarness struct {
	t       *testing.T
	db      *store.DB
	eng     *Engine
	counter *store.QueryCounter
	core    *fakeCore
	wl      *scriptedWarLink
	handler *messaging.EdgeHandler
	mutator *uop.Mutator

	processID int64
	nodeID    int64
	stationID int64 // the operator station the fixture's front node sits on
	coID      int64 // the changeover a fixture started, when it started one
	partnerID int64 // a second node the fixture built (the A/B partner, the unchanged neighbour)

	legs  []relLeg
	extra []string // outcome fields a cell adds (task state, pile, ...)

	// lifted: nodes a lift was reported at (h.pickedUp), which no longer hold
	// the bin the fake Core's lift dependencies ask about.
	lifted map[string]bool
	// coreReads / coreWrites: statements the fake Core issued against the
	// shared database while standing in for Core, which are Core's cost and
	// not the Edge's. Its handler runs while the Edge waits on the HTTP call
	// over the one connection, so they never interleave and subtract exactly.
	coreReads, coreWrites int64

	outboxMark int64
}

// relLeg is one order under observation, by the name the outcome prints.
type relLeg struct {
	name string
	id   int64
}

// newRelHarness builds an engine wired the way production wires the release
// path: the order manager emits onto the engine's bus (so a Core reply that
// rolls a leg back to staged fires the same handlers it fires on a plant), the
// event handlers are subscribed, the uop mutator is the real one, Core's HTTP
// reads go to a counting fake, and the curtain tag is scripted.
func newRelHarness(t *testing.T) *relHarness {
	t.Helper()
	db, counter := testEngineDBCounting(t)
	eng := testEngine(t, db)
	eng.orderMgr = orders.NewManager(db, &orderEmitter{bus: eng.Events}, "test.station")
	eng.wireEventHandlers()

	mut := uop.New(db, "test.station", db, db)
	eng.SetInventoryDeltaSink(mut)

	fc := newFakeCore(t)
	// The fake Core always answers, so the act waits for its answer: a fake
	// slowed by a loaded machine must not read as "no point from Core" (G3). A
	// cell that wants G3 sets pointsDown, which answers 503 at once.
	eng.coreClient = stubCoreClient(fc.srv.URL)
	eng.points = nil // Core's points over HTTP, from the fake Core (counted)

	wl := &scriptedWarLink{value: curtainSafe}
	eng.plcMgr = plc.NewManager(nil, nil, nil, wl)

	h := &relHarness{
		t: t, db: db, eng: eng, counter: counter, core: fc, wl: wl,
		handler: newEdgeHandlerFor(eng),
		mutator: mut,
		lifted:  map[string]bool{},
	}
	fc.mu.Lock()
	fc.points = h.fakePoints
	fc.mu.Unlock()
	return h
}

// newEdgeHandlerFor is the production Core-reply handler over an engine's
// order manager.
func newEdgeHandlerFor(eng *Engine) *messaging.EdgeHandler {
	return messaging.NewEdgeHandler(eng.orderMgr)
}

// leg returns the id of the named leg.
func (h *relHarness) leg(name string) int64 {
	h.t.Helper()
	for _, l := range h.legs {
		if l.name == name {
			return l.id
		}
	}
	h.t.Fatalf("fixture has no leg %q", name)
	return 0
}

func (h *relHarness) addLeg(name string, id int64) {
	h.legs = append(h.legs, relLeg{name: name, id: id})
}

// setStatus forces a leg's row to a status, the way a fixture puts the world
// where the cell needs it. It writes the column directly: no event fires.
func (h *relHarness) setStatus(name string, s protocol.Status) {
	h.t.Helper()
	testutil.MustNoErr(h.t, h.db.UpdateOrderStatus(h.leg(name), string(s)), "set "+name+" "+string(s))
}

func (h *relHarness) order(name string) *storeorders.Order {
	h.t.Helper()
	o, err := h.db.GetOrder(h.leg(name))
	testutil.MustNoErr(h.t, err, "get "+name)
	return o
}

// armCurtain enables the front node's curtain interlock with the plant polarity
// and sets the tag's reading. The front node is the produce node, which is
// where the migration carried each process's live setting.
func (h *relHarness) armCurtain(reading bool) {
	h.t.Helper()
	safe := curtainSafe
	testutil.MustNoErr(h.t, h.db.SetProcessNodeCurtain(h.nodeID, true, "CURTAIN-PLC", "CURTAIN-TAG", &safe), "arm curtain")
	h.wl.set(reading)
}

// mark starts an act: every counter and the outbox watermark reset, so the
// outcome and the counts are the act's and not the fixture's.
func (h *relHarness) mark() {
	h.t.Helper()
	msgs, err := h.db.ListPendingOutbox(10000)
	testutil.MustNoErr(h.t, err, "ListPendingOutbox")
	h.outboxMark = 0
	for _, m := range msgs {
		if m.ID > h.outboxMark {
			h.outboxMark = m.ID
		}
	}
	h.counter.Reset()
	h.coreReads, h.coreWrites = 0, 0
	h.core.reset()
	h.wl.reset()
}

// sinceMark returns the outbox rows enqueued since mark().
func (h *relHarness) sinceMark() []storemsg.Message {
	h.t.Helper()
	msgs, err := h.db.ListPendingOutbox(10000)
	testutil.MustNoErr(h.t, err, "ListPendingOutbox")
	var out []storemsg.Message
	for _, m := range msgs {
		if m.ID > h.outboxMark {
			out = append(out, m)
		}
	}
	return out
}

// releasedSinceMark names the legs that got an OrderRelease envelope since
// mark(), in enqueue order, with a count when one got more than one.
func (h *relHarness) releasedSinceMark() []string {
	h.t.Helper()
	byUUID := map[string]string{}
	for _, l := range h.legs {
		o, err := h.db.GetOrder(l.id)
		testutil.MustNoErr(h.t, err, "get "+l.name)
		byUUID[o.UUID] = l.name
	}
	var names []string
	for _, m := range h.sinceMark() {
		if m.MsgType != protocol.TypeOrderRelease {
			continue
		}
		rel := decodeOrderRelease(h.t, m)
		name := byUUID[rel.OrderUUID]
		if name == "" {
			name = "?" + rel.OrderUUID
		}
		names = append(names, name)
	}
	return names
}

// releaseEnvelopes returns the OrderRelease payloads since mark(), keyed by leg.
func (h *relHarness) releaseEnvelopes() map[string][]protocol.OrderRelease {
	h.t.Helper()
	byUUID := map[string]string{}
	for _, l := range h.legs {
		o, err := h.db.GetOrder(l.id)
		testutil.MustNoErr(h.t, err, "get "+l.name)
		byUUID[o.UUID] = l.name
	}
	out := map[string][]protocol.OrderRelease{}
	for _, m := range h.sinceMark() {
		if m.MsgType != protocol.TypeOrderRelease {
			continue
		}
		rel := decodeOrderRelease(h.t, m)
		out[byUUID[rel.OrderUUID]] = append(out[byUUID[rel.OrderUUID]], rel)
	}
	return out
}

// paperworkSinceMark counts the paperwork that shipped since mark(): ingest
// manifests, and the sum of capture_reduction deltas (after a flush).
func (h *relHarness) paperworkSinceMark() (ingest int, capRed int) {
	h.t.Helper()
	h.mutator.Flush()
	for _, m := range h.sinceMark() {
		switch m.MsgType {
		case protocol.TypeOrderIngest:
			ingest++
		case protocol.SubjectBinUOPDelta:
			// A data envelope: the delta rides under p.data.
			var env struct {
				P struct {
					Data protocol.BinUOPDelta `json:"data"`
				} `json:"p"`
			}
			testutil.MustNoErr(h.t, json.Unmarshal(m.Payload, &env), "decode BinUOPDelta envelope")
			if env.P.Data.Reason == protocol.ReasonCaptureReduction {
				capRed += env.P.Data.Delta
			}
		}
	}
	return ingest, capRed
}

// pile is the node's active lineside pile quantity for a part.
func (h *relHarness) pile(part string) int {
	h.t.Helper()
	bs, err := h.db.ListLinesideBuckets(h.nodeID)
	testutil.MustNoErr(h.t, err, "list piles")
	n := 0
	for _, b := range bs {
		if b.PayloadCode == part {
			n += b.Qty
		}
	}
	return n
}

// errClass reduces a door's error to the verdict the outcome prints.
func errClass(err error) string {
	if err == nil {
		return "ok"
	}
	var held *release.HeldError
	if errors.As(err, &held) {
		return "hold:" + map[string]string{release.G1: "wait", release.G3: "core", release.G6: "curtain", release.G7: "lift"}[held.Gate]
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "light curtain"), strings.Contains(msg, "curtain"):
		return "refuse:curtain"
	case strings.Contains(msg, "would land on the carrier still on"):
		return "refuse:outgoing-carrier"
	case strings.Contains(msg, "line is pulling"), strings.Contains(msg, "whether the line is pulling"):
		return "refuse:pull"
	case strings.Contains(msg, "which Core will not release"):
		return "refuse:not-releasable"
	case strings.Contains(msg, "no active claim"):
		return "refuse:no-claim"
	case strings.Contains(msg, "supply-leg check"):
		return "refuse:unclassifiable"
	case strings.Contains(msg, "already on its way"):
		return "refuse:in-flight"
	case strings.Contains(msg, "not a coordinated pair"), strings.Contains(msg, "no tracked orders"):
		return "refuse:no-pair"
	}
	if len(msg) > 48 {
		msg = msg[:48]
	}
	return "err:" + msg
}

// outcome renders everything the act changed as one string:
//
//	<verdict> | <leg>=<status> ... | rel=<legs> | ingest=N capred=N [extra...]
func (h *relHarness) outcome(err error) string {
	h.t.Helper()
	var legs []string
	for _, l := range h.legs {
		o, gerr := h.db.GetOrder(l.id)
		testutil.MustNoErr(h.t, gerr, "get "+l.name)
		legs = append(legs, l.name+"="+string(o.Status))
	}
	rel := h.releasedSinceMark()
	relStr := "-"
	if len(rel) > 0 {
		relStr = strings.Join(rel, ",")
	}
	ingest, capRed := h.paperworkSinceMark()
	parts := []string{
		errClass(err),
		strings.Join(legs, " "),
		"rel=" + relStr,
		fmt.Sprintf("ingest=%d capred=%d", ingest, capRed),
	}
	parts = append(parts, h.extra...)
	return strings.Join(parts, " | ")
}

// ── Core's replies, through the real EdgeHandler ─────────────────────────

// coreRefuses delivers Core's error reply for a leg's release — the stubs for
// invalid_state, manifest_sync_failed, fleet_failed and internal_error.
func (h *relHarness) coreRefuses(name, code string) {
	h.t.Helper()
	o := h.order(name)
	h.handler.HandleOrderError(&protocol.Envelope{}, &protocol.OrderError{
		OrderUUID: o.UUID, ErrorCode: code, Detail: "stub: Core refused (" + code + ")",
	})
}

// coreStages delivers Core's OrderStaged push for a leg: its robot parked at a
// wait. This is what fires the staged transition on a plant.
func (h *relHarness) coreStages(name string) {
	h.t.Helper()
	o := h.order(name)
	h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: o.UUID, Detail: "stub: parked"})
}

// coreStagesAt is coreStages from a Core that numbers its station waits: the
// leg is parked at station wait ordinal.
func (h *relHarness) coreStagesAt(name string, ordinal int) {
	h.t.Helper()
	o := h.order(name)
	n := ordinal
	h.handler.HandleOrderStaged(&protocol.Envelope{}, &protocol.OrderStaged{OrderUUID: o.UUID, Detail: "stub: parked",
		StationWait: &n, WaitKind: protocol.WaitKindStation})
}

// coreConfirms drives a leg to confirmed through the lifecycle, firing the
// terminal transition that wakes its sibling's held intent.
func (h *relHarness) coreConfirms(name string) {
	h.t.Helper()
	testutil.MustNoErr(h.t, h.eng.orderMgr.TransitionOrder(h.leg(name), protocol.StatusDelivered, "stub: delivered"), "deliver "+name)
	testutil.MustNoErr(h.t, h.eng.orderMgr.TransitionOrder(h.leg(name), protocol.StatusConfirmed, "stub: confirmed"), "confirm "+name)
}

// ── Counts ───────────────────────────────────────────────────────────────

// actCounts is the cost of one act: SQLite statements (reads and writes),
// direct WarLink reads, and Core HTTP round trips. Envelopes are counted
// separately by the outcome.
type actCounts struct {
	reads, writes int64
	warlink       int
	coreHTTP      int
}

func (h *relHarness) counts() actCounts {
	return actCounts{
		reads:    h.counter.Reads() - h.coreReads,
		writes:   h.counter.Writes() - h.coreWrites,
		warlink:  h.wl.count(),
		coreHTTP: h.core.total(),
	}
}

func (c actCounts) String() string {
	return fmt.Sprintf("sqlite reads=%d writes=%d warlink=%d coreHTTP=%d", c.reads, c.writes, c.warlink, c.coreHTTP)
}
