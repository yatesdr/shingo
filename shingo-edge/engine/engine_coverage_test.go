package engine

// engine_coverage_test.go — coverage tests for the thin accessor/adapter/
// service layer of the Engine. These exercise the files that had zero
// coverage before PR 3.3:
//
//   engine.go              — accessors + Core node sync + func-injection
//                            + HandlePayloadCatalog + SendEnvelope +
//                            ReconnectKafka
//   adapters.go            — plcEmitter + orderEmitter branches
//   core_client.go         — CoreClient against httptest.NewServer
//   core_sync_service.go   — CoreSyncService StartupReconcile +
//                            RequestOrderStatusSync + HandleOrderStatusSnapshots
//   reconciliation.go      — thin Engine delegates
//   reconciliation_service.go — ReconciliationService delegates
//   countgroup_sender.go   — SendCountGroupAck
//
// The tests construct Engine and subsystem structs directly (injecting
// fields) rather than calling Start(); Start() wires PLC polling and
// WarLink and isn't safe to invoke from a unit test.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/orders"
	"shingoedge/store"
)

// ── Shared fixtures ─────────────────────────────────────────────────

// newCoverageDB opens a fresh SQLite DB for one test.
func newCoverageDB(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "engine-cov.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// newCoverageEngine builds an Engine wired with the minimum set of
// dependencies needed for the thin-layer tests — no Start() call.
func newCoverageEngine(t *testing.T) *Engine {
	t.Helper()
	db := newCoverageDB(t)
	cfg := &config.Config{
		Namespace: "test-ns",
		LineID:    "line-1",
	}
	eng := &Engine{
		cfg:      cfg,
		db:       db,
		Events:   NewEventBus(),
		stopChan: make(chan struct{}),
		logFn:    func(string, ...any) {},
	}
	eng.coreClient = NewCoreClient("")
	eng.reconciliation = newReconciliationService(eng.db)
	eng.coreSync = newCoreSyncService(eng)
	eng.orderMgr = orders.NewManager(db, testOrderEmitter{}, cfg.StationID())
	return eng
}

// ── engine.go accessors ─────────────────────────────────────────────

func TestEngine_Accessors(t *testing.T) {
	eng := newCoverageEngine(t)
	if eng.DB() == nil {
		t.Error("DB() returned nil")
	}
	if eng.CoreAPI() == nil {
		t.Error("CoreAPI() returned nil")
	}
	if eng.AppConfig() == nil {
		t.Error("AppConfig() returned nil")
	}
	if eng.AppConfig().Namespace != "test-ns" {
		t.Errorf("AppConfig().Namespace = %q, want %q", eng.AppConfig().Namespace, "test-ns")
	}
	eng.configPath = "/tmp/shingoedge.yaml"
	if got := eng.ConfigPath(); got != "/tmp/shingoedge.yaml" {
		t.Errorf("ConfigPath() = %q, want %q", got, "/tmp/shingoedge.yaml")
	}
	if eng.Reconciliation() == nil {
		t.Error("Reconciliation() returned nil")
	}
	if eng.CoreSync() == nil {
		t.Error("CoreSync() returned nil")
	}
	// PLCManager/OrderManager: PLC is nil (no Start), orderMgr was injected.
	if eng.OrderManager() == nil {
		t.Error("OrderManager() returned nil")
	}
	if eng.PLCManager() != nil {
		t.Error("PLCManager() should be nil before Start()")
	}
}

func TestEngine_UptimeAndStop(t *testing.T) {
	eng := newCoverageEngine(t)
	// startedAt is zero value until Start() runs, so Uptime() will be large.
	// Set it manually so Uptime returns something sane.
	eng.startedAt = time.Now().Add(-3 * time.Second)
	if got := eng.Uptime(); got < 2 || got > 10 {
		t.Errorf("Uptime() = %d, expected roughly 3 seconds", got)
	}
	// Stop() should be safe even with no PLC manager — it must not panic.
	eng.Stop()
	// Second call is idempotent (channel-already-closed branch).
	eng.Stop()
	select {
	case <-eng.stopChan:
		// expected: closed
	default:
		t.Error("stopChan should be closed after Stop()")
	}
}

// ── Core node sync ──────────────────────────────────────────────────

func TestEngine_SetCoreNodesEmitsEvent(t *testing.T) {
	eng := newCoverageEngine(t)
	var got CoreNodesUpdatedEvent
	var gotType EventType
	eng.Events.Subscribe(func(evt Event) {
		gotType = evt.Type
		got, _ = evt.Payload.(CoreNodesUpdatedEvent)
	})

	in := []protocol.NodeInfo{
		{Name: "N1", NodeType: "cell"},
		{Name: "N2", NodeType: "storage"},
	}
	eng.SetCoreNodes(in)

	if gotType != EventCoreNodesUpdated {
		t.Errorf("event type = %v, want EventCoreNodesUpdated", gotType)
	}
	if len(got.Nodes) != 2 {
		t.Fatalf("event payload had %d nodes, want 2", len(got.Nodes))
	}

	// CoreNodes returns a copy — mutating it must not affect the engine.
	out := eng.CoreNodes()
	if len(out) != 2 {
		t.Fatalf("CoreNodes() = %d entries, want 2", len(out))
	}
	if _, ok := out["N1"]; !ok {
		t.Error("CoreNodes() missing N1")
	}
	delete(out, "N1")
	if _, ok := eng.CoreNodes()["N1"]; !ok {
		t.Error("mutating returned map affected engine state")
	}
}

// ── Func injection ──────────────────────────────────────────────────

func TestEngine_RequestNodeSync(t *testing.T) {
	eng := newCoverageEngine(t)
	// No fn set → no-op, no panic.
	eng.RequestNodeSync()

	called := 0
	eng.SetNodeSyncFunc(func() { called++ })
	eng.RequestNodeSync()
	eng.RequestNodeSync()
	if called != 2 {
		t.Errorf("nodeSyncFn called %d times, want 2", called)
	}
}

func TestEngine_RequestCatalogSync(t *testing.T) {
	eng := newCoverageEngine(t)
	eng.RequestCatalogSync() // no fn set — no-op

	called := 0
	eng.SetCatalogSyncFunc(func() { called++ })
	eng.RequestCatalogSync()
	if called != 1 {
		t.Errorf("catalogSyncFn called %d times, want 1", called)
	}
}

func TestEngine_SendEnvelope(t *testing.T) {
	eng := newCoverageEngine(t)
	// No sendFn configured.
	err := eng.SendEnvelope(&protocol.Envelope{})
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("SendEnvelope with no fn: err=%v, want 'send function not configured'", err)
	}

	var captured *protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		captured = env
		return nil
	})
	sentinel := &protocol.Envelope{}
	if err := eng.SendEnvelope(sentinel); err != nil {
		t.Fatalf("SendEnvelope: %v", err)
	}
	if captured != sentinel {
		t.Error("sendFn did not receive the envelope we passed in")
	}
}

func TestEngine_ReconnectKafka(t *testing.T) {
	eng := newCoverageEngine(t)
	if err := eng.ReconnectKafka(); err == nil {
		t.Error("ReconnectKafka with no fn should error")
	}

	called := false
	eng.SetKafkaReconnectFunc(func() error {
		called = true
		return fmt.Errorf("boom")
	})
	err := eng.ReconnectKafka()
	if !called {
		t.Error("kafkaReconnFn was not called")
	}
	if err == nil || err.Error() != "boom" {
		t.Errorf("ReconnectKafka err = %v, want boom", err)
	}
}

// ── HandlePayloadCatalog ────────────────────────────────────────────

func TestEngine_HandlePayloadCatalog_UpsertsAndPrunes(t *testing.T) {
	eng := newCoverageEngine(t)

	// Seed an entry with ID=99 that should be pruned when Core's catalog
	// doesn't mention it.
	stale := &store.PayloadCatalogEntry{ID: 99, Name: "stale", Code: "STALE", UOPCapacity: 10}
	if err := eng.db.UpsertPayloadCatalog(stale); err != nil {
		t.Fatalf("seed stale: %v", err)
	}

	// Handle a catalog with two fresh entries (ID 1 and 2).
	entries := []protocol.CatalogPayloadInfo{
		{ID: 1, Name: "widget", Code: "WIDGET", Description: "a widget", UOPCapacity: 100},
		{ID: 2, Name: "gadget", Code: "GADGET", Description: "a gadget", UOPCapacity: 50},
	}
	eng.HandlePayloadCatalog(entries)

	list, err := eng.db.ListPayloadCatalog()
	if err != nil {
		t.Fatalf("list catalog: %v", err)
	}
	// Expect exactly the two fresh entries; stale row 99 should be pruned.
	byCode := map[string]*store.PayloadCatalogEntry{}
	for _, e := range list {
		byCode[e.Code] = e
	}
	if _, ok := byCode["STALE"]; ok {
		t.Error("STALE catalog entry should have been pruned")
	}
	if byCode["WIDGET"] == nil || byCode["WIDGET"].UOPCapacity != 100 {
		t.Errorf("WIDGET entry missing or wrong capacity: %+v", byCode["WIDGET"])
	}
	if byCode["GADGET"] == nil || byCode["GADGET"].UOPCapacity != 50 {
		t.Errorf("GADGET entry missing or wrong capacity: %+v", byCode["GADGET"])
	}
}

func TestEngine_HandlePayloadCatalog_EmptyIsSafe(t *testing.T) {
	eng := newCoverageEngine(t)
	// Seed three entries.
	for i := int64(1); i <= 3; i++ {
		eng.db.UpsertPayloadCatalog(&store.PayloadCatalogEntry{
			ID: i, Name: fmt.Sprintf("e%d", i), Code: fmt.Sprintf("E%d", i), UOPCapacity: 10,
		})
	}
	// Empty catalog from Core is treated as a safety no-op — entries remain.
	// (DeleteStalePayloadCatalogEntries bails on empty active list to avoid
	// wiping the whole catalog when Core returns an empty response in error.)
	eng.HandlePayloadCatalog(nil)
	list, err := eng.db.ListPayloadCatalog()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 3 {
		t.Errorf("empty-catalog safety: want 3 entries preserved, got %d", len(list))
	}
}

// ── adapters.go — plcEmitter branches ───────────────────────────────

func TestPlcEmitter_AllEvents(t *testing.T) {
	bus := NewEventBus()
	// Collect every event the emitter fires so we can assert on type + payload.
	received := map[EventType]any{}
	var mu sync.Mutex
	bus.Subscribe(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		received[evt.Type] = evt.Payload
	})

	em := &plcEmitter{bus: bus}
	em.EmitCounterRead(7, "plc-1", "tag-1", 42)
	em.EmitCounterDelta(7, 1, 2, 3, 42, "")
	em.EmitCounterAnomaly(100, 7, "plc-1", "tag-1", 40, 42, "jump")
	em.EmitPLCConnected("plc-1")
	em.EmitPLCDisconnected("plc-1", fmt.Errorf("timeout"))
	em.EmitPLCDisconnected("plc-2", nil) // nil-error branch
	em.EmitPLCHealthAlert("plc-1", "no ping")
	em.EmitPLCHealthRecover("plc-1")
	em.EmitCounterReadError(7, "plc-1", "tag-1", "bad tag")
	em.EmitWarLinkConnected()
	em.EmitWarLinkDisconnected(fmt.Errorf("socket closed"))
	em.EmitWarLinkDisconnected(nil) // nil-error branch

	wantTypes := []EventType{
		EventCounterRead, EventCounterDelta, EventCounterAnomaly,
		EventPLCConnected, EventPLCDisconnected,
		EventPLCHealthAlert, EventPLCHealthRecover,
		EventCounterReadError, EventWarLinkConnected, EventWarLinkDisconnected,
	}
	for _, tp := range wantTypes {
		if _, ok := received[tp]; !ok {
			t.Errorf("missing event %v", tp)
		}
	}

	// Spot-check payload content on a few events.
	if cr, ok := received[EventCounterRead].(CounterReadEvent); !ok {
		t.Error("CounterRead payload wrong type")
	} else if cr.Value != 42 || cr.PLCName != "plc-1" {
		t.Errorf("CounterRead payload = %+v", cr)
	}
	if ca, ok := received[EventCounterAnomaly].(CounterAnomalyEvent); !ok {
		t.Error("CounterAnomaly payload wrong type")
	} else if ca.AnomalyType != "jump" || ca.OldValue != 40 || ca.NewValue != 42 {
		t.Errorf("CounterAnomaly payload = %+v", ca)
	}
	if wl, ok := received[EventWarLinkDisconnected].(WarLinkEvent); !ok {
		t.Error("WarLinkDisconnected payload wrong type")
	} else if wl.Connected {
		t.Error("WarLinkDisconnected should set Connected=false")
	}
	// nil-error disconnect branch clears Error string.
	if pe, ok := received[EventPLCDisconnected].(PLCEvent); !ok {
		t.Error("PLCDisconnected payload wrong type")
	} else if pe.PLCName != "plc-2" || pe.Error != "" {
		t.Errorf("last PLCDisconnected should have been nil-err branch, got %+v", pe)
	}
}

// ── adapters.go — orderEmitter branches ─────────────────────────────

func TestOrderEmitter_AllEvents(t *testing.T) {
	bus := NewEventBus()
	received := map[EventType]any{}
	var mu sync.Mutex
	bus.Subscribe(func(evt Event) {
		mu.Lock()
		defer mu.Unlock()
		received[evt.Type] = evt.Payload
	})

	em := &orderEmitter{bus: bus}
	nodeID := int64(42)
	em.EmitOrderCreated(1, "uuid-1", "complex", nil, &nodeID)
	em.EmitOrderStatusChanged(1, "uuid-1", "complex", "planning", "dispatched", "eta", nil, &nodeID)
	em.EmitOrderCompleted(1, "uuid-1", "complex", nil, &nodeID)
	em.EmitOrderFailed(1, "uuid-1", "complex", "timeout")

	if _, ok := received[EventOrderCreated]; !ok {
		t.Error("missing EventOrderCreated")
	}
	if p, ok := received[EventOrderStatusChanged].(OrderStatusChangedEvent); !ok {
		t.Error("OrderStatusChanged payload wrong type")
	} else if p.OldStatus != "planning" || p.NewStatus != "dispatched" {
		t.Errorf("status-change payload = %+v", p)
	}
	if _, ok := received[EventOrderCompleted]; !ok {
		t.Error("missing EventOrderCompleted")
	}
	if p, ok := received[EventOrderFailed].(OrderFailedEvent); !ok {
		t.Error("OrderFailed payload wrong type")
	} else if p.Reason != "timeout" {
		t.Errorf("OrderFailed payload = %+v", p)
	}
}

// ── core_client.go ──────────────────────────────────────────────────

func TestCoreClient_AvailableAndSetBaseURL(t *testing.T) {
	c := NewCoreClient("")
	if c.Available() {
		t.Error("new empty-url client should not be Available")
	}
	c.SetBaseURL("http://example.com/")
	if !c.Available() {
		t.Error("after SetBaseURL, should be Available")
	}
	// Trailing slash should have been trimmed.
	if c.baseURL != "http://example.com" {
		t.Errorf("baseURL = %q, want http://example.com (no trailing slash)", c.baseURL)
	}
	c.SetBaseURL("") // reset → unavailable
	if c.Available() {
		t.Error("SetBaseURL(\"\") should disable the client")
	}
}

func TestCoreClient_NoBaseURL_GracefulDegrade(t *testing.T) {
	c := NewCoreClient("")

	// All telemetry reads should return (nil,nil) — not an error.
	if m, err := c.FetchPayloadManifest("WIDGET"); m != nil || err != nil {
		t.Errorf("FetchPayloadManifest with no base-url = (%v,%v), want (nil,nil)", m, err)
	}
	if ch, err := c.FetchNodeChildren("NGRP"); ch != nil || err != nil {
		t.Errorf("FetchNodeChildren with no base-url = (%v,%v), want (nil,nil)", ch, err)
	}
	if bins, err := c.FetchNodeBins([]string{"N1"}); bins != nil || err != nil {
		t.Errorf("FetchNodeBins with no base-url = (%v,%v), want (nil,nil)", bins, err)
	}

	// Writes should return errors when Core isn't configured.
	if _, err := c.LoadBin(&BinLoadRequest{NodeName: "N1"}); err == nil {
		t.Error("LoadBin with no base-url should error")
	}
	if err := c.ClearBin("N1"); err == nil {
		t.Error("ClearBin with no base-url should error")
	}
}

func TestCoreClient_FetchPayloadManifest_EmptyCode(t *testing.T) {
	// Even with a base URL, empty payload code short-circuits to (nil,nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("server should not be hit: %s", r.URL.Path)
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	m, err := c.FetchPayloadManifest("")
	if m != nil || err != nil {
		t.Errorf("empty code should short-circuit, got (%v,%v)", m, err)
	}
}

func TestCoreClient_FetchPayloadManifest_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/WIDGET/manifest") {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		resp := PayloadManifestResponse{
			UOPCapacity: 100,
			Items: []ManifestItem{
				{PartNumber: "P1", Quantity: 10, Description: "part one"},
			},
		}
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	c := NewCoreClient(srv.URL)
	m, err := c.FetchPayloadManifest("WIDGET")
	if err != nil || m == nil {
		t.Fatalf("FetchPayloadManifest: (%v,%v)", m, err)
	}
	if m.UOPCapacity != 100 || len(m.Items) != 1 || m.Items[0].PartNumber != "P1" {
		t.Errorf("manifest = %+v", m)
	}
}

func TestCoreClient_FetchPayloadManifest_404(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	m, err := c.FetchPayloadManifest("UNKNOWN")
	// 404 must graceful-degrade: (nil,nil), NOT an error.
	if m != nil || err != nil {
		t.Errorf("404 should return (nil,nil), got (%v,%v)", m, err)
	}
}

func TestCoreClient_FetchPayloadManifest_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not-json"))
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	m, err := c.FetchPayloadManifest("WIDGET")
	if m != nil || err != nil {
		t.Errorf("bad-json should return (nil,nil), got (%v,%v)", m, err)
	}
}

func TestCoreClient_FetchPayloadManifest_NetworkError(t *testing.T) {
	// Point at a closed server → HTTP transport error → graceful (nil,nil).
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := srv.URL
	srv.Close()

	c := NewCoreClient(closedURL)
	m, err := c.FetchPayloadManifest("WIDGET")
	if m != nil || err != nil {
		t.Errorf("network error should graceful-degrade, got (%v,%v)", m, err)
	}
}

func TestCoreClient_FetchNodeChildren(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/NGRP/children") {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode([]NodeChildInfo{
			{Name: "child-1", NodeType: "cell"},
			{Name: "child-2", NodeType: "cell"},
		})
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	got, err := c.FetchNodeChildren("NGRP")
	if err != nil {
		t.Fatalf("FetchNodeChildren: %v", err)
	}
	if len(got) != 2 || got[0].Name != "child-1" {
		t.Errorf("children = %+v", got)
	}

	// Empty node name short-circuits.
	if got, err := c.FetchNodeChildren(""); got != nil || err != nil {
		t.Errorf("empty name should short-circuit, got (%v,%v)", got, err)
	}
}

func TestCoreClient_FetchNodeChildren_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	got, err := c.FetchNodeChildren("NGRP")
	if got != nil || err != nil {
		t.Errorf("Non-200 should return (nil,nil), got (%v,%v)", got, err)
	}
}

func TestCoreClient_FetchNodeChildren_BadJSON(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("{not:json"))
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	got, err := c.FetchNodeChildren("NGRP")
	if got != nil || err != nil {
		t.Errorf("bad-json should return (nil,nil), got (%v,%v)", got, err)
	}
}

func TestCoreClient_FetchNodeBins(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("nodes"); got != "N1,N2" {
			t.Errorf("nodes query = %q, want N1,N2", got)
		}
		json.NewEncoder(w).Encode([]NodeBinInfo{
			{NodeName: "N1", BinLabel: "B1", PayloadCode: "WIDGET", UOPRemaining: 10, Occupied: true},
			{NodeName: "N2", Occupied: false},
		})
	}))
	defer srv.Close()

	c := NewCoreClient(srv.URL)
	bins, err := c.FetchNodeBins([]string{"N1", "N2"})
	if err != nil {
		t.Fatalf("FetchNodeBins: %v", err)
	}
	if len(bins) != 2 || bins[0].BinLabel != "B1" {
		t.Errorf("bins = %+v", bins)
	}

	// Empty slice short-circuits.
	if bins, err := c.FetchNodeBins(nil); bins != nil || err != nil {
		t.Errorf("empty nodes list should short-circuit, got (%v,%v)", bins, err)
	}
}

func TestCoreClient_FetchNodeBins_ErrorPaths(t *testing.T) {
	// 500 → graceful (nil,nil).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	if bins, err := c.FetchNodeBins([]string{"N1"}); bins != nil || err != nil {
		t.Errorf("500 should graceful-degrade, got (%v,%v)", bins, err)
	}

	// Bad JSON → graceful (nil,nil).
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Write([]byte("not-json"))
	}))
	defer srv2.Close()
	c2 := NewCoreClient(srv2.URL)
	if bins, err := c2.FetchNodeBins([]string{"N1"}); bins != nil || err != nil {
		t.Errorf("bad-json should graceful-degrade, got (%v,%v)", bins, err)
	}
}

func TestCoreClient_LoadBin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		json.NewEncoder(w).Encode(BinLoadResponse{
			Status:       "ok",
			BinID:        42,
			BinLabel:     "B42",
			PayloadCode:  "WIDGET",
			UOPRemaining: 100,
		})
	}))
	defer srv.Close()

	c := NewCoreClient(srv.URL)
	resp, err := c.LoadBin(&BinLoadRequest{
		NodeName:    "N1",
		PayloadCode: "WIDGET",
		UOPCount:    100,
	})
	if err != nil {
		t.Fatalf("LoadBin: %v", err)
	}
	if resp.BinID != 42 || resp.BinLabel != "B42" {
		t.Errorf("LoadBin response = %+v", resp)
	}
}

func TestCoreClient_LoadBin_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(BinLoadResponse{Status: "error", Detail: "bin already loaded"})
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	if _, err := c.LoadBin(&BinLoadRequest{NodeName: "N1"}); err == nil || !strings.Contains(err.Error(), "bin already loaded") {
		t.Errorf("LoadBin err = %v, want detail surfaced", err)
	}
}

func TestCoreClient_LoadBin_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(BinLoadResponse{})
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	if _, err := c.LoadBin(&BinLoadRequest{NodeName: "N1"}); err == nil {
		t.Error("Non-200 should error")
	}
}

func TestCoreClient_LoadBin_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewCoreClient(url)
	if _, err := c.LoadBin(&BinLoadRequest{NodeName: "N1"}); err == nil {
		t.Error("network error should surface")
	}
}

func TestCoreClient_ClearBin_Success(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	if err := c.ClearBin("N1"); err != nil {
		t.Errorf("ClearBin: %v", err)
	}
}

func TestCoreClient_ClearBin_ErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "detail": "nope"})
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	if err := c.ClearBin("N1"); err == nil || !strings.Contains(err.Error(), "nope") {
		t.Errorf("ClearBin err = %v", err)
	}
}

func TestCoreClient_ClearBin_Non200NoDetail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		json.NewEncoder(w).Encode(map[string]string{}) // no detail
	}))
	defer srv.Close()
	c := NewCoreClient(srv.URL)
	err := c.ClearBin("N1")
	if err == nil || !strings.Contains(err.Error(), "core returned") {
		t.Errorf("ClearBin err = %v, want 'core returned ...'", err)
	}
}

func TestCoreClient_ClearBin_NetworkError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	url := srv.URL
	srv.Close()
	c := NewCoreClient(url)
	if err := c.ClearBin("N1"); err == nil {
		t.Error("network error should surface")
	}
}

// ── core_sync_service.go ────────────────────────────────────────────

func TestCoreSyncService_StartupReconcileCallsAllHooks(t *testing.T) {
	eng := newCoverageEngine(t)
	nodeSyncCalls := 0
	catalogSyncCalls := 0
	eng.SetNodeSyncFunc(func() { nodeSyncCalls++ })
	eng.SetCatalogSyncFunc(func() { catalogSyncCalls++ })
	// No sendFn configured → RequestOrderStatusSync returns error.
	err := eng.coreSync.StartupReconcile()
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("StartupReconcile err = %v, want send-function-not-configured", err)
	}
	if nodeSyncCalls != 1 || catalogSyncCalls != 1 {
		t.Errorf("nodeSync=%d catalogSync=%d, want 1/1", nodeSyncCalls, catalogSyncCalls)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_NoSendFn(t *testing.T) {
	eng := newCoverageEngine(t)
	err := eng.coreSync.RequestOrderStatusSync()
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("err = %v, want send-function-not-configured", err)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_NoActiveOrders(t *testing.T) {
	eng := newCoverageEngine(t)
	sent := 0
	eng.SetSendFunc(func(*protocol.Envelope) error { sent++; return nil })
	// No orders in DB → returns nil without sending.
	if err := eng.coreSync.RequestOrderStatusSync(); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sent != 0 {
		t.Errorf("sendFn called %d times, want 0 when no active orders", sent)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_SendsEnvelope(t *testing.T) {
	eng := newCoverageEngine(t)
	// Seed an active order so ListActiveOrders returns something.
	orderUUID := "uuid-active"
	if _, err := eng.db.CreateOrder(orderUUID, "complex", nil, false, 1, "", "", "", "", false, ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	var captured *protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		captured = env
		return nil
	})
	if err := eng.coreSync.RequestOrderStatusSync(); err != nil {
		t.Fatalf("RequestOrderStatusSync: %v", err)
	}
	if captured == nil {
		t.Fatal("expected an envelope to be sent")
	}
	if captured.Type != protocol.TypeData {
		t.Errorf("envelope Type = %q, want %q", captured.Type, protocol.TypeData)
	}
	var data protocol.Data
	if err := captured.DecodePayload(&data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if data.Subject != protocol.SubjectOrderStatusRequest {
		t.Errorf("Subject = %q, want %q", data.Subject, protocol.SubjectOrderStatusRequest)
	}
	// The body should embed the order UUID we seeded.
	var req protocol.OrderStatusRequest
	if err := json.Unmarshal(data.Body, &req); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if len(req.OrderUUIDs) != 1 || req.OrderUUIDs[0] != orderUUID {
		t.Errorf("OrderUUIDs = %v, want [%s]", req.OrderUUIDs, orderUUID)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_SendFnError(t *testing.T) {
	eng := newCoverageEngine(t)
	if _, err := eng.db.CreateOrder("uuid-err", "complex", nil, false, 1, "", "", "", "", false, ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	eng.SetSendFunc(func(*protocol.Envelope) error { return fmt.Errorf("kafka down") })
	err := eng.coreSync.RequestOrderStatusSync()
	if err == nil || !strings.Contains(err.Error(), "kafka down") {
		t.Errorf("err = %v, want kafka-down", err)
	}
}

func TestCoreSyncService_HandleOrderStatusSnapshots(t *testing.T) {
	eng := newCoverageEngine(t)
	// Create a local order so ApplyCoreStatusSnapshot has something to act on.
	if _, err := eng.db.CreateOrder("uuid-snap", "complex", nil, false, 1, "", "", "", "", false, ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	// One known snapshot + one unknown UUID (exercises the debugFn err path
	// inside ApplyCoreStatusSnapshot, without crashing).
	items := []protocol.OrderStatusSnapshot{
		{OrderUUID: "uuid-snap", Found: true, Status: "confirmed"},
		{OrderUUID: "uuid-missing", Found: false, ErrorDetail: "not found in core"},
	}
	// Should not panic or return anything — just exercises the loop.
	eng.coreSync.HandleOrderStatusSnapshots(items)
}

// ── reconciliation.go (engine-level thin delegates) ─────────────────

func TestEngine_StartupReconcileDelegates(t *testing.T) {
	eng := newCoverageEngine(t)
	// Both delegates through to coreSync.StartupReconcile → no sendFn → err.
	if err := eng.StartupReconcile(); err == nil {
		t.Error("StartupReconcile without sendFn should error")
	}
	if err := eng.RequestOrderStatusSync(); err == nil {
		t.Error("RequestOrderStatusSync without sendFn should error")
	}
	// HandleOrderStatusSnapshots on empty list — must be a no-op.
	eng.HandleOrderStatusSnapshots(nil)
}

// ── reconciliation_service.go ───────────────────────────────────────

func TestReconciliationService_Summary(t *testing.T) {
	eng := newCoverageEngine(t)
	summary, err := eng.Reconciliation().Summary()
	if err != nil {
		t.Fatalf("Summary: %v", err)
	}
	if summary == nil {
		t.Fatal("summary is nil")
	}
	// Fresh DB — nothing to reconcile.
	if summary.TotalAnomalies != 0 {
		t.Errorf("fresh DB TotalAnomalies = %d, want 0", summary.TotalAnomalies)
	}
	if summary.DeadLetters != 0 {
		t.Errorf("fresh DB DeadLetters = %d, want 0", summary.DeadLetters)
	}
}

func TestReconciliationService_ListAnomaliesEmpty(t *testing.T) {
	eng := newCoverageEngine(t)
	anomalies, err := eng.Reconciliation().ListAnomalies()
	if err != nil {
		t.Fatalf("ListAnomalies: %v", err)
	}
	if len(anomalies) != 0 {
		t.Errorf("fresh DB should have no anomalies, got %d", len(anomalies))
	}
}

func TestReconciliationService_ListDeadLetterAndRequeue(t *testing.T) {
	eng := newCoverageEngine(t)
	db := eng.db
	// Seed an outbox row + bump retries above MaxOutboxRetries to dead-letter it.
	id, err := db.EnqueueOutbox([]byte(`{"msg":1}`), "test")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < store.MaxOutboxRetries; i++ {
		if err := db.IncrementOutboxRetries(id); err != nil {
			t.Fatalf("increment: %v", err)
		}
	}

	dead, err := eng.Reconciliation().ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != id {
		t.Fatalf("dead-letter list = %+v, want [%d]", dead, id)
	}

	// Requeue clears retries so the row is no longer dead-lettered.
	if err := eng.Reconciliation().RequeueOutbox(id); err != nil {
		t.Fatalf("RequeueOutbox: %v", err)
	}
	dead2, err := eng.Reconciliation().ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox after requeue: %v", err)
	}
	if len(dead2) != 0 {
		t.Errorf("after requeue, dead-letter list should be empty, got %d rows", len(dead2))
	}
}

// ── countgroup_sender.go ────────────────────────────────────────────

func TestEngine_SendCountGroupAck_NoSendFn(t *testing.T) {
	eng := newCoverageEngine(t)
	err := eng.SendCountGroupAck(&protocol.CountGroupAck{
		CorrelationID: "corr-1", Group: "g1", Outcome: "acked",
	})
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("err = %v, want send-function-not-configured", err)
	}
}

func TestEngine_SendCountGroupAck_BuildsEnvelope(t *testing.T) {
	eng := newCoverageEngine(t)
	var captured *protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		captured = env
		return nil
	})
	ack := &protocol.CountGroupAck{
		CorrelationID: "corr-1",
		Group:         "g1",
		Outcome:       protocol.AckOutcomeAcked,
		AckLatencyMs:  42,
		Timestamp:     time.Now(),
	}
	if err := eng.SendCountGroupAck(ack); err != nil {
		t.Fatalf("SendCountGroupAck: %v", err)
	}
	if captured == nil {
		t.Fatal("expected envelope to be sent")
	}
	if captured.Type != protocol.TypeData {
		t.Errorf("envelope Type = %q, want %q", captured.Type, protocol.TypeData)
	}
	var data protocol.Data
	if err := captured.DecodePayload(&data); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if data.Subject != protocol.SubjectCountGroupAck {
		t.Errorf("Subject = %q, want %q", data.Subject, protocol.SubjectCountGroupAck)
	}
	if captured.Src.Role != protocol.RoleEdge || captured.Src.Station != eng.cfg.StationID() {
		t.Errorf("Src = %+v, want edge/%s", captured.Src, eng.cfg.StationID())
	}
	if captured.Dst.Role != protocol.RoleCore {
		t.Errorf("Dst.Role = %q, want core", captured.Dst.Role)
	}
}

func TestEngine_SendCountGroupAck_SendFnError(t *testing.T) {
	eng := newCoverageEngine(t)
	eng.SetSendFunc(func(*protocol.Envelope) error { return fmt.Errorf("bus closed") })
	err := eng.SendCountGroupAck(&protocol.CountGroupAck{CorrelationID: "c1"})
	if err == nil || !strings.Contains(err.Error(), "bus closed") {
		t.Errorf("err = %v, want bus-closed", err)
	}
}
