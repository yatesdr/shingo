package www

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/auth"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/service"
	"shingoedge/store"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// testDB is the shared SQLite database initialised by TestMain.
var testDB *store.DB

// TestMain creates an ephemeral SQLite database, runs migrations, and makes
// it available to all tests in this package via the testDB variable.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "shingo-edge-test-*")
	if err != nil {
		panic(err)
	}
	defer os.RemoveAll(dir)

	dbPath := filepath.Join(dir, "test.db")
	testDB, err = store.Open(dbPath)
	if err != nil {
		panic(err)
	}
	defer testDB.Close()

	os.Exit(m.Run())
}

// --- Stub engine ---

// stubOrderEmitter is a no-op implementation of orders.EventEmitter for tests.
// The order manager fires events on every transition; tests don't care about
// them, so we drop them on the floor.
type stubOrderEmitter struct{}

func (stubOrderEmitter) EmitOrderCreated(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {
}
func (stubOrderEmitter) EmitOrderStatusChanged(orderID int64, orderUUID, orderType, oldStatus, newStatus, eta string, payloadID, processNodeID *int64) {
}
func (stubOrderEmitter) EmitOrderCompleted(orderID int64, orderUUID, orderType string, payloadID, processNodeID *int64) {
}
func (stubOrderEmitter) EmitOrderFailed(orderID int64, orderUUID, orderType, reason string) {}

// stubEngine implements both ServiceAccess and EngineOrchestration for
// tests, since *Handlers now holds two fields of those interface types
// (Phase 6.5). Tests construct one *stubEngine and assign it to both
// fields in newTestHandlers — any handler can reach what it needs.
//
// Tests that want to verify a handler does NOT reach orchestration can
// build a narrower fixture: assign *stubEngine to h.engine, leave
// h.orchestration nil, and any accidental orchestration call will nil-
// panic with a clear stack. We don't ship that helper today; add it
// when a test needs the discipline.
//
// orderMgr is a real *orders.Manager backed by testDB so handlers in
// handlers_api_orders.go can exercise the full create/transition flow
// against the ephemeral SQLite database. Tests that don't touch order
// endpoints can ignore it.
type stubEngine struct {
	db       *store.DB
	cfg      *config.Config
	cfgPath  string
	core     map[string]protocol.NodeInfo
	orderMgr *orders.Manager

	// Spy fields — populated by stub methods so handler tests can assert on
	// the values that flowed through. Add new fields here as needed; keep
	// them named after the method that writes to them so the assertion
	// site is easy to find.
	lastReleaseChangeoverWaitCalledBy string
	lastReleaseOrderDisposition       *engine.ReleaseDisposition
	lastReleaseStagedOrdersDisposition *engine.ReleaseDisposition
}

func (s *stubEngine) AppConfig() *config.Config        { return s.cfg }
func (s *stubEngine) ConfigPath() string               { return s.cfgPath }
func (s *stubEngine) CoreAPI() *engine.CoreClient      { return nil }
func (s *stubEngine) PLCManager() *plc.Manager         { return nil }
func (s *stubEngine) OrderManager() *orders.Manager    { return s.orderMgr }
func (s *stubEngine) Reconciliation() *engine.ReconciliationService   { return nil }
func (s *stubEngine) CoreSync() *engine.CoreSyncService               { return nil }
func (s *stubEngine) ApplyWarLinkConfig()               {}
func (s *stubEngine) ReconnectKafka() error             { return nil }
func (s *stubEngine) SendEnvelope(env *protocol.Envelope) error        { return nil }
func (s *stubEngine) CoreNodes() map[string]protocol.NodeInfo          { return s.core }
func (s *stubEngine) RequestNodeSync()                  {}
func (s *stubEngine) RequestCatalogSync()               {}
func (s *stubEngine) RequestOrderStatusSync() error     { return nil }
func (s *stubEngine) StartupReconcile() error           { return nil }
func (s *stubEngine) SendClaimSync()                    {}
func (s *stubEngine) SyncProcessCounter(int64) error    { return nil }
func (s *stubEngine) EnsureTagPublished(int64, string, string) {}
func (s *stubEngine) ManageReportingPointTag(int64, string, string, bool, string, string) {}
func (s *stubEngine) CleanupReportingPointTag(int64, string, string, bool) {}
func (s *stubEngine) RequestNodeMaterial(int64, int64) (*engine.NodeOrderResult, error) { return nil, nil }
func (s *stubEngine) ReleaseNodeEmpty(int64) (*storeorders.Order, error)                     { return nil, nil }
func (s *stubEngine) ReleaseNodePartial(int64, int64) (*storeorders.Order, error)             { return nil, nil }
// ReleaseOrderWithLineside stub forwards to orderMgr.ReleaseOrder so
// existing release-flow tests (TestApiOrders_ReleaseOrder_Success, etc.)
// still exercise the status-transition + lifecycle-error paths. The
// engine-side lineside capture + UOP reset is covered by
// engine/operator_release_test.go, not the www handler tests. Pass nil
// remainingUOP since the stub doesn't model the disposition/manifest
// late-binding (Core-side concern).
func (s *stubEngine) ReleaseOrderWithLineside(orderID int64, disp engine.ReleaseDisposition) error {
	d := disp
	s.lastReleaseOrderDisposition = &d
	return s.orderMgr.ReleaseOrder(orderID, nil, "")
}
func (s *stubEngine) ReleaseStagedOrders(_ int64, disp engine.ReleaseDisposition) error {
	d := disp
	s.lastReleaseStagedOrdersDisposition = &d
	return nil
}
func (s *stubEngine) FinalizeProduceNode(int64) (*engine.NodeOrderResult, error)        { return nil, nil }
func (s *stubEngine) LoadBin(int64, string, int64, []protocol.IngestManifestItem) error { return nil }
func (s *stubEngine) ClearBin(int64) error                                             { return nil }
func (s *stubEngine) RequestEmptyBin(int64, string) (*storeorders.Order, error)               { return nil, nil }
func (s *stubEngine) RequestFullBin(int64, string) (*storeorders.Order, error)                { return nil, nil }
func (s *stubEngine) StartProcessChangeover(int64, int64, string, string) (*processes.Changeover, error) { return nil, nil }
func (s *stubEngine) CompleteProcessProductionCutover(int64) error                      { return nil }
func (s *stubEngine) CancelProcessChangeover(int64) error                               { return nil }
func (s *stubEngine) CancelProcessChangeoverRedirect(int64, *int64) error               { return nil }
func (s *stubEngine) ReleaseChangeoverWait(_ int64, calledBy string) error {
	s.lastReleaseChangeoverWaitCalledBy = calledBy
	return nil
}
func (s *stubEngine) StageNodeChangeoverMaterial(int64, int64) (*storeorders.Order, error)     { return nil, nil }
func (s *stubEngine) EmptyNodeForToolChange(int64, int64, int64) (*storeorders.Order, error)   { return nil, nil }
func (s *stubEngine) ReleaseNodeIntoProduction(int64, int64) (*storeorders.Order, error)       { return nil, nil }
func (s *stubEngine) SwitchNodeToTarget(int64, int64) error                             { return nil }
func (s *stubEngine) SwitchOperatorStationToTarget(int64, int64) error                   { return nil }
func (s *stubEngine) FlipABNode(int64) error                                            { return nil }

// ── Service accessors (Phase 6.2′) ─────────────────────────────────
// Each accessor returns a real *service.X backed by the test DB so
// handler tests exercise the full service → *store.DB → SQLite path.
// Phase 6.2′ replaced ~60 named-method stubs with these 9 accessors;
// the underlying behavior is identical (still goes through the same
// *store.DB shim methods) but the call shape now matches production.

func (s *stubEngine) StationService() *service.StationService {
	return service.NewStationService(s.db)
}
func (s *stubEngine) ChangeoverService() *service.ChangeoverService {
	return service.NewChangeoverService(s.db)
}
func (s *stubEngine) AdminService() *service.AdminService {
	return service.NewAdminService(s.db)
}
func (s *stubEngine) ProcessService() *service.ProcessService {
	return service.NewProcessService(s.db)
}
func (s *stubEngine) StyleService() *service.StyleService {
	return service.NewStyleService(s.db)
}
func (s *stubEngine) ShiftService() *service.ShiftService {
	return service.NewShiftService(s.db)
}
func (s *stubEngine) CounterService() *service.CounterService {
	return service.NewCounterService(s.db)
}
func (s *stubEngine) CatalogService() *service.CatalogService {
	return service.NewCatalogService(s.db)
}
func (s *stubEngine) OrderService() *service.OrderService {
	return service.NewOrderService(s.db)
}

// newTestHandlers builds *Handlers backed by the test DB, returning it along
// with a fresh chi.Router that has the config-group routes registered.
func newTestHandlers(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	cfg := config.Defaults()

	eng := &stubEngine{
		db:       testDB,
		cfg:      cfg,
		cfgPath:  cfgPath,
		orderMgr: orders.NewManager(testDB, stubOrderEmitter{}, "test.station"),
	}

	h := &Handlers{
		engine:        eng, // ServiceAccess
		orchestration: eng, // EngineOrchestration
		sessions:      newSessionStore(""),
		eventHub:      NewEventHub(),
	}

	r := chi.NewRouter()
	return h, r
}

// newAdminRouter creates a test handler + chi.Router with config API routes
// registered under /api (matching the production layout).
func newAdminRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Route("/api", func(r chi.Router) {
		// Public anomaly endpoints
		r.Post("/confirm-anomaly/{snapshotID}", h.apiConfirmAnomaly)
		r.Post("/dismiss-anomaly/{snapshotID}", h.apiDismissAnomaly)
		// Public lookups
		r.Get("/core-nodes", h.apiGetCoreNodes)
		r.Get("/payload-catalog", h.apiListPayloadCatalog)

		// Admin-only routes
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)

			r.Get("/plcs", h.apiListPLCs)
			r.Get("/warlink/status", h.apiWarLinkStatus)
			r.Put("/config/warlink", h.apiUpdateWarLink)

			r.Get("/reporting-points", h.apiListReportingPoints)
			r.Post("/reporting-points", h.apiCreateReportingPoint)
			r.Put("/reporting-points/{id}", h.apiUpdateReportingPoint)
			r.Delete("/reporting-points/{id}", h.apiDeleteReportingPoint)

			r.Get("/processes", h.apiListProcesses)
			r.Post("/processes", h.apiCreateProcess)
			r.Put("/processes/{id}", h.apiUpdateProcess)
			r.Delete("/processes/{id}", h.apiDeleteProcess)
			r.Put("/processes/{id}/active-style", h.apiSetActiveStyle)
			r.Get("/processes/{id}/styles", h.apiListProcessStyles)

			r.Get("/styles", h.apiListStyles)
			r.Post("/styles", h.apiCreateStyle)
			r.Put("/styles/{id}", h.apiUpdateStyle)
			r.Delete("/styles/{id}", h.apiDeleteStyle)

			r.Get("/styles/{id}/node-claims", h.apiListStyleNodeClaims)
			r.Post("/style-node-claims", h.apiUpsertStyleNodeClaim)
			r.Delete("/style-node-claims/{id}", h.apiDeleteStyleNodeClaim)

			// Sync (core nodes, payload catalog)
			r.Post("/core-nodes/sync", h.apiSyncCoreNodes)
			r.Post("/payload-catalog/sync", h.apiSyncPayloadCatalog)

			r.Put("/config/core-api", h.apiUpdateCoreAPI)
			r.Post("/config/core-api/test", h.apiTestCoreAPI)
			r.Put("/config/messaging", h.apiUpdateMessaging)
			r.Put("/config/station-id", h.apiUpdateStationID)
			r.Post("/config/kafka/test", h.apiTestKafka)
			r.Put("/config/auto-confirm", h.apiUpdateAutoConfirm)
			r.Post("/config/password", h.apiChangePassword)
		})
	})

	return h, r
}

// doRequest is a test helper that executes an HTTP request against a router
// and returns the response. Caller supplies method, path, optional JSON body,
// and optional cookie.
func doRequest(t *testing.T, router *chi.Mux, method, path string, body interface{}, cookie *http.Cookie) *http.Response {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

// authCookie creates an admin user and returns a valid session cookie.
func authCookie(t *testing.T, h *Handlers) *http.Cookie {
	t.Helper()
	hash, err := auth.HashPassword("password")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	testDB.Exec("DELETE FROM admin_users WHERE username = 'testadmin'")
	testDB.Exec("INSERT INTO admin_users (username, password_hash) VALUES ('testadmin', ?)", hash)

	req := httptest.NewRequest("POST", "/api/login-dummy", nil)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.sessions.setUser(w, req, "testadmin")

	result := w.Result()
	cookies := result.Cookies()
	if len(cookies) == 0 {
		t.Fatal("expected session cookie after setUser")
	}
	return cookies[0]
}

// decodeJSON decodes the response body into v.
func decodeJSON(t *testing.T, resp *http.Response, v interface{}) {
	t.Helper()
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("decode response body: %v", err)
	}
}

// assertStatus asserts the HTTP status code.
func assertStatus(t *testing.T, resp *http.Response, want int) {
	t.Helper()
	if resp.StatusCode != want {
		t.Errorf("status: got %d, want %d", resp.StatusCode, want)
	}
}

// assertJSONPath asserts that a value exists at the given dot-separated path
// in the JSON response body. Example: assertJSONPath(t, resp, "status", "ok")
func assertJSONPath(t *testing.T, resp *http.Response, path string, want interface{}) {
	t.Helper()
	defer resp.Body.Close()
	var raw map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode json for path assertion: %v", err)
	}
	// For single-level paths only (all our handlers use flat JSON)
	got, ok := raw[path]
	if !ok {
		t.Errorf("json path %q: key not found in response", path)
		return
	}
	if got != want {
		t.Errorf("json path %q: got %v (%T), want %v (%T)", path, got, got, want, want)
	}
}

// seedProcess creates a process and returns its ID.
func seedProcess(t *testing.T, name string) int64 {
	t.Helper()
	id, err := testDB.CreateProcess(name, "test process", "", "", "", false)
	if err != nil {
		t.Fatalf("seed process %q: %v", name, err)
	}
	return id
}

// seedStyle creates a style under the given process and returns its ID.
func seedStyle(t *testing.T, name string, processID int64) int64 {
	t.Helper()
	id, err := testDB.CreateStyle(name, "test style", processID)
	if err != nil {
		t.Fatalf("seed style %q: %v", name, err)
	}
	return id
}

// seedOperatorStation creates a bare operator station attached to the given
// process and returns its ID. Callers pass a unique code to avoid collisions
// across tests sharing testDB.
func seedOperatorStation(t *testing.T, processID int64, code, name string) int64 {
	t.Helper()
	id, err := testDB.CreateOperatorStation(stations.Input{
		ProcessID:  processID,
		Code:       code,
		Name:       name,
		DeviceMode: "fixed_hmi",
		Enabled:    true,
	})
	if err != nil {
		t.Fatalf("seed operator station %q: %v", code, err)
	}
	return id
}

// seedProcessNode creates a process node under the given station (optional)
// and returns its ID. Pass stationID=0 to leave OperatorStationID nil.
func seedProcessNode(t *testing.T, processID, stationID int64, coreNodeName string) int64 {
	t.Helper()
	in := processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: coreNodeName,
		Name:         coreNodeName,
		Enabled:      true,
	}
	if stationID > 0 {
		sid := stationID
		in.OperatorStationID = &sid
	}
	id, err := testDB.CreateProcessNode(in)
	if err != nil {
		t.Fatalf("seed process node %q: %v", coreNodeName, err)
	}
	return id
}

// orderUUIDCounter generates unique UUID-like strings for seeded test orders.
// testDB.orders.uuid is UNIQUE, so callers must avoid collisions across tests
// sharing the package-level testDB.
var orderUUIDCounter int64

// seedOrder inserts an order row directly via store.CreateOrder and (if status
// differs from the schema default of "pending") flips its status with
// UpdateOrderStatus. Returns the new order ID. Used by handlers_api_orders
// tests that need an existing order in a specific lifecycle state.
func seedOrder(t *testing.T, orderType, status string) int64 {
	t.Helper()
	n := atomic.AddInt64(&orderUUIDCounter, 1)
	uuid := fmt.Sprintf("test-uuid-%s-%d", orderType, n)
	id, err := testDB.CreateOrder(uuid, orderType, nil, false, 10, "DELIVERY", "", "", "", false, "")
	if err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if status != "" && status != "pending" {
		if err := testDB.UpdateOrderStatus(id, status); err != nil {
			t.Fatalf("update seeded order status to %q: %v", status, err)
		}
	}
	return id
}
