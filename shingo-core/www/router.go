package www

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"

	"shingocore/debuglog"
	"shingocore/engine"
)

type Handlers struct {
	engine   *engine.Engine
	sessions *sessions.CookieStore
	tmpls    map[string]*template.Template
	eventHub *EventHub
	debugLog *debuglog.Logger
}

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

	pages := []string{
		"templates/dashboard.html",
		"templates/nodes.html",
		"templates/orders.html",
		"templates/diagnostics.html",
		"templates/login.html",
		"templates/config.html",
		"templates/rds_explorer.html",
		"templates/robots.html",
		"templates/payloads.html",
		"templates/inventory.html",
		"templates/demand.html",
		"templates/test-orders.html",
	}
	tmpls := make(map[string]*template.Template, len(pages))
	for _, p := range pages {
		clone := template.Must(base.Clone())
		clone = template.Must(clone.ParseFS(templateFS, p))
		// Key is the filename without path: "dashboard.html"
		name := p[len("templates/"):]
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
	r.Use(middleware.Compress(5))

	// Static files
	staticSub, _ := fs.Sub(staticFS, "static")
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticSub))))

	// SSE
	r.Get("/events", hub.SSEHandler)

	// --- Public pages ---
	r.Get("/", h.handleDashboard)
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Get("/logout", h.handleLogout)
	r.Get("/nodes", h.handleNodes)
	r.Get("/orders", h.handleOrders)
	r.Get("/orders/detail", h.handleOrderDetail)
	r.Get("/robots", h.handleRobots)
	r.Get("/demand", h.handleDemand)

	// --- Public API (read-only, no auth) ---
	r.Route("/api", func(r chi.Router) {
		// Nodes
		r.Get("/nodes", h.apiListNodes)
		r.Get("/nodes/inventory", h.apiNodePayloads)
		r.Get("/nodes/occupancy", h.apiNodeOccupancy)
		r.Get("/nodes/detail", h.apiNodeDetail)
		r.Get("/nodestate", h.apiNodeState)
		r.Get("/map/points", h.apiScenePoints)

		// Orders
		r.Get("/orders", h.apiListOrders)
		r.Get("/orders/detail", h.apiGetOrder)

		// Robots
		r.Get("/robots", h.apiRobotsStatus)

		// Payloads
		r.Get("/payload-styles", h.apiListPayloadStyles)
		r.Get("/instances", h.apiListInstances)
		r.Get("/instances/detail", h.apiGetInstance)
		r.Get("/instances/manifest", h.apiListManifest)

		// Corrections
		r.Get("/corrections", h.apiListNodeCorrections)

		// Demands
		r.Get("/demands", h.apiListDemands)
		r.Get("/demands/{id}/log", h.apiDemandLog)

		// Health
		r.Get("/health", h.apiHealthCheck)
	})

	// --- Protected routes (auth required) ---
	r.Group(func(r chi.Router) {
		r.Use(h.requireAuth)

		// Protected pages
		r.Get("/test-orders", h.handleTestOrders)
		r.Get("/payloads", h.handlePayloads)
		r.Get("/inventory", h.handleInventory)
		r.Get("/diagnostics", h.handleDiagnostics)
		r.Get("/config", h.handleConfig)
		r.Post("/config/save", h.handleConfigSave)
		r.Get("/fleet-explorer", h.handleFleetExplorer)

		// Node management
		r.Route("/nodes", func(r chi.Router) {
			r.Post("/create", h.handleNodeCreate)
			r.Post("/update", h.handleNodeUpdate)
			r.Post("/delete", h.handleNodeDelete)
			r.Post("/sync-fleet", h.handleNodeSyncFleet)
			r.Post("/sync-scene", h.handleSceneSync)
		})

		// Node type management
		r.Route("/node-types", func(r chi.Router) {
			r.Post("/create", h.handleNodeTypeCreate)
			r.Post("/update", h.handleNodeTypeUpdate)
			r.Post("/delete", h.handleNodeTypeDelete)
		})

		// Payload style management
		r.Route("/payload-styles", func(r chi.Router) {
			r.Post("/create", h.handlePayloadStyleCreate)
			r.Post("/update", h.handlePayloadStyleUpdate)
			r.Post("/delete", h.handlePayloadStyleDelete)
		})

		// Payload instance management
		r.Route("/instances", func(r chi.Router) {
			r.Post("/create", h.handleInstanceCreate)
			r.Post("/update", h.handleInstanceUpdate)
			r.Post("/delete", h.handleInstanceDelete)
		})

		// Protected API
		r.Route("/api", func(r chi.Router) {
			// Node properties
			r.Post("/nodes/properties/set", h.apiNodePropertySet)
			r.Post("/nodes/properties/delete", h.apiNodePropertyDelete)
			r.Post("/nodes/test-order", h.apiNodeTestOrder)

			// Kafka test orders
			r.Get("/test-orders", h.apiTestOrdersList)
			r.Get("/test-orders/detail", h.apiTestOrderDetail)
			r.Post("/test-orders/submit", h.apiTestOrderSubmit)
			r.Post("/test-orders/cancel", h.apiTestOrderCancel)
			r.Post("/test-orders/receipt", h.apiTestOrderReceipt)
			r.Get("/test-orders/robots", h.apiTestRobots)
			r.Get("/test-orders/scene-points", h.apiTestScenePoints)

			// Direct-to-fleet orders
			r.Get("/test-orders/direct", h.apiDirectOrdersList)
			r.Post("/test-orders/direct", h.apiDirectOrderSubmit)

			// Direct RDS commands
			r.Post("/test-commands/submit", h.apiTestCommandSubmit)
			r.Get("/test-commands", h.apiTestCommandsList)
			r.Get("/test-commands/status", h.apiTestCommandStatus)

			// Payload manifest
			r.Post("/payloads/manifest/create", h.apiCreateManifestItem)
			r.Post("/payloads/manifest/update", h.apiUpdateManifestItem)
			r.Post("/payloads/manifest/delete", h.apiDeleteManifestItem)

			// Inventory instance actions
			r.Post("/instances/action", h.apiInstanceAction)
			r.Post("/instances/bulk-register", h.apiBulkRegisterInstances)
			r.Get("/instances/events", h.apiListInstanceEvents)

			// Supermarket management
			r.Post("/supermarket/create", h.apiCreateSupermarket)
			r.Get("/supermarket/layout", h.apiGetSupermarketLayout)
			r.Post("/supermarket/delete", h.apiDeleteSupermarket)

			// Corrections
			r.Post("/corrections/create", h.apiCreateCorrection)

			// Fleet proxy
			r.Post("/fleet/proxy", h.apiFleetProxy)

			// Robot management
			r.Post("/robots/availability", h.apiRobotSetAvailability)
			r.Post("/robots/retry", h.apiRobotRetryFailed)
			r.Post("/robots/force-complete", h.apiRobotForceComplete)

			// Order management
			r.Post("/orders/terminate", h.apiTerminateOrder)
			r.Post("/orders/priority", h.apiSetOrderPriority)

			// Demand management
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

