//go:build docker

package engine

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/fleet/simulator"
	"shingocore/material"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// The whole pipe, end to end: a bin crosses a tagged boundary and a POST
// arrives at a middleware stub carrying the right number.
//
// The unit tests in this project each hold one seam still and check the other
// side of it. This one holds none: it drives a real engine with a real database
// through the event bus, the builder, the queue, the translator and the client,
// and asserts on the bytes a middleware would receive. Every seam between those
// is somewhere the pieces could each be right and the assembly wrong.

// middlewareStub records what it was sent and answers as instructed.
type middlewareStub struct {
	mu       sync.Mutex
	bodies   [][]byte
	status   int
	response string
	srv      *httptest.Server
}

func newMiddlewareStub(t *testing.T, status int, response string) *middlewareStub {
	return newMiddlewareStubWith(t, status, response, nil)
}

// newMiddlewareStubWith builds the stub with an optional replacement handler,
// supplied AT CONSTRUCTION.
//
// The handler cannot be swapped afterwards. Reassigning srv.Config.Handler on a
// running httptest.Server is documented as unsupported — the server has already
// captured it — and it is a real data race between the test goroutine and any
// in-flight request, not a theoretical one. A test that needs a hostile
// middleware asks for one here instead.
func newMiddlewareStubWith(t *testing.T, status int, response string, h http.HandlerFunc) *middlewareStub {
	t.Helper()
	m := &middlewareStub{status: status, response: response}
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			// io.ReadAll, not one Read. A single Read is permitted to return
			// fewer bytes than are available, so this recorded a truncated
			// body whenever the runtime felt like it — and every assertion
			// downstream is on that recording. It works today only because the
			// bodies are small.
			body, _ := io.ReadAll(r.Body)
			m.mu.Lock()
			m.bodies = append(m.bodies, body)
			st, resp := m.status, m.response
			m.mu.Unlock()

			w.WriteHeader(st)
			_, _ = w.Write([]byte(resp))
		}
	}
	m.srv = httptest.NewServer(h)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *middlewareStub) received() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([][]byte, len(m.bodies))
	copy(out, m.bodies)
	return out
}

// awaitPostingSettled blocks until the newest posting reaches a terminal status,
// and returns it.
//
// THE TEST IS NOT THE ONLY DRAINER. eng.Start() runs the poster's own loop, and
// Enqueue rings its doorbell — so the moment the subscriber queues a posting,
// that loop begins draining it concurrently with the explicit DrainOnce below.
// MarkInflight is guarded on status='pending', so exactly one of them sends and
// nothing is double-posted; but whichever one loses returns immediately, and a
// test that asserts right afterwards is reading a row the winner is still
// working on.
//
// That race predates this file's current shape and could not manifest while
// ClassPosted was a single UPDATE: the row went pending → inflight → posted with
// no observable middle. Writing the transaction id BEFORE the status (so a
// failed settle leaves something the reconciler can resolve) opened a window
// where `inflight` with a real id is a state a reader can catch, and this test
// caught it — intermittently, which is the worst way to find out.
//
// Waiting on the end state is the honest fix. A poster that genuinely never
// settles still fails here, by timeout, with the last status it had.
func awaitPostingSettled(t *testing.T, db *store.DB) (status, txID, lastErr string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		if err := db.QueryRow(`SELECT status, transaction_id, last_error
			FROM cms_postings ORDER BY id DESC LIMIT 1`).Scan(&status, &txID, &lastErr); err != nil {
			t.Fatalf("read posting: %v", err)
		}
		switch status {
		case "posted", "rejected", "failed":
			return status, txID, lastErr
		}
		if time.Now().After(deadline) {
			t.Fatalf("the posting never settled: status=%q transaction_id=%q last_error=%q",
				status, txID, lastErr)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// awaitRequests blocks until the stub has received at least n requests, and
// returns everything it has. Same race as awaitPostingSettled: the send may be
// on the engine's own poster goroutine.
func awaitRequests(t *testing.T, stub *middlewareStub, n int) [][]byte {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		got := stub.received()
		if len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("the middleware received %d requests, want %d", len(got), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// cmsEngine builds an engine with the CMS block pointed at the stub.
func cmsEngine(t *testing.T, db *store.DB, baseURL string) *Engine {
	t.Helper()
	// The block goes in BEFORE New, because New is where the poster is built.
	eng := newUnstartedEngineWith(t, db, simulator.New(), func(cfg *config.Config) {
		cfg.CMS = config.CMSConfig{
			BaseURL: baseURL, AccessKey: "AK-E2E", SecretKey: "SK-E2E",
			Timeout: 5 * time.Second, PollInterval: time.Hour, SettleWindow: time.Hour,
			MaxAttempts: 3,
			ReasonCode:  "TEST-AMR", IncreaseType: "I", DecreaseType: "D",
			UnitOfMeasure: "EA", UserID: "SHINGO",
		}
	})
	eng.Start()
	t.Cleanup(eng.Stop)
	return eng
}

// tagBoundary makes a synthetic root a CMS boundary with a storeroom code and
// returns it plus a child slot.
func tagBoundary(t *testing.T, db *store.DB, name, storeroom string) (*nodes.Node, *nodes.Node) {
	t.Helper()
	root := &nodes.Node{Name: name + "-ROOT", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(root), "create boundary root")
	testutil.MustNoErr(t, db.SetNodeProperty(root.ID, material.CMSStoreroomProperty, storeroom), "tag boundary")
	slot := &nodes.Node{Name: name + "-SLOT", Enabled: true, ParentID: &root.ID}
	testutil.MustNoErr(t, db.CreateNode(slot), "create slot")
	root, _ = db.GetNode(root.ID)
	slot, _ = db.GetNode(slot.ID)
	return root, slot
}

// TestCMSEndToEnd_APartialBinShipsItsACTUALCount is the project's headline
// correctness claim, checked on the wire.
//
// A bin loaded from a 24-cycle template holding 8 cycles of a part consumed 2
// per cycle holds SIXTEEN parts. Before this work the manifest carried the
// template's full-bin nominal, nothing rewrote it as production drew the bin
// down, and CMS would have been told 48 (the nominal) — or, had the rename
// landed without its backfill, 24 x 48.
func TestCMSEndToEnd_APartialBinShipsItsACTUALCount(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-E2E-1"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	// A payload whose bin holds 24 cycles, at 2 parts of PART-X per cycle.
	pay := &payloads.Payload{Code: "E2E-PAYLOAD", UOPCapacity: 24}
	testutil.MustNoErr(t, db.CreatePayload(pay), "create payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pay.ID, PartNumber: "PART-X", PartsPerCycle: 2,
	}), "create template line")

	_, srcSlot := tagBoundary(t, db, "E2E-SUPERMARKET", "SM01")
	_, dstSlot := tagBoundary(t, db, "E2E-LINE", "MAN")

	bin := createTestBinAtNode(t, db, pay.Code, srcSlot.ID, "BIN-E2E")
	// PARTIALLY DRAWN DOWN: 8 of the 24 cycles left.
	m := bins.Manifest{Items: []bins.ManifestEntry{{CatID: "PART-X"}}}
	body, _ := json.Marshal(m)
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, string(body), pay.Code, 8), "set manifest")

	eng.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		Action: "moved", BinID: bin.ID, PayloadCode: pay.Code,
		FromNodeID: srcSlot.ID, ToNodeID: dstSlot.ID, NodeID: dstSlot.ID,
		RobotID: "AMR-042", OrderID: 0,
	}})

	// The subscriber queues; the poster sends on its own loop. Drain it here
	// rather than waiting on the ticker.
	if eng.cmsPoster == nil {
		t.Fatal("no poster was started for a configured cms: block")
	}
	eng.cmsPoster.DrainOnce(t.Context())

	got := awaitRequests(t, stub, 1)
	if len(got) != 1 {
		t.Fatalf("the middleware received %d requests, want 1 — a second send means two "+
			"drains both got past MarkInflight, which double-books the transfer", len(got))
	}
	var rows []map[string]any
	if err := json.Unmarshal(got[0], &rows); err != nil {
		t.Fatalf("body is not a JSON array: %v (%s)", err, got[0])
	}
	if len(rows) != 2 {
		t.Fatalf("body carries %d rows, want 2 (one per boundary): %s", len(rows), got[0])
	}

	byLocation := map[string]map[string]any{}
	for _, r := range rows {
		byLocation[r["StockLocation"].(string)] = r
	}
	departure, ok := byLocation["SM01"]
	if !ok {
		t.Fatalf("no departure row for SM01: %s", got[0])
	}
	arrival, ok := byLocation["MAN"]
	if !ok {
		t.Fatalf("no arrival row for MAN: %s", got[0])
	}

	// THE NUMBER. 8 cycles x 2 parts per cycle.
	if departure["Quantity"] != float64(16) || arrival["Quantity"] != float64(16) {
		t.Errorf("quantities = %v / %v, want 16 and 16 — a bin holding 8 of a 24-cycle "+
			"template at 2 per cycle holds 16 parts, not the template's nominal: %s",
			departure["Quantity"], arrival["Quantity"], got[0])
	}
	// Direction is in the type; the quantity is unsigned.
	if departure["TransactionType"] != "D" || arrival["TransactionType"] != "I" {
		t.Errorf("types = %v / %v, want D and I", departure["TransactionType"], arrival["TransactionType"])
	}
	if departure["PartNumber"] != "PART-X" {
		t.Errorf("part number = %v, want PART-X", departure["PartNumber"])
	}
	if departure["Resource"] != "AMR-042" {
		t.Errorf("resource = %v, want AMR-042 — the robot that carried it", departure["Resource"])
	}
	if departure["ReasonCode"] != "TEST-AMR" || departure["UserId"] != "SHINGO" {
		t.Errorf("configured vocabulary did not reach the wire: %s", got[0])
	}

	// The posting settled with the id the middleware named.
	status, txID, _ := awaitPostingSettled(t, db)
	if status != "posted" || txID != "MW-E2E-1" {
		t.Errorf("posting = %s / %q, want posted with the middleware's id", status, txID)
	}

	// And the health surface agrees the feed works.
	health, err := eng.CMSFeedHealth()
	if err != nil {
		t.Fatalf("CMSFeedHealth: %v", err)
	}
	if !health.Healthy {
		t.Errorf("the feed reports unhealthy after a successful post: %s", health.Why)
	}
}

// TestCMSEndToEnd_RefusalIsTerminalAndCarriesNoCredentials covers the 4xx
// drill: the posting stops, and what it records about why must be safe to read
// on a page.
func TestCMSEndToEnd_RefusalIsTerminalAndCarriesNoCredentials(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	// A hostile refusal: the middleware echoes the credentials it received.
	// Supplied at construction — see newMiddlewareStubWith.
	var stub *middlewareStub
	stub = newMiddlewareStubWith(t, http.StatusBadRequest, "",
		func(w http.ResponseWriter, r *http.Request) {
			stub.mu.Lock()
			stub.bodies = append(stub.bodies, []byte("x"))
			stub.mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte("rejected: key=" + r.Header.Get("x-access-key") +
				" secret=" + r.Header.Get("x-secret-key")))
		})
	eng := cmsEngine(t, db, stub.srv.URL)

	pay := &payloads.Payload{Code: "E2E-REJECT", UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(pay), "create payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pay.ID, PartNumber: "PART-R", PartsPerCycle: 1,
	}), "create template line")

	_, srcSlot := tagBoundary(t, db, "E2E-REJ-SRC", "SM02")
	_, dstSlot := tagBoundary(t, db, "E2E-REJ-DST", "DOCK")

	bin := createTestBinAtNode(t, db, pay.Code, srcSlot.ID, "BIN-E2E-REJ")
	m := bins.Manifest{Items: []bins.ManifestEntry{{CatID: "PART-R"}}}
	body, _ := json.Marshal(m)
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, string(body), pay.Code, 3), "set manifest")

	eng.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		Action: "moved", BinID: bin.ID, PayloadCode: pay.Code,
		FromNodeID: srcSlot.ID, ToNodeID: dstSlot.ID, NodeID: dstSlot.ID,
	}})
	eng.cmsPoster.DrainOnce(t.Context())

	status, _, lastErr := awaitPostingSettled(t, db)
	if status != "rejected" {
		t.Errorf("status = %q, want rejected — a refused body will be refused again", status)
	}
	if strings.Contains(lastErr, "AK-E2E") || strings.Contains(lastErr, "SK-E2E") {
		t.Errorf("last_error carries the credentials, and it is read on a diagnostics page: %s", lastErr)
	}
	if !strings.Contains(lastErr, "400") {
		t.Errorf("last_error = %q, want it to name the status a person can look up", lastErr)
	}

	// Draining again must not re-send: the refusal is terminal. Safe to read
	// the count directly here — awaitPostingSettled has already established
	// that no drain is still in flight.
	before := len(stub.received())
	eng.cmsPoster.DrainOnce(t.Context())
	if after := len(stub.received()); after != before {
		t.Errorf("a rejected posting was sent again (%d -> %d)", before, after)
	}

	// And the feed says so rather than looking quiet.
	health, err := eng.CMSFeedHealth()
	if err != nil {
		t.Fatalf("CMSFeedHealth: %v", err)
	}
	if health.Healthy {
		t.Error("the feed reports healthy with a rejected posting in the table")
	}
	if !strings.Contains(health.Why, "refused") {
		t.Errorf("why = %q, want it to name the refusal", health.Why)
	}
}

// TestCMSEndToEnd_UntaggedBoundariesPostNothing is the fail-closed claim at
// full scale. Springfield runs with no cms_storeroom anywhere; a move there
// must produce no transaction, no posting, and no request — and the health
// surface must say WHY rather than reporting a quiet feed as fine.
func TestCMSEndToEnd_UntaggedBoundariesPostNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	stub := newMiddlewareStub(t, http.StatusOK, `{"TransactionId":"MW-SHOULD-NOT-HAPPEN"}`)
	eng := cmsEngine(t, db, stub.srv.URL)

	pay := &payloads.Payload{Code: "E2E-UNTAGGED", UOPCapacity: 10}
	testutil.MustNoErr(t, db.CreatePayload(pay), "create payload")
	testutil.MustNoErr(t, db.CreatePayloadManifestItem(&payloads.ManifestItem{
		PayloadID: pay.ID, PartNumber: "PART-U", PartsPerCycle: 1,
	}), "create template line")

	// Synthetic roots with NO cms_storeroom — the old predicate made exactly
	// these boundaries by default, so a robot pickup booked a storeroom
	// transfer into "the robot".
	src := &nodes.Node{Name: "E2E-UNTAGGED-A", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(src), "create src")
	dst := &nodes.Node{Name: "_ROBOT:AMR-099", IsSynthetic: true, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(dst), "create carrier node")

	bin := createTestBinAtNode(t, db, pay.Code, src.ID, "BIN-E2E-UNTAGGED")
	m := bins.Manifest{Items: []bins.ManifestEntry{{CatID: "PART-U"}}}
	body, _ := json.Marshal(m)
	testutil.MustNoErr(t, db.SetBinManifest(bin.ID, string(body), pay.Code, 5), "set manifest")

	eng.Events.Emit(Event{Type: EventBinUpdated, Payload: BinUpdatedEvent{
		Action: "moved", BinID: bin.ID, PayloadCode: pay.Code,
		FromNodeID: src.ID, ToNodeID: dst.ID, NodeID: dst.ID,
	}})
	eng.cmsPoster.DrainOnce(t.Context())

	if n := len(stub.received()); n != 0 {
		t.Errorf("the middleware received %d requests for a move between untagged nodes, want 0", n)
	}
	var txns, postings int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM cms_transactions`).Scan(&txns), "count txns")
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM cms_postings`).Scan(&postings), "count postings")
	if txns != 0 || postings != 0 {
		t.Errorf("untagged move produced %d transactions and %d postings, want 0 and 0", txns, postings)
	}

	health, err := eng.CMSFeedHealth()
	if err != nil {
		t.Fatalf("CMSFeedHealth: %v", err)
	}
	if health.Healthy {
		t.Error("a feed with nothing tagged reported healthy — that is the zero-pending inference")
	}
	if !strings.Contains(health.Why, "cms_storeroom") {
		t.Errorf("why = %q, want it to say no node is tagged", health.Why)
	}
}
