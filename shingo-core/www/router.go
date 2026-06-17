package www

import (
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"

	"shingo/protocol/debuglog"
	"shingo/shared"
	"shingocore/engine"
)

// Handlers holds dependencies for HTTP handlers.
//
// Phase 6.5 (2026-04-25) split the engine dependency into two fields
// of different interface types so that compile-time enforcement
// constrains where orchestration verbs can be reached:
//
//   - h.engine (ServiceAccess) — narrow surface, ~25 methods. CRUD-only
//     handlers and read-only state queries use this. Calling
//     orchestration verbs through h.engine fails to compile because
//     those methods are not on ServiceAccess.
//   - h.orchestration (EngineOrchestration) — wide surface adding 12
//     verbs (corrections, direct orders, scene sync, cross-edge
//     messaging, live reconfig). Embeds ServiceAccess so it can also
//     reach service accessors and state queries.
//
// In production both fields point to the same *engine.Engine. In tests
// they may differ (a service-only test fixture can leave orchestration
// nil so any accidental orchestration call panics with a clear stack).
type Handlers struct {
	engine        ServiceAccess
	orchestration EngineOrchestration
	sessions      *sessions.CookieStore
	tmpls         map[string]*template.Template
	eventHub      *EventHub
	debugLog      *debuglog.Logger
}

// NewRouter registers all HTTP endpoints for shingo-core.
//
// To find a handler: grep for the URL path → handler func name → handlers_*.go.
//
// Route layout:
//
//	/events                — SSE stream (outside compression middleware)
//	/                      — Public pages (dashboard, login, nodes, orders, robots, etc.)
//	/api/* (public)        — Read-only JSON API (nodes, orders, bins, payloads, telemetry)
//	/api/* (protected)     — Write API (test orders, payloads, bins, nodegroups, fleet, recovery)
//	/* (protected)         — Admin pages (test-orders, config, diagnostics, CRUD forms)
//
// Auth boundary: h.requireAuth middleware. Public = shop floor read access.
// Handlers live in handlers_*.go files grouped by domain (bins, nodes, payloads, etc.).
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger) (http.Handler, func(), error) {
	hub := NewEventHub()
	hub.Start()
	hub.SetupEngineListeners(eng)

	dbg.SetOnEntry(func(e debuglog.Entry) {
		hub.Broadcast("debug-log", sseJSON(e))
	})

	sessionStore := newSessionStore(eng.AppConfig().Web.SessionSecret)

	// Parse layout + partials as a base template set. Each page is cloned separately
	// to avoid the "last define wins" problem with {{define "content"}}.
	base := template.New("").Funcs(templateFuncs())
	base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))

	// Discover page templates via fs.Glob — new templates are picked up
	// automatically without code changes. Layout is the base, not a page.
	pages, err := fs.Glob(templateFS, "templates/*.html")
	if err != nil {
		return nil, nil, fmt.Errorf("glob templates: %w", err)
	}
	tmpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		name := p[len("templates/"):]
		if name == "layout.html" {
			continue // layout is the base template, not a page
		}
		clone := template.Must(base.Clone())
		clone = template.Must(clone.ParseFS(templateFS, p))
		tmpls[name] = clone
	}

	h := &Handlers{
		engine:        eng, // ServiceAccess — narrow surface for CRUD handlers
		orchestration: eng, // EngineOrchestration — wide surface for flow handlers
		sessions:      sessionStore,
		tmpls:         tmpls,
		eventHub:      hub,
		debugLog:      dbg,
	}

	h.ensureDefaultAdmin()

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// SSE — must be outside compression middleware. Compression buffers
	// defeat streaming flushes and cause stale connection buildup when
	// navigating between pages.
	r.Get("/events", hub.SSEHandler)

	// Everything else gets compressed
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))

		// Shared UI assets (tokens.css, status-classes.css, utils.js)
		// from the shingo/shared module. Registered BEFORE /static/* so
		// the more specific prefix wins.
		r.Handle("/static/shared/*", http.StripPrefix("/static/shared/",
			http.FileServer(http.FS(shared.Files)),
		))

		// Static files
		staticSub, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

		// ── Public pages ───────────────────────────────────────
		// Wave 2 (Q-035): "/" is now the Operations Overview (the snapshot page).
		// SB's call (2026-06-10): the original Dashboard (active-orders /
		// system-health) landing stays the home page; Overview lives in the
		// Dashboards dropdown at /overview.
		r.Get("/", h.handleDashboard)
		r.Get("/overview", h.handleOverview)
		r.Get("/login", h.handleLoginPage)
		r.Post("/login", h.handleLogin)
		r.Get("/logout", h.handleLogout)
		r.Get("/nodes", h.handleNodes)
		r.Get("/orders", h.handleOrders)
		r.Get("/orders/detail", h.handleOrderDetail)
		r.Get("/robots", h.handleRobots)
		r.Get("/inventory", h.handleInventory)
		r.Get("/demand", h.handleDemand)
		r.Get("/missions", h.handleMissions)
		r.Get("/missions/{orderID}", h.handleMissionDetail)
		r.Get("/traffic", h.handleTraffic)
		// Dashboard platform: chromeless per-instance display for wall
		// monitors (public, no nav). The old /board tab is superseded —
		// redirect bookmarks to the management page.
		r.Get("/dashboard/{id}", h.handleDashboardDisplay)
		// Production-heartbeat kiosk (Phase F): chromeless wall display of cell
		// rhythm. Public, no nav — open full-screen on a floor monitor.
		r.Get("/heartbeat", h.handleHeartbeatKiosk)
		r.Get("/board", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboards", http.StatusMovedPermanently)
		})
		// Dashboards-dropdown links resolve to the first enabled board of a kind.
		r.Get("/board/{kind}", h.handleBoardKindRedirect)

		// ── API routes ─────────────────────────────────────────
		r.Route("/api", func(r chi.Router) {

			// ── Public API (read-only, no auth) ────────────────

			// Dev sim control (live speed toggle). Sim builds only — a
			// no-op stub in production (sim_routes_stub.go).
			h.registerSimRoutes(r)

			// Nodes
			r.Get("/nodes", h.apiListNodes)
			r.Get("/nodes/inventory", h.apiNodePayloads)
			r.Get("/nodes/occupancy", h.apiNodeOccupancy)
			r.Get("/nodes/detail", h.apiNodeDetail)
			r.Get("/nodes/bin-types", h.apiGetNodeBinTypes)
			r.Get("/nodestate", h.apiNodeState)
			// Loaders are part of the node layout (shop-floor read access) — the
			// box render reads this; all loader WRITES stay auth-gated below.
			r.Get("/loader/list", h.apiListLoaders)
			r.Get("/fleet/robot-groups", h.apiRobotGroups)
			r.Get("/map/points", h.apiScenePoints)
			r.Get("/map/edges", h.apiSceneEdges)
			r.Get("/stations", h.apiStations)
			r.Get("/plant/timezone", h.apiPlantTimezone)

			// Orders & missions
			r.Get("/orders", h.apiListOrders)
			r.Get("/orders/detail", h.apiGetOrder)
			r.Get("/orders/enriched", h.apiGetOrderEnriched)
			r.Get("/dispatch/preview-capacity", h.apiPreviewDropoffCapacity)
			r.Get("/dispatch/anomalies", h.apiListTransitAnomalies)
			r.Get("/missions", h.apiListMissions)
			r.Get("/missions/stats", h.apiMissionStats)
			r.Get("/missions/stats/v2", h.apiMissionStatsV2)
			r.Get("/missions/active", h.apiMissionsActive)
			r.Get("/missions/alerts", h.apiMissionsAlerts)
			r.Get("/missions/timeseries", h.apiMissionTimeseries)
			r.Get("/missions/breakdown", h.apiMissionBreakdown)
			r.Get("/missions/failures", h.apiMissionFailures)
			r.Get("/missions/{orderID}", h.apiGetMission)

			// Robots
			r.Get("/robots", h.apiRobotsStatus)
			r.Get("/robots/fleet", h.apiRobotsFleet)

			// Operations Overview (plant footprint)
			r.Get("/footprint", h.apiFootprint)

			// Cells — production heartbeat (slice 5, §12) + cell config (Phase E)
			r.Get("/cells", h.apiCellsList)
			r.Get("/cells/catalog", h.apiCellsCatalog) // Q-034 auto-derived catalog

			r.Get("/cells/{id}/heartbeat", h.apiCellHeartbeat)
			r.Get("/cells/{id}/stops", h.apiCellStops)
			r.Get("/cells/{id}/state", h.apiCellState)

			// Parts (produced / cycle time / consumption)
			r.Get("/parts/produced", h.apiPartsProduced)
			r.Get("/parts/cycle-time", h.apiPartsCycleTime)
			r.Get("/parts/consumption", h.apiPartsConsumption)

			// Board
			r.Get("/board/orders", h.handleBoardOrders)

			// Dashboards (read) — public so a wall display (or a future
			// standalone display host) can fetch definitions without auth.
			r.Get("/dashboards", h.apiListDashboards)
			r.Get("/dashboards/{id}", h.apiGetDashboard)
			r.Get("/dashboards/{id}/cells", h.apiDashboardCells) // refactor #4: per-dashboard heartbeat cells
			r.Get("/dashboards/{id}/node-report", h.apiDashboardNodeReport)

			// Payloads & manifest
			r.Get("/payloads/templates", h.apiListPayloads)
			r.Get("/payloads/templates/manifest", h.apiGetPayloadManifestTemplate)
			r.Get("/payloads/templates/bin-types", h.apiGetPayloadBinTypes)
			r.Get("/payloads", h.apiListPayloads)
			r.Get("/payloads/detail", h.apiGetPayload)
			r.Get("/payloads/manifest", h.apiListManifest)
			r.Get("/payloads/by-node", h.apiPayloadsByNode)

			// Bins
			r.Get("/bins/by-node", h.apiBinsByNode)
			r.Get("/bins/available", h.apiListAvailableBins)
			r.Get("/bins/detail", h.apiBinDetail)

			// Traffic (count groups)
			r.Get("/traffic/groups", h.apiTrafficGroups)

			// Telemetry
			r.Get("/telemetry/node-bins", h.apiTelemetryNodeBins)
			r.Get("/telemetry/uop-state", h.apiTelemetryUOPState)
			r.Get("/telemetry/payload/{code}/manifest", h.apiTelemetryPayloadManifest)
			r.Get("/telemetry/node/{name}/children", h.apiTelemetryNodeChildren)
			r.Post("/telemetry/bin-load", h.apiBinLoad)
			r.Post("/telemetry/bin-clear", h.apiBinClear)
			r.Get("/telemetry/e-maint", h.apiEMaintRobotTelemetry)
			r.Get("/telemetry/e-maint/download", h.apiEMaintRobotTelemetryDownload)

			// Inventory & diagnostics
			r.Get("/inventory", h.apiInventory)
			r.Get("/inventory/invariant", h.apiInventoryInvariant)
			r.Post("/inventory/preflight", h.apiInventoryPreflight)
			r.Post("/inventory/system-count", h.apiInventorySystemCount)
			r.Get("/buckets", h.apiBuckets)
			r.Post("/buckets/delete", h.apiBucketDelete)

			// Audit (Item 10) — bin_uop_audit read endpoints
			r.Get("/audit/bin/{id}", h.apiAuditBinTimeline)
			r.Get("/audit/operator/{name}", h.apiAuditOperatorActivity)
			r.Get("/audit/station/{station}", h.apiAuditStationOverrides)
			r.Get("/corrections", h.apiListNodeCorrections)
			r.Get("/cms-transactions", h.apiListCMSTransactions)
			r.Get("/outbox/deadletters", h.apiListDeadLetterOutbox)
			r.Get("/reconciliation", h.apiReconciliation)
			r.Get("/recovery/actions", h.apiListRecoveryActions)
			r.Get("/health", h.apiHealthCheck)

			// Demands
			r.Get("/demands", h.apiListDemands)
			r.Get("/demands/{id}/log", h.apiDemandLog)

			// ── Protected API (auth required) ──────────────────
			r.Group(func(r chi.Router) {
				r.Use(h.requireAuth)

				// Inventory export
				r.Get("/inventory/export", h.apiInventoryExport)

				// Cells — production-cell config (Phase E, Q-025)
				r.Get("/cells/processes", h.apiCellProcesses)
				r.Post("/cells", h.apiCellUpsert)
				r.Delete("/cells/{id}", h.apiCellDelete)

				// Edges — ask edge(s) to re-send their registration + catalog (Q-034)
				r.Post("/edges/reregister", h.apiEdgeReregister)

				// Node management
				r.Post("/nodes/generate-test", h.apiGenerateTestNodes)
				r.Post("/nodes/delete-test", h.apiDeleteTestNodes)
				r.Post("/nodes/bin-types", h.apiSetNodeBinTypes)
				r.Post("/nodes/properties/set", h.apiNodePropertySet)
				r.Post("/nodes/properties/delete", h.apiNodePropertyDelete)
				r.Post("/nodes/reparent", h.apiReparentNode)

				// Test orders (Kafka path)
				r.Get("/test-orders", h.apiTestOrdersList)
				r.Get("/test-orders/detail", h.apiTestOrderDetail)
				r.Post("/test-orders/submit", h.apiTestOrderSubmit)
				r.Post("/test-orders/submit/complex", h.apiKafkaComplexOrderSubmit)
				r.Post("/test-orders/cancel", h.apiTestOrderCancel)
				r.Post("/test-orders/receipt", h.apiTestOrderReceipt)
				r.Get("/test-orders/robots", h.apiTestRobots)
				r.Get("/test-orders/scene-points", h.apiTestScenePoints)

				// Test orders (direct dispatch path)
				r.Get("/test-orders/direct", h.apiDirectOrdersList)
				r.Post("/test-orders/direct", h.apiDirectOrderSubmit)
				r.Post("/test-orders/direct/complex", h.apiDirectComplexOrderSubmit)
				r.Post("/test-orders/direct/release", h.apiDirectOrderRelease)
				r.Post("/test-orders/direct/receipt", h.apiDirectOrderReceipt)

				// Test commands
				r.Post("/test-commands/submit", h.apiTestCommandSubmit)
				r.Post("/test-commands/cancel", h.apiTestCommandCancel)
				r.Get("/test-commands", h.apiTestCommandsList)
				r.Get("/test-commands/status", h.apiTestCommandStatus)

				// Payload templates
				r.Post("/payloads/templates/create", h.apiCreatePayloadTemplate)
				r.Post("/payloads/templates/update", h.apiUpdatePayloadTemplate)
				r.Post("/payloads/templates/manifest", h.apiSavePayloadManifestTemplate)
				r.Post("/payloads/templates/bin-types", h.apiSavePayloadBinTypes)

				// Manifest items
				r.Post("/payloads/manifest/create", h.apiCreateManifestItem)
				r.Post("/payloads/manifest/update", h.apiUpdateManifestItem)
				r.Post("/payloads/manifest/delete", h.apiDeleteManifestItem)
				r.Post("/payloads/confirm-manifest", h.apiConfirmManifest)
				r.Get("/payloads/events", h.apiListPayloadEvents)

				// Bins
				r.Post("/bins/bulk-register", h.apiBulkRegisterBins)
				r.Post("/bins/action", h.apiBinAction)
				r.Post("/bins/bulk-action", h.apiBulkBinAction)
				r.Post("/bins/request-transport", h.apiRequestBinTransport)

				// Node groups
				r.Post("/nodegroup/create", h.apiCreateNodeGroup)
				r.Get("/nodegroup/layout", h.apiGetGroupLayout)
				r.Post("/nodegroup/delete", h.apiDeleteNodeGroup)
				r.Post("/nodegroup/add-lane", h.apiAddLane)
				r.Post("/nodegroup/reorder-lane", h.apiReorderLaneSlots)
				r.Post("/loader/create", h.apiCreateLoader)
				r.Post("/loader/update", h.apiUpdateLoader)
				r.Post("/loader/set-payload", h.apiSetLoaderPayload)
				r.Post("/loader/set-home", h.apiSetLoaderHome)
				r.Post("/loader/remove-home", h.apiRemoveLoaderHome)
				r.Post("/loader/reorder-homes", h.apiReorderLoaderHomes)
				r.Post("/loader/remove-payload", h.apiRemoveLoaderPayload)
				r.Post("/loader/delete", h.apiDeleteLoader)
				r.Post("/loader/calculate", h.apiCalculateThreshold)
				// NOTE: GET /loader/list is registered in the PUBLIC block above
				// (loaders render read-only on the shop-floor Nodes page).

				// Corrections
				r.Post("/corrections/create", h.apiCreateCorrection)
				r.Post("/corrections/batch", h.apiApplyBatchCorrection)

				// Fleet
				r.Post("/fleet/proxy", h.apiFleetProxy)

				// Robots
				r.Post("/robots/availability", h.apiRobotSetAvailability)
				r.Post("/robots/retry", h.apiRobotRetryFailed)
				r.Post("/robots/force-complete", h.apiRobotForceComplete)

				// Orders
				r.Post("/orders/terminate", h.apiTerminateOrder)
				r.Post("/orders/priority", h.apiSetOrderPriority)
				r.Post("/orders/spot", h.apiManualOrderSubmit)
				r.Post("/dispatch/clear-anomaly", h.apiClearTransitAnomaly)

				// Outbox & recovery
				r.Post("/outbox/replay", h.apiReplayOutbox)
				r.Post("/recovery/repair", h.apiRepairAnomaly)

				// Fire alarm
				r.Get("/fire-alarm/status", h.apiFireAlarmStatus)
				r.Post("/fire-alarm/trigger", h.apiFireAlarmTrigger)

				// Demands
				r.Post("/demands", h.apiCreateDemand)
				r.Put("/demands/{id}", h.apiUpdateDemand)
				r.Put("/demands/{id}/apply", h.apiApplyDemand)
				r.Delete("/demands/{id}", h.apiDeleteDemand)
				r.Post("/demands/apply-all", h.apiApplyAllDemands)
				r.Put("/demands/{id}/produced", h.apiSetDemandProduced)
				r.Post("/demands/{id}/clear", h.apiClearDemandProduced)
				r.Post("/demands/clear-all", h.apiClearAllProduced)

				// Dashboards (write) — management CRUD behind auth. Reads
				// live in the public API group above.
				r.Post("/dashboards", h.apiCreateDashboard)
				r.Put("/dashboards/{id}", h.apiUpdateDashboard)
				r.Delete("/dashboards/{id}", h.apiDeleteDashboard)
			})
		})

		// ── Protected pages (auth required) ────────────────────
		r.Group(func(r chi.Router) {
			r.Use(h.requireAuth)

			// Admin pages
			r.Get("/test-orders", h.handleTestOrders)
			r.Get("/payloads", h.handlePayloadsPage)
			r.Get("/bins", h.handleBins)
			r.Get("/diagnostics", h.handleDiagnostics)
			r.Get("/config", h.handleConfig)
			r.Post("/config/save", h.handleConfigSave)
			r.Get("/fleet-explorer", h.handleFleetExplorer)
			r.Get("/dashboards", h.handleDashboardsAdmin)
			r.Get("/admin/cells", h.handleCellsAdmin)

			// Traffic (count group CRUD)
			r.Post("/traffic/save", h.handleTrafficSave)
			r.Post("/traffic/add", h.handleTrafficAdd)
			r.Post("/traffic/delete", h.handleTrafficDelete)

			// Node CRUD
			r.Post("/nodes/create", h.handleNodeCreate)
			r.Post("/nodes/update", h.handleNodeUpdate)
			r.Post("/nodes/delete", h.handleNodeDelete)
			r.Post("/nodes/sync-fleet", h.handleNodeSyncFleet)
			r.Post("/nodes/sync-scene", h.handleSceneSync)

			// Payload CRUD
			r.Post("/payloads/create", h.handlePayloadCreate)
			r.Post("/payloads/update", h.handlePayloadUpdate)
			r.Post("/payloads/delete", h.handlePayloadDelete)

			// Bin & bin-type CRUD
			r.Post("/bin-types/create", h.handleBinTypeCreate)
			r.Post("/bin-types/update", h.handleBinTypeUpdate)
			r.Post("/bin-types/delete", h.handleBinTypeDelete)
			r.Post("/bins/create", h.handleBinCreate)
			r.Post("/bins/retire", h.handleBinRetire)
		})
	}) // end compression group (wraps all routes except SSE)

	stopFn := func() {
		hub.Stop()
	}

	return r, stopFn, nil
}

func (h *Handlers) render(w http.ResponseWriter, r *http.Request, name string, data map[string]any) {
	tmpl, ok := h.tmpls[name]
	if !ok {
		log.Printf("render: template %q not found", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	if _, exists := data["Authenticated"]; !exists {
		data["Authenticated"] = h.isAuthenticated(r)
	}
	// Never cache the HTML shell: it carries auth-gated markup + cache-busted
	// script tags that change on every deploy. Without this the browser (or a
	// service worker) serves a stale page after a rebuild — e.g. a new toolbar
	// button that's deployed but invisible until the user clears cache.
	w.Header().Set("Cache-Control", "no-store, must-revalidate")
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

// renderBare renders a standalone (chromeless) page by executing the
// template named by its own file rather than the shared "layout". Used for
// kiosk/display surfaces — the dashboard displays — that must not carry the
// admin nav. The template is a full <!DOCTYPE> document with no
// {{define "content"}} wrapper.
func (h *Handlers) renderBare(w http.ResponseWriter, name string, data map[string]any) {
	tmpl, ok := h.tmpls[name]
	if !ok {
		log.Printf("renderBare: template %q not found", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("renderBare %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
