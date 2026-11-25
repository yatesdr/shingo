package engine

import (
	"encoding/json"
	"fmt"
	"reflect"
	"shingoedge/plc"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/internal/testdb"
	"shingoedge/orders"
	ordertestutil "shingoedge/orders/testutil"
	"shingoedge/service"
	"shingoedge/store"
	"shingoedge/store/catalog"
	"shingoedge/uop"
)

// ── Shared fixtures ─────────────────────────────────────────────────

// newCoverageDB opens a fresh migrated SQLite DB for one test (testdb.Open).
func newCoverageDB(t *testing.T) *store.DB {
	t.Helper()
	return testdb.Open(t)
}

// newCoverageEngine builds an Engine wired with the minimum set of
// dependencies needed for the thin-layer tests — no Start() call.
func newCoverageEngine(t *testing.T) *Engine {
	t.Helper()
	db := newCoverageDB(t)
	cfg := &config.Config{}
	eng := &Engine{
		cfg:      cfg,
		db:       db,
		Events:   NewEventBus(),
		stopChan: make(chan struct{}),
		logFn:    func(string, ...any) {},
		debugFn:  func(string, ...any) {},
	}
	eng.reconciliation = newReconciliationService(eng.db)
	eng.coreSync = newCoreSyncService(eng)
	eng.orderMgr = orders.NewManager(db, ordertestutil.NoOpOrderEmitter{}, cfg.StationID())
	eng.stationService = service.NewStationService(db)
	eng.changeoverService = service.NewChangeoverService(db)
	// Production wires the UOP mutator as the inventory sink (main.go);
	// Released adjustments and the other slot verbs route through it.
	eng.SetInventoryDeltaSink(uop.New(db, "stn-test", db, db))
	return eng
}

// ── engine.go accessors ─────────────────────────────────────────────

func TestEngine_Accessors(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	if eng.DB() == nil {
		t.Error("DB() returned nil")
	}
	if eng.AppConfig() == nil {
		t.Error("AppConfig() returned nil")
	}
	if eng.AppConfig() != eng.cfg {
		t.Error("AppConfig() is not the engine's config")
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
	t.Parallel()
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

// heldLink is the robot feed's link with a Stop held until let go: the feed's
// loop closes its link as it leaves at Stop, so the loop is held there.
type heldLink struct {
	fakeEphemeralLink
	stopping, release chan struct{}
	said, gone        sync.Once
}

func (l *heldLink) Stop() {
	l.said.Do(func() { close(l.stopping) })
	<-l.release
	l.fakeEphemeralLink.Stop()
}

// letGo releases the held Stop; any number of calls.
func (l *heldLink) letGo() { l.gone.Do(func() { close(l.release) }) }

// TestEngineStop_WaitsForTheLoopsStartLaunched: Stop returns only once the
// goroutines Start launched have left. The robot feed's loop is held where it
// leaves, closing the feed's link as a stop does, and Stop must not return
// while it is. Docket B5: Stop closed the stop channel, joined the PLC poll,
// wrote the no-cart stretches and returned, waiting for no other loop.
func TestEngineStop_WaitsForTheLoopsStartLaunched(t *testing.T) {
	t.Parallel()
	const topic = "shingo.ephemeral.test"
	eng := New(Config{AppConfig: &config.Config{StationUID: "edge.a",
		Messaging: config.MessagingConfig{EphemeralTopic: topic}}, DB: testEngineDB(t)})
	link := &heldLink{stopping: make(chan struct{}), release: make(chan struct{})}
	t.Cleanup(link.letGo)
	eng.SetEphemeralLink(link)
	eng.Start()
	eng.OfferEphemeralTopic(topic)
	eventually(t, "the feed's link opened", func() bool {
		starts, _, _ := link.state()
		return len(starts) == 1
	})
	stopped := make(chan struct{})
	go func() {
		eng.Stop()
		close(stopped)
	}()
	select {
	case <-link.stopping:
	case <-time.After(10 * time.Second):
		t.Fatal("the robot feed's loop did not close its link at Stop")
	}
	select {
	case <-stopped:
		t.Fatal("Stop returned while the robot feed's loop was still closing its link")
	case <-time.After(100 * time.Millisecond):
	}
	link.letGo()
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the loop had left")
	}
}

// TestEngineStop_OneDeadlineOverEveryWait: Stop's limit covers its waits: a
// goroutine on the engine's group that never leaves holds Stop for the limit
// and no longer (engineStopLimit).
func TestEngineStop_OneDeadlineOverEveryWait(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	eng.loops.spawn(func() { <-release })
	done := make(chan struct{})
	go func() {
		eng.stopBounded(100 * time.Millisecond)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop was still waiting on a wedged goroutine long after its limit")
	}
}

// TestEngineStop_WaitsForAnInboundHandler: a Stop begun while a message from
// Core is being handled (Inbound) returns only once the handler has, and a
// message taken after Stop is still handled, outside the wait.
func TestEngineStop_WaitsForAnInboundHandler(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	in, out := make(chan struct{}), make(chan struct{})
	go eng.Inbound(func() {
		close(in)
		<-out
	})
	<-in
	stopped := make(chan struct{})
	go func() {
		eng.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
		t.Fatal("Stop returned while an inbound handler was running")
	case <-time.After(100 * time.Millisecond):
	}
	close(out)
	select {
	case <-stopped:
	case <-time.After(10 * time.Second):
		t.Fatal("Stop did not return once the handler had")
	}
	handled := false
	eng.Inbound(func() { handled = true })
	if !handled {
		t.Error("a message taken after Stop was not handled")
	}
}

// ── Core node sync ──────────────────────────────────────────────────

func TestEngine_SetCoreNodesEmitsEvent(t *testing.T) {
	t.Parallel()
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
	eng.SetCoreNodes(in, false)

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
	t.Parallel()
	eng := newCoverageEngine(t)
	// No fn set → nothing asked, and the caller is told so.
	if err := eng.RequestNodeSync(); err != errSyncNotWired {
		t.Errorf("unwired RequestNodeSync err = %v, want errSyncNotWired", err)
	}

	called := 0
	eng.SetNodeSyncFunc(func() error { called++; return nil })
	testutil.MustNoErr(t, eng.RequestNodeSync(), "RequestNodeSync")
	testutil.MustNoErr(t, eng.RequestNodeSync(), "RequestNodeSync")
	if called != 2 {
		t.Errorf("nodeSyncFn called %d times, want 2", called)
	}
}

func TestEngine_RequestCatalogSync(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	if err := eng.RequestCatalogSync(); err != errSyncNotWired { // no fn set
		t.Errorf("unwired RequestCatalogSync err = %v, want errSyncNotWired", err)
	}

	called := 0
	eng.SetCatalogSyncFunc(func() error { called++; return nil })
	testutil.MustNoErr(t, eng.RequestCatalogSync(), "RequestCatalogSync")
	if called != 1 {
		t.Errorf("catalogSyncFn called %d times, want 1", called)
	}
}

func TestEngine_SendEnvelope(t *testing.T) {
	t.Parallel()
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
	testutil.MustNoErr(t, eng.SendEnvelope(sentinel), "SendEnvelope")
	if captured != sentinel {
		t.Error("sendFn did not receive the envelope we passed in")
	}
}

func TestEngine_ReconnectKafka(t *testing.T) {
	t.Parallel()
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

// TestEngine_HooksAreSetAndReadOnTwoGoroutines pins docket D31, step one. The
// composition root (cmd/shingoedge main.go) sets each of these hooks after
// Start: three on the kafka-connect-retry goroutine, the rest on main beside
// the goroutines Start has running. A web handler, the heartbeater or one of
// the engine's own loops reads it from another goroutine. So each should be
// stored as heartbeatBodyFn is, in a sync/atomic type, and a set and a read
// then need no other ordering.
//
// Each hook gets two checks. The field's type, by reflection, fails with or
// without the race detector. The two goroutines, one on the setter and one on
// the reader, fail only under it. The dev tier runs no -race: that half is run
// by CI's docker-race job (.github/workflows/edge.yml), and by hand with -race
// and -run TestEngine_HooksAreSetAndReadOnTwoGoroutines (tags lockcheck).
// A healthy boot under -race is no evidence either way: it has no reader in
// the window.
//
// Not parallel: the detector fails every test running when it reports, so
// this one runs alone.
func TestEngine_HooksAreSetAndReadOnTwoGoroutines(t *testing.T) {
	hooks := []struct {
		field string
		set   func(e *Engine)
		read  func(e *Engine)
	}{
		// The control: atomic before this pin was written.
		{"heartbeatBodyFn",
			func(e *Engine) { e.SetHeartbeatBodyFunc(func() *protocol.EdgeHeartbeat { return nil }) },
			func(e *Engine) { _ = e.HeartbeatBody() }},
		{"kafkaReconnFn",
			func(e *Engine) { e.SetKafkaReconnectFunc(func() error { return nil }) },
			func(e *Engine) { _ = e.ReconnectKafka() }},
		{"nodeSyncFn",
			func(e *Engine) { e.SetNodeSyncFunc(func() error { return nil }) },
			func(e *Engine) { e.RequestNodeSync() }},
		{"catalogSyncFn",
			func(e *Engine) { e.SetCatalogSyncFunc(func() error { return nil }) },
			func(e *Engine) { e.RequestCatalogSync() }},
		{"sendFn",
			func(e *Engine) { e.SetSendFunc(func(*protocol.Envelope) error { return nil }) },
			func(e *Engine) { _ = e.SendEnvelope(&protocol.Envelope{}) }},
		{"plantClaimsFn",
			func(e *Engine) { e.SetPlantClaimsFunc(func(int64) {}) },
			func(e *Engine) { e.publishPlantClaims(1) }},
		{"kafkaConnFn",
			func(e *Engine) { e.SetKafkaConnFunc(func() bool { return true }) },
			func(e *Engine) { _ = e.KafkaConnected() }},
		{"kafkaLastPublishFn",
			func(e *Engine) {
				e.SetKafkaLastPublishFunc(func() (bool, time.Time, bool) { return true, time.Time{}, true })
			},
			func(e *Engine) { _, _, _ = e.KafkaLastPublish() }},
		{"productionTickLagFn",
			func(e *Engine) { e.SetProductionTickLagFunc(func() (int64, int64, error) { return 0, 0, nil }) },
			func(e *Engine) {
				if _, _, err := e.ProductionTickLag(); err != nil {
					return // the read raced the setter; its value is not the pin's question
				}
			}},
	}
	engineType := reflect.TypeOf((*Engine)(nil)).Elem()
	for _, h := range hooks {
		t.Run(h.field, func(t *testing.T) {
			f, ok := engineType.FieldByName(h.field)
			if !ok {
				t.Fatalf("Engine has no field %s: this pin lost its input; name the field that replaced it", h.field)
			}
			if f.Type.PkgPath() != "sync/atomic" {
				t.Errorf("Engine.%s is a plain %s: store it in a sync/atomic type and read it with Load, as heartbeatBodyFn is; "+
					"main.go sets it while another goroutine may read it", h.field, f.Type)
			}
			const passes = 200
			e := &Engine{}
			var wg sync.WaitGroup
			wg.Add(2)
			go func() {
				defer wg.Done()
				for i := 0; i < passes; i++ {
					h.set(e)
				}
			}()
			go func() {
				defer wg.Done()
				for i := 0; i < passes; i++ {
					h.read(e)
				}
			}()
			wg.Wait()
		})
	}
}

// TestEngine_TheDeltaSinkIsStoredAsTheHooksAre pins docket D31's ninth field
// of the class: main.go sets inventoryDelta (SetInventoryDeltaSink, the UOP
// mutator) after Start, while goroutines Start began (the PLC poll, the
// lineside reporter, the event handlers) may read it. So it is stored as the
// hooks above are, in a sync/atomic type, and read through one accessor,
// deltaSink. The type is the check, by reflection, with or without the race
// detector. uop's archtests that name the sink's methods
// (TestArch_CarrierLeavesThroughOneVerb and
// TestArch_NoDirectRecordBinOrRecordBucket) name them with no receiver, so
// they hold for a call through deltaSink as they held for one through the
// field.
func TestEngine_TheDeltaSinkIsStoredAsTheHooksAre(t *testing.T) {
	f, ok := reflect.TypeOf((*Engine)(nil)).Elem().FieldByName("inventoryDelta")
	if !ok {
		t.Fatal("Engine has no field inventoryDelta: this pin lost its input; name the field that replaced it")
	}
	if f.Type.PkgPath() != "sync/atomic" {
		t.Errorf("Engine.inventoryDelta is a plain %s: store it in a sync/atomic type, as the hooks above are; "+
			"main.go sets it after Start, while Start's goroutines may read it", f.Type)
	}
}

// ── HandlePayloadCatalog ────────────────────────────────────────────

func TestEngine_HandlePayloadCatalog_UpsertsAndPrunes(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)

	// Seed an entry with ID=99 that should be pruned when Core's catalog
	// doesn't mention it.
	stale := &catalog.CatalogEntry{ID: 99, Name: "stale", Code: "STALE", UOPCapacity: 10}
	testutil.MustNoErr(t, eng.db.UpsertPayloadCatalog(stale), "seed stale")

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
	byCode := map[string]*catalog.CatalogEntry{}
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
	t.Parallel()
	eng := newCoverageEngine(t)
	// Seed three entries.
	for i := int64(1); i <= 3; i++ {
		eng.db.UpsertPayloadCatalog(&catalog.CatalogEntry{
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
	t.Parallel()
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
	em.EmitCounterDelta(7, 1, 3, 42, "jump", plc.TickIdentity{StyleID: 2, Kind: plc.TickToday})
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
		EventCounterRead, EventCounterDelta,
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
	if cd, ok := received[EventCounterDelta].(CounterDeltaEvent); !ok {
		t.Error("CounterDelta payload wrong type")
	} else if cd.Anomaly != "jump" || cd.Delta != 3 || cd.NewCount != 42 {
		t.Errorf("CounterDelta payload = %+v", cd)
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
	t.Parallel()
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
	em.EmitOrderFailed(1, "uuid-1", "complex", "no_bin", "timeout")

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
	} else if p.Reason != "timeout" || p.Code != "no_bin" {
		t.Errorf("OrderFailed payload = %+v", p)
	}
}

// ── core_sync_service.go ────────────────────────────────────────────

// StartupReconcile is the order reconcile and nothing else: it asks for no
// node list and no catalog (the boot burst does, and feeds resend them), and
// it sends the order status request, which here fails for want of a sendFn.
func TestCoreSyncService_StartupReconcileAsksOnlyForOrders(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	nodeSyncCalls := 0
	catalogSyncCalls := 0
	eng.SetNodeSyncFunc(func() error { nodeSyncCalls++; return nil })
	eng.SetCatalogSyncFunc(func() error { catalogSyncCalls++; return nil })
	// No sendFn configured → RequestOrderStatusSync returns error.
	err := eng.coreSync.StartupReconcile()
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("StartupReconcile err = %v, want send-function-not-configured", err)
	}
	if nodeSyncCalls != 0 || catalogSyncCalls != 0 {
		t.Errorf("nodeSync=%d catalogSync=%d, want 0/0: the boot burst asks, and feeds resend", nodeSyncCalls, catalogSyncCalls)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_NoSendFn(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	err := eng.coreSync.RequestOrderStatusSync()
	if err == nil || !strings.Contains(err.Error(), "send function not configured") {
		t.Errorf("err = %v, want send-function-not-configured", err)
	}
}

// TestCoreSyncService_RequestOrderStatusSync_NoActiveOrders: an Edge with
// nothing to ask about STILL asks.
//
// This used to assert the opposite, on the reasonable-looking grounds that there
// was nothing to reconcile. That became exactly backwards once Core started
// authoring orders: an Edge with no orders is the one most likely to be missing
// one. A fresh install, a wiped database, or a plant that dropped every
// projection would ask nothing, Core would never get the chance to answer with
// the orders it holds for that station, and the hole would never close.
func TestCoreSyncService_RequestOrderStatusSync_NoActiveOrders(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	var captured *protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error { captured = env; return nil })
	testutil.MustNoErr(t, eng.coreSync.RequestOrderStatusSync(), "unexpected err")
	if captured == nil {
		t.Fatal("sent nothing with an empty order list; Core can only heal an Edge that asks")
	}
	var data protocol.Data
	testutil.MustNoErr(t, captured.DecodePayload(&data), "decode payload")
	if data.Subject != protocol.SubjectOrderStatusRequest {
		t.Errorf("subject = %q, want %q", data.Subject, protocol.SubjectOrderStatusRequest)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_SendsEnvelope(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	// Seed an active order so ListActiveOrders returns something.
	orderUUID := "uuid-active"
	if _, err := eng.db.CreateOrder(orderUUID, "complex", nil, false, 1, "", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	var captured *protocol.Envelope
	eng.SetSendFunc(func(env *protocol.Envelope) error {
		captured = env
		return nil
	})
	testutil.MustNoErr(t, eng.coreSync.RequestOrderStatusSync(), "RequestOrderStatusSync")
	if captured == nil {
		t.Fatal("expected an envelope to be sent")
	}
	if captured.Type != protocol.TypeData {
		t.Errorf("envelope Type = %q, want %q", captured.Type, protocol.TypeData)
	}
	var data protocol.Data
	testutil.MustNoErr(t, captured.DecodePayload(&data), "decode payload")
	if data.Subject != protocol.SubjectOrderStatusRequest {
		t.Errorf("Subject = %q, want %q", data.Subject, protocol.SubjectOrderStatusRequest)
	}
	// The body should embed the order UUID we seeded.
	var req protocol.OrderStatusRequest
	testutil.MustNoErr(t, json.Unmarshal(data.Body, &req), "unmarshal body")
	if len(req.OrderUUIDs) != 1 || req.OrderUUIDs[0] != orderUUID {
		t.Errorf("OrderUUIDs = %v, want [%s]", req.OrderUUIDs, orderUUID)
	}
}

func TestCoreSyncService_RequestOrderStatusSync_SendFnError(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	if _, err := eng.db.CreateOrder("uuid-err", "complex", nil, false, 1, "", "", "", protocol.Height{}, false, "", "", ""); err != nil {
		t.Fatalf("create order: %v", err)
	}
	eng.SetSendFunc(func(*protocol.Envelope) error { return fmt.Errorf("kafka down") })
	err := eng.coreSync.RequestOrderStatusSync()
	if err == nil || !strings.Contains(err.Error(), "kafka down") {
		t.Errorf("err = %v, want kafka-down", err)
	}
}

func TestCoreSyncService_HandleOrderStatusSnapshots(t *testing.T) {
	t.Parallel()
	eng := newCoverageEngine(t)
	// Create a local order so ApplyCoreStatusSnapshot has something to act on.
	if _, err := eng.db.CreateOrder("uuid-snap", "complex", nil, false, 1, "", "", "", protocol.Height{}, false, "", "", ""); err != nil {
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
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
	t.Parallel()
	eng := newCoverageEngine(t)
	db := eng.db
	// Seed an outbox row + bump retries above MaxOutboxRetries to dead-letter it.
	id, err := db.EnqueueOutbox([]byte(`{"msg":1}`), "test")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	for i := 0; i < store.MaxOutboxRetries; i++ {
		testutil.MustNoErr(t, db.IncrementOutboxRetries(id), "increment")
	}

	dead, err := eng.Reconciliation().ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox: %v", err)
	}
	if len(dead) != 1 || dead[0].ID != id {
		t.Fatalf("dead-letter list = %+v, want [%d]", dead, id)
	}

	// Requeue clears retries so the row is no longer dead-lettered.
	testutil.MustNoErr(t, eng.Reconciliation().RequeueOutbox(id), "RequeueOutbox")
	dead2, err := eng.Reconciliation().ListDeadLetterOutbox(10)
	if err != nil {
		t.Fatalf("ListDeadLetterOutbox after requeue: %v", err)
	}
	if len(dead2) != 0 {
		t.Errorf("after requeue, dead-letter list should be empty, got %d rows", len(dead2))
	}
}
