package www

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"

	"shingo/protocol/debuglog"
	"shingocore/engine"
)

type Handlers struct {
	engine   *engine.Engine
	sessions *sessions.CookieStore
	tmpls    map[string]*template.Template
	eventHub *EventHub
	debugLog *debuglog.Logger
}

// NewRouter registers all HTTP endpoints for shingo-core.
//
// To find a handler: grep for the URL path → handler func name → handlers_*.go.
//
// Route layout:
//   /events                — SSE stream (outside compression middleware)
//   /                      — Public pages (dashboard, login, nodes, orders, robots, etc.)
//   /api/* (public)        — Read-only JSON API (nodes, orders, bins, payloads, telemetry)
//   /api/* (protected)     — Write API (test orders, payloads, bins, nodegroups, fleet, recovery)
//   /* (protected)         — Admin pages (test-orders, config, diagnostics, CRUD forms)
//
// Auth boundary: h.requireAuth middleware. Public = shop floor read access.
// Handlers live in handlers_*.go files grouped by domain (bins, nodes, payloads, etc.).
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger) (http.Handler, func()) {
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
		log.Fatalf("glob templates: %v", err)
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
		engine:   eng,
		sessions: sessionStore,
		tmpls:    tmpls,
		eventHub: hub,
		debugLog: dbg,
	}

	h.ensureDefaultAdmin(eng.DB())

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// SSE — must be outside compression middleware. Compression buffers
	// defeat streaming flushes and cause stale connection buildup when
	// navigating between pages.
	r.Get("/events", hub.SSEHandler)

	// Everything else gets compressed
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))

		// Static files
		staticSub, _ := fs.Sub(staticFS, "static")
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

		// ── Public pages ───────────────────────────────────────
		r.Get("/", h.handleDashboard)
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

		// ── API routes ─────────────────────────────────────────
		r.Route("/api", func(r chi.Router) {

			// ── Public API (read-only, no auth) ────────────────

			// Nodes
			r.Get("/nodes", h.apiListNodes)
			r.Get("/nodes/inventory", h.apiNodePayloads)
			r.Get("/nodes/occupancy", h.apiNodeOccupancy)
			r.Get("/nodes/detail", h.apiNodeDetail)
			r.Get("/nodes/bin-types", h.apiGetNodeBinTypes)
			r.Get("/nodestate", h.apiNodeState)
			r.Get("/map/points", h.apiScenePoints)

			// Orders & missions
			r.Get("/orders", h.apiListOrders)
			r.Get("/orders/detail", h.apiGetOrder)
			r.Get("/orders/enriched", h.apiGetOrderEnriched)
			r.Get("/missions", h.apiListMissions)
			r.Get("/missions/stats", h.apiMissionStats)
			r.Get("/missions/{orderID}", h.apiGetMission)

			// Robots
			r.Get("/robots", h.apiRobotsStatus)

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
			r.Get("/telemetry/payload/{code}/manifest", h.apiTelemetryPayloadManifest)
			r.Get("/telemetry/node/{name}/children", h.apiTelemetryNodeChildren)
			r.Post("/telemetry/bin-load", h.apiBinLoad)
			r.Post("/telemetry/bin-clear", h.apiBinClear)

			// Inventory & diagnostics
			r.Get("/inventory", h.apiInventory)
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
				r.Post("/orders/spot", h.apiSpotOrderSubmit)

				// Outbox & recovery
				r.Post("/outbox/replay", h.apiReplayOutbox)
				r.Post("/recovery/repair", h.apiRepairAnomaly)

				// Demands
				r.Post("/demands", h.apiCreateDemand)
				r.Put("/demands/{id}", h.apiUpdateDemand)
				r.Put("/demands/{id}/apply", h.apiApplyDemand)
				r.Delete("/demands/{id}", h.apiDeleteDemand)
				r.Post("/demands/apply-all", h.apiApplyAllDemands)
				r.Put("/demands/{id}/produced", h.apiSetDemandProduced)
				r.Post("/demands/{id}/clear", h.apiClearDemandProduced)
				r.Post("/demands/clear-all", h.apiClearAllProduced)
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
			r.Post("/bins/delete", h.handleBinDelete)
		})
	}) // end compression group (wraps all routes except SSE)

	stopFn := func() {
		hub.Stop()
	}

	return r, stopFn
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
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}
