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
//   - h.engine (ServiceAccess) — narrow surface, 49 methods. CRUD-only
//     handlers and read-only state queries use this. Calling
//     orchestration verbs through h.engine fails to compile because
//     those methods are not on ServiceAccess.
//   - h.orchestration (EngineOrchestration) — wide surface adding 13
//     verbs (corrections, direct orders, scene sync, cross-edge
//     messaging, live reconfig). Embeds ServiceAccess so it can also
//     reach service accessors and state queries.
//
// Both counts measured 2026-08-19; they read "~25" and "12" from the
// 6.5 split onwards and were never re-measured. engine_iface_width_test.go
// asserts them now.
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
	base := template.New("").Funcs(templateFuncs(eng.NodeService()))
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
			staticCache(http.FileServer(http.FS(shared.Files))),
		))

		// Static files
		staticSub, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix("/static/", staticCache(http.FileServer(http.FS(staticSub)))))

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
		// The board's row fragment. Same filter params as /orders; used by the
		// SSE handler to refresh rows instead of reloading the page.
		r.Get("/orders/rows", h.handleOrdersRows)
		r.Get("/orders/detail", h.handleOrderDetail)
		r.Get("/robots", h.handleRobots)
		r.Get("/inventory", h.handleInventory)
		r.Get("/demand", h.handleDemand)
		// Phase 6: the demand GRAIN, a different concept from the quota page
		// above. Distinct path on purpose — see handlers_demand_episodes.go.
		r.Get("/demand-episodes", h.handleDemandEpisodes)
		// 5.12 — origin-indexed forensics: one demand and every order it
		// spawned, each linking to its /missions/{orderID} detail below.
		r.Get("/demand-episodes/{originID}", h.handleDemandEpisodeDetail)
		// Phase 6 (5.7): orders that should have carried an origin and did not.
		// A different GRAIN from the page above — an orphan has no episode by
		// definition, so it has no row there. See handlers_orphans.go.
		r.Get("/orphans", h.handleOrphans)
		// Phase 6 (5.10): cycle time from the applied-BinUOPDelta audit trail.
		r.Get("/cycle-time", h.handleCycleTime)
		// Phase 6 (5.11): the two readings a starved cell can produce, kept
		// apart — an open demand episode past its worry line, and a carrier
		// whose binding ShinGo has held long enough for the count to have
		// drifted. NOT "material downtime": nothing here records whether a line
		// stopped, and a negative ledger is a cycle count. See
		// handlers_material_flags.go.
		r.Get("/material-flags", h.handleMaterialFlags)
		r.Get("/missions", h.handleMissions)
		r.Get("/missions/{orderID}", h.handleMissionDetail)
		// Wall displays: the per-instance display for a floor monitor
		// (public, no nav). Framed by default so a person clicking from the
		// hub keeps Core's chrome; ?kiosk=1 is the chromeless page a monitor
		// actually loads.
		//
		// THE NAME BOUNDARY IS DELIBERATE AND IT IS ONLY HERE. Owner decision
		// 11 renamed what a passer-by reads off a screen, so the PAGE routes
		// moved. The stored entity is still a `dashboard`: the table, the API
		// namespace under /api, DashboardService and domain.Dashboard all keep
		// that name. Renaming those serves nothing a passer-by can see and
		// would break every kiosk and script holding an API URL — and unlike a
		// page rename, an API rename is not undoable with a redirect.
		r.Get("/wall-display/{id}", h.handleWallDisplay)
		// The old path, 301 and CARRYING THE QUERY STRING. See
		// handleWallDisplayMoved: dropping it silently reframes every monitor.
		r.Get("/dashboard/{id}", h.handleWallDisplayMoved)
		// Production-heartbeat kiosk (Phase F): chromeless wall display of cell
		// rhythm. Public, no nav — open full-screen on a floor monitor.
		r.Get("/heartbeat", h.handleHeartbeatKiosk)
		r.Get("/board", func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, "/dashboards", http.StatusMovedPermanently)
		})
		// Both land on the hub at "/", which is where wall displays are made
		// and edited. /wall-displays exists so the URL owner decision 11 names
		// resolves to something; /dashboards is the old one.
		//
		// PUBLIC, and /dashboards MOVED HERE to be. It sat in the auth group,
		// which meant a route whose entire body is `Redirect(w, r, "/")` —
		// to handleDashboard, registered public in this same group — demanded
		// a login first.
		// The visible effect is on /board, the old public board URL: its 301
		// lands on /dashboards, so a shop-floor bookmark walked into a login
		// wall on the way to a page it was always allowed to see. Nothing is
		// exposed by this; the destination was already public and the handler
		// reads nothing.
		r.Get("/wall-displays", h.handleWallDisplaysMoved)
		r.Get("/dashboards", h.handleWallDisplaysMoved)
		// First enabled wall display of a kind, for a typed or bookmarked URL.
		// NOT a nav dropdown — the comment that said so outlived the dropdown;
		// nothing in layout.html has linked here for some time.
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
			// Maintained-group config: read beside the other node reads, written
			// under auth below. The section is admin-only on screen; the read is
			// public for the same reason /nodes/detail is — it is what the node
			// IS, and the shop floor gets to look at that.
			r.Get("/nodes/maintained-group", h.apiMaintainedGroup)
			r.Get("/nodes/process-options", h.apiMaintainedGroupProcessOptions)
			r.Get("/nodestate", h.apiNodeState)
			// Loaders are part of the node layout (shop-floor read access) — the
			// box render reads this; all loader WRITES stay auth-gated below.
			r.Get("/loader/list", h.apiListLoaders)
			r.Get("/bin-types", h.apiListBinTypes)
			r.Get("/fleet/robot-groups", h.apiRobotGroups)
			r.Get("/map/points", h.apiScenePoints)
			// The waiting-point picker's view of the same data: slim and searchable.
			r.Get("/map/marks", h.apiSceneMarks)
			r.Get("/nodes/lane-waiting", h.apiLaneWaiting)
			r.Get("/nodes/lane-gate-points", h.apiLaneGatePoints)
			r.Get("/map/edges", h.apiSceneEdges)
			// Structure lives beside structure. Areas and reflectors are not
			// owned by the page that first needed them.
			r.Get("/map/areas", h.apiSceneAreas)
			r.Get("/map/reflectors", h.apiSceneReflectors)
			r.Get("/map/diffs", h.apiSceneDiffs)
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
			r.Get("/missions/dwell", h.apiMissionDwell)
			r.Get("/missions/failures", h.apiMissionFailures)
			r.Get("/missions/{orderID}", h.apiGetMission)

			// Robots
			r.Get("/robots", h.apiRobotsStatus)
			r.Get("/robots/fleet", h.apiRobotsFleet)
			// The localization overlay for the robots page: lane state, zones,
			// reflectors and the map change log. Geometry stays on /api/map/edges.
			r.Get("/robots/localization", h.apiLocalizationBoard)
			// The change annotation for one lane, on demand -- most lanes have
			// never been edited, so it is not folded into the board payload.
			r.Get("/robots/lane-change", h.apiLaneChange)

			// Operations Overview (plant footprint)
			r.Get("/footprint", h.apiFootprint)

			// Cells — production heartbeat (slice 5, §12) + cell config (Phase E)
			r.Get("/cells", h.apiCellsList)
			r.Get("/cells/catalog", h.apiCellsCatalog) // Q-034 auto-derived catalog

			r.Get("/cells/{id}/heartbeat", h.apiCellHeartbeat)
			r.Get("/cells/{id}/stops", h.apiCellStops)
			r.Get("/cells/{id}/state", h.apiCellState)

			// Parts (produced / carrying-mission duration / consumption)
			//
			// "mission-duration" AND NOT "cycle-time", WHICH IS WHAT IT USED TO
			// BE CALLED. This endpoint averages mission_telemetry.duration_ms:
			// how long a robot took to carry a payload, attributed to each part
			// in that payload's manifest. /cycle-time (5.10) measures the
			// interval between consecutive PLC ticks for one part at one
			// station. One order's journey against one part crossing a station
			// — different table, different grain, different key, and only the
			// second is a cycle time.
			//
			// RENAMED RATHER THAN ALIASED. A second name for one thing is the
			// disease, not the cure. It is safe to move: nothing in this
			// repository fetches it — inventory.js consumes its two siblings,
			// /parts/produced and /parts/consumption, and not this one — and its
			// only reference anywhere is a planning document. An external client
			// nobody has told us about would break, and that is the accepted
			// cost of settling this while /cycle-time is still new.
			r.Get("/parts/produced", h.apiPartsProduced)
			r.Get("/parts/mission-duration", h.apiPartsMissionDuration)
			r.Get("/parts/consumption", h.apiPartsConsumption)

			// Board
			r.Get("/board/orders", h.handleBoardOrders)

			// Dashboards (read) — public so a wall display (or a future
			// standalone display host) can fetch definitions without auth.
			//
			// STILL `dashboards`, on purpose, after the page routes became
			// /wall-display. This is the stored entity's name and it matches
			// the table; a URL a monitor or a script holds is not a thing a
			// passer-by reads, which is all decision 11 was about.
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

			// Telemetry
			r.Get("/telemetry/node-bins", h.apiTelemetryNodeBins)
			r.Get("/telemetry/uop-state", h.apiTelemetryUOPState)
			r.Get("/telemetry/payload/{code}/manifest", h.apiTelemetryPayloadManifest)
			r.Get("/telemetry/node/{name}/children", h.apiTelemetryNodeChildren)
			r.Post("/telemetry/bin-load", h.apiBinLoad)
			r.Post("/telemetry/bin-clear", h.apiBinClear)
			r.Post("/telemetry/bin-count", h.apiBinCount)
			r.Get("/telemetry/e-maint", h.apiEMaintRobotTelemetry)
			r.Get("/telemetry/e-maint/download", h.apiEMaintRobotTelemetryDownload)

			// Inventory & diagnostics
			r.Get("/inventory", h.apiInventory)
			r.Get("/inventory/monitor-totals", h.apiInventoryMonitorTotals)
			r.Get("/inventory/anomaly-summary", h.apiInventoryAnomalySummary)
			r.Get("/inventory/ledger-exceptions", h.apiInventoryLedgerExceptions)
			r.Get("/inventory/maintained-groups", h.apiInventoryMaintainedGroups)
			r.Get("/sourceability/events", h.apiSourceabilityEvents)
			r.Get("/core/health", h.apiCoreHealth)
			r.Get("/inventory/rejected-deltas", h.apiInventoryRejectedDeltas)
			r.Get("/inventory/invariant", h.apiInventoryInvariant)
			r.Post("/inventory/preflight", h.apiInventoryPreflight)
			r.Post("/inventory/system-count", h.apiInventorySystemCount)
			r.Get("/buckets", h.apiBuckets)
			r.Post("/buckets/delete", h.apiBucketDelete)

			// Audit (Item 10) — bin_uop_ledger read endpoints
			r.Get("/audit/bin/{id}", h.apiAuditBinTimeline)
			r.Get("/audit/discrepancies", h.apiAuditDiscrepancies)
			r.Get("/corrections", h.apiListNodeCorrections)
			r.Get("/cms-transactions", h.apiListCMSTransactions)
			r.Get("/outbox/deadletters", h.apiListDeadLetterOutbox)
			r.Get("/reconciliation", h.apiReconciliation)
			r.Get("/recovery/actions", h.apiListRecoveryActions)
			r.Get("/health", h.apiHealthCheck)

			// Demands
			r.Get("/demands", h.apiListDemands)

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

				// Edge identity (v66). See handlers_edges.go for why there is
				// no "re-issue" endpoint: handing an existing uid to
				// replacement hardware is the operator reading it off this
				// list, and Core still holding it is the entire design.
				r.Get("/edges", h.apiEdges)
				r.Post("/edges/enroll", h.apiEdgeEnroll)
				r.Post("/edges/claim", h.apiEdgeClaim)
				r.Post("/edges/rename", h.apiEdgeRename)
				r.Post("/edges/rebind", h.apiEdgeRebind)

				// Node management
				r.Post("/nodes/generate-test", h.apiGenerateTestNodes)
				r.Post("/nodes/delete-test", h.apiDeleteTestNodes)
				r.Post("/nodes/bin-types", h.apiSetNodeBinTypes)
				r.Post("/nodes/properties/set", h.apiNodePropertySet)
				// Maintained groups: one endpoint per thing an operator edits.
				// A single save-everything call would have to decide what an
				// omitted field means, and both answers are wrong — one deletes
				// a level when the form fails to populate, the other makes
				// clearing impossible.
				r.Post("/nodes/maintained-group/check-types", h.apiMaintainedGroupCheckTypes)
				r.Post("/nodes/maintained-group/settings", h.apiMaintainedGroupSettingsSet)
				r.Post("/nodes/maintained-group/level", h.apiMaintainedGroupLevelSet)
				r.Post("/nodes/maintained-group/level/remove", h.apiMaintainedGroupLevelRemove)
				r.Post("/nodes/maintained-group/supports", h.apiMaintainedGroupSupportsSet)
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
				// Advanced load sequences: dropdown source + on-demand Check.
				r.Get("/payloads/templates/sequences", h.apiListLoadSequences)
				r.Get("/payloads/templates/check-sequence", h.apiCheckLoadSequence)

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
				r.Post("/loader/set-quota", h.apiSetLoaderQuota)
				r.Post("/loader/remove-quota", h.apiRemoveLoaderQuota)
				r.Post("/loader/set-window-bin-types", h.apiSetWindowBinTypes)
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
				r.Post("/robots/move", h.apiRobotMoveTo)

				// Orders
				r.Post("/orders/terminate", h.apiTerminateOrder)
				r.Post("/orders/hard-release", h.apiHardReleaseOrder)
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
			r.Get("/sourcing", h.handleSourcing)
			r.Get("/bins", h.handleBins)
			r.Get("/diagnostics", h.handleDiagnostics)
			r.Get("/config", h.handleConfig)
			r.Post("/config/save", h.handleConfigSave)
			r.Post("/config/test-email", h.handleConfigTestEmail)
			r.Post("/config/test-alert", h.handleConfigTestAlert)
			r.Post("/config/password", h.handleConfigPassword)
			r.Get("/fleet-explorer", h.handleFleetExplorer)
			r.Get("/admin/cells", h.handleCellsAdmin)
			// Stations — enrolled edges and the display-name rename. Auth-gated
			// to match POST /api/edges/rename, which the page calls.
			r.Get("/edges", h.handleEdgesAdmin)

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
	// Before the first write: the compression middleware reads Content-Type at
	// WriteHeader and skips compression when it is empty. See shared.SetHTMLContentType.
	shared.SetHTMLContentType(w)
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
	shared.SetHTMLContentType(w)
	if err := tmpl.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("renderBare %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
