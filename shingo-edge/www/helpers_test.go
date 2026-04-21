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

	"shingo/protocol"
	"shingo/protocol/auth"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/orders"
	"shingoedge/plc"
	"shingoedge/store"

	"github.com/go-chi/chi/v5"
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

// stubEngine implements EngineAccess for tests. Only the fields needed by
// handlers_api_config.go are wired; everything else returns zero values.
//
// orderMgr is a real *orders.Manager backed by testDB so handlers in
// handlers_api_orders.go can exercise the full create/transition flow against
// the ephemeral SQLite database. Tests that don't touch order endpoints can
// ignore it.
type stubEngine struct {
	db       *store.DB
	cfg      *config.Config
	cfgPath  string
	core     map[string]protocol.NodeInfo
	orderMgr *orders.Manager
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
func (s *stubEngine) SyncProcessCounter(int64) error    { return nil }
func (s *stubEngine) EnsureTagPublished(int64, string, string) {}
func (s *stubEngine) ManageReportingPointTag(int64, string, string, bool, string, string) {}
func (s *stubEngine) CleanupReportingPointTag(int64, string, string, bool) {}
func (s *stubEngine) RequestNodeMaterial(int64, int64) (*engine.NodeOrderResult, error) { return nil, nil }
func (s *stubEngine) ReleaseNodeEmpty(int64) (*store.Order, error)                     { return nil, nil }
func (s *stubEngine) ReleaseNodePartial(int64, int64) (*store.Order, error)             { return nil, nil }
func (s *stubEngine) ReleaseStagedOrders(int64) error                                   { return nil }
func (s *stubEngine) ConfirmNodeManifest(int64) error                                  { return nil }
func (s *stubEngine) FinalizeProduceNode(int64) (*engine.NodeOrderResult, error)        { return nil, nil }
func (s *stubEngine) LoadBin(int64, string, int64, []protocol.IngestManifestItem) error { return nil }
func (s *stubEngine) ClearBin(int64) error                                             { return nil }
func (s *stubEngine) RequestEmptyBin(int64, string) (*store.Order, error)               { return nil, nil }
func (s *stubEngine) RequestFullBin(int64, string) (*store.Order, error)                { return nil, nil }
func (s *stubEngine) StartProcessChangeover(int64, int64, string, string) (*store.ProcessChangeover, error) { return nil, nil }
func (s *stubEngine) CompleteProcessProductionCutover(int64) error                      { return nil }
func (s *stubEngine) CancelProcessChangeover(int64) error                               { return nil }
func (s *stubEngine) CancelProcessChangeoverRedirect(int64, *int64) error               { return nil }
func (s *stubEngine) ReleaseChangeoverWait(int64) error                                 { return nil }
func (s *stubEngine) StageNodeChangeoverMaterial(int64, int64) (*store.Order, error)     { return nil, nil }
func (s *stubEngine) EmptyNodeForToolChange(int64, int64, int64) (*store.Order, error)   { return nil, nil }
func (s *stubEngine) ReleaseNodeIntoProduction(int64, int64) (*store.Order, error)       { return nil, nil }
func (s *stubEngine) SwitchNodeToTarget(int64, int64) error                             { return nil }
func (s *stubEngine) SwitchOperatorStationToTarget(int64, int64) error                   { return nil }
func (s *stubEngine) FlipABNode(int64) error                                            { return nil }

// ── Named DB delegates (Phase 4) ───────────────────────────────────
// Each method below mirrors a method added to *engine.Engine in
// shingo-edge/engine/engine_db_methods.go and forwards to the same
// *store.DB so handler tests exercise the real persistence layer.

func (s *stubEngine) AdminUserExists() (bool, error) { return s.db.AdminUserExists() }
func (s *stubEngine) BuildOperatorStationView(stationID int64) (*store.OperatorStationView, error) {
	return s.db.BuildOperatorStationView(stationID)
}
func (s *stubEngine) ConfirmAnomaly(id int64) error { return s.db.ConfirmAnomaly(id) }
func (s *stubEngine) CreateAdminUser(username, passwordHash string) (int64, error) {
	return s.db.CreateAdminUser(username, passwordHash)
}
func (s *stubEngine) CreateOperatorStation(in store.OperatorStationInput) (int64, error) {
	return s.db.CreateOperatorStation(in)
}
func (s *stubEngine) CreateProcess(name, description, productionState, counterPLC, counterTag string, counterEnabled bool) (int64, error) {
	return s.db.CreateProcess(name, description, productionState, counterPLC, counterTag, counterEnabled)
}
func (s *stubEngine) CreateProcessNode(in store.ProcessNodeInput) (int64, error) {
	return s.db.CreateProcessNode(in)
}
func (s *stubEngine) CreateReportingPoint(plcName, tagName string, styleID int64) (int64, error) {
	return s.db.CreateReportingPoint(plcName, tagName, styleID)
}
func (s *stubEngine) CreateStyle(name, description string, processID int64) (int64, error) {
	return s.db.CreateStyle(name, description, processID)
}
func (s *stubEngine) DeleteOperatorStation(id int64) error { return s.db.DeleteOperatorStation(id) }
func (s *stubEngine) DeleteProcess(id int64) error         { return s.db.DeleteProcess(id) }
func (s *stubEngine) DeleteProcessNode(id int64) error     { return s.db.DeleteProcessNode(id) }
func (s *stubEngine) DeleteReportingPoint(id int64) error  { return s.db.DeleteReportingPoint(id) }
func (s *stubEngine) DeleteShift(shiftNumber int) error    { return s.db.DeleteShift(shiftNumber) }
func (s *stubEngine) DeleteStyle(id int64) error           { return s.db.DeleteStyle(id) }
func (s *stubEngine) DeleteStyleNodeClaim(id int64) error  { return s.db.DeleteStyleNodeClaim(id) }
func (s *stubEngine) DismissAnomaly(id int64) error        { return s.db.DismissAnomaly(id) }
func (s *stubEngine) EnsureProcessNodeRuntime(processNodeID int64) (*store.ProcessNodeRuntimeState, error) {
	return s.db.EnsureProcessNodeRuntime(processNodeID)
}
func (s *stubEngine) GetActiveProcessChangeover(processID int64) (*store.ProcessChangeover, error) {
	return s.db.GetActiveProcessChangeover(processID)
}
func (s *stubEngine) GetAdminUser(username string) (*store.AdminUser, error) {
	return s.db.GetAdminUser(username)
}
func (s *stubEngine) GetOperatorStation(id int64) (*store.OperatorStation, error) {
	return s.db.GetOperatorStation(id)
}
func (s *stubEngine) GetOrder(id int64) (*store.Order, error)             { return s.db.GetOrder(id) }
func (s *stubEngine) GetProcessNode(id int64) (*store.ProcessNode, error) { return s.db.GetProcessNode(id) }
func (s *stubEngine) GetReportingPoint(id int64) (*store.ReportingPoint, error) {
	return s.db.GetReportingPoint(id)
}
func (s *stubEngine) GetStationNodeNames(stationID int64) ([]string, error) {
	return s.db.GetStationNodeNames(stationID)
}
func (s *stubEngine) GetStyle(id int64) (*store.Style, error) { return s.db.GetStyle(id) }
func (s *stubEngine) GetStyleNodeClaim(id int64) (*store.StyleNodeClaim, error) {
	return s.db.GetStyleNodeClaim(id)
}
func (s *stubEngine) HourlyCountTotals(processID int64, countDate string) (map[int]int64, error) {
	return s.db.HourlyCountTotals(processID, countDate)
}
func (s *stubEngine) ListActiveOrders() ([]store.Order, error) { return s.db.ListActiveOrders() }
func (s *stubEngine) ListActiveOrdersByProcess(processID int64) ([]store.Order, error) {
	return s.db.ListActiveOrdersByProcess(processID)
}
func (s *stubEngine) ListChangeoverNodeTasks(changeoverID int64) ([]store.ChangeoverNodeTask, error) {
	return s.db.ListChangeoverNodeTasks(changeoverID)
}
func (s *stubEngine) ListChangeoverStationTasks(changeoverID int64) ([]store.ChangeoverStationTask, error) {
	return s.db.ListChangeoverStationTasks(changeoverID)
}
func (s *stubEngine) ListHourlyCounts(processID, styleID int64, countDate string) ([]store.HourlyCount, error) {
	return s.db.ListHourlyCounts(processID, styleID, countDate)
}
func (s *stubEngine) ListOperatorStations() ([]store.OperatorStation, error) {
	return s.db.ListOperatorStations()
}
func (s *stubEngine) ListOperatorStationsByProcess(processID int64) ([]store.OperatorStation, error) {
	return s.db.ListOperatorStationsByProcess(processID)
}
func (s *stubEngine) ListPayloadCatalog() ([]*store.PayloadCatalogEntry, error) {
	return s.db.ListPayloadCatalog()
}
func (s *stubEngine) ListProcessChangeovers(processID int64) ([]store.ProcessChangeover, error) {
	return s.db.ListProcessChangeovers(processID)
}
func (s *stubEngine) ListProcessNodes() ([]store.ProcessNode, error) { return s.db.ListProcessNodes() }
func (s *stubEngine) ListProcessNodesByProcess(processID int64) ([]store.ProcessNode, error) {
	return s.db.ListProcessNodesByProcess(processID)
}
func (s *stubEngine) ListProcessNodesByStation(stationID int64) ([]store.ProcessNode, error) {
	return s.db.ListProcessNodesByStation(stationID)
}
func (s *stubEngine) ListProcesses() ([]store.Process, error)                     { return s.db.ListProcesses() }
func (s *stubEngine) ListReportingPoints() ([]store.ReportingPoint, error)         { return s.db.ListReportingPoints() }
func (s *stubEngine) ListShifts() ([]store.Shift, error)                           { return s.db.ListShifts() }
func (s *stubEngine) ListStyleNodeClaims(styleID int64) ([]store.StyleNodeClaim, error) {
	return s.db.ListStyleNodeClaims(styleID)
}
func (s *stubEngine) ListStyles() ([]store.Style, error) { return s.db.ListStyles() }
func (s *stubEngine) ListStylesByProcess(processID int64) ([]store.Style, error) {
	return s.db.ListStylesByProcess(processID)
}
func (s *stubEngine) ListUnconfirmedAnomalies() ([]store.CounterSnapshot, error) {
	return s.db.ListUnconfirmedAnomalies()
}
func (s *stubEngine) MoveOperatorStation(id int64, direction string) error {
	return s.db.MoveOperatorStation(id, direction)
}
func (s *stubEngine) SetActiveStyle(processID int64, styleID *int64) error {
	return s.db.SetActiveStyle(processID, styleID)
}
func (s *stubEngine) SetStationNodes(stationID int64, nodeNames []string) error {
	return s.db.SetStationNodes(stationID, nodeNames)
}
func (s *stubEngine) TouchOperatorStation(id int64, healthStatus string) error {
	return s.db.TouchOperatorStation(id, healthStatus)
}
func (s *stubEngine) UpdateAdminPassword(username, passwordHash string) error {
	return s.db.UpdateAdminPassword(username, passwordHash)
}
func (s *stubEngine) UpdateOperatorStation(id int64, in store.OperatorStationInput) error {
	return s.db.UpdateOperatorStation(id, in)
}
func (s *stubEngine) UpdateOrderFinalCount(id int64, finalCount int64, confirmed bool) error {
	return s.db.UpdateOrderFinalCount(id, finalCount, confirmed)
}
func (s *stubEngine) UpdateProcess(id int64, name, description, productionState, counterPLC, counterTag string, counterEnabled bool) error {
	return s.db.UpdateProcess(id, name, description, productionState, counterPLC, counterTag, counterEnabled)
}
func (s *stubEngine) UpdateProcessNode(id int64, in store.ProcessNodeInput) error {
	return s.db.UpdateProcessNode(id, in)
}
func (s *stubEngine) UpdateProcessNodeRuntimeOrders(processNodeID int64, activeOrderID, stagedOrderID *int64) error {
	return s.db.UpdateProcessNodeRuntimeOrders(processNodeID, activeOrderID, stagedOrderID)
}
func (s *stubEngine) UpdateReportingPoint(id int64, plcName, tagName string, styleID int64, enabled bool) error {
	return s.db.UpdateReportingPoint(id, plcName, tagName, styleID, enabled)
}
func (s *stubEngine) UpdateStyle(id int64, name, description string, processID int64) error {
	return s.db.UpdateStyle(id, name, description, processID)
}
func (s *stubEngine) UpsertShift(shiftNumber int, name, startTime, endTime string) error {
	return s.db.UpsertShift(shiftNumber, name, startTime, endTime)
}
func (s *stubEngine) UpsertStyleNodeClaim(in store.StyleNodeClaimInput) (int64, error) {
	return s.db.UpsertStyleNodeClaim(in)
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
		engine:   eng,
		sessions: newSessionStore(""),
		eventHub: NewEventHub(),
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
	id, err := testDB.CreateOperatorStation(store.OperatorStationInput{
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
	in := store.ProcessNodeInput{
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
