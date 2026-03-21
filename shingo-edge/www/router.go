package www

import (
	"encoding/json"
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"shingo/protocol/debuglog"
	"shingoedge/engine"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// buildVer is kept for any non-favicon cache-busting that wants a stable per-restart value.
var buildVer = time.Now().Format("20060102150405")

// Handlers holds dependencies for HTTP handlers.
type Handlers struct {
	engine   EngineAccess
	eng      *engine.Engine // concrete engine for EventBus/SSE wiring
	sessions *sessionStore
	tmpl     *template.Template
	eventHub *EventHub
	debugLog *debuglog.Logger
}

// NewRouter creates the chi router and returns it along with a stop function.
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger) (http.Handler, func()) {
	h := &Handlers{
		engine:   eng,
		eng:      eng,
		sessions: newSessionStore(eng.AppConfig().Web.SessionSecret),
		eventHub: NewEventHub(),
		debugLog: dbg,
	}

	funcMap := template.FuncMap{
		"join": strings.Join,
		"truncate": func(s string, n int) string {
			if len(s) <= n {
				return s
			}
			return s[:n] + "..."
		},
		"divPercent": func(a, b int) float64 {
			if b == 0 {
				return 0
			}
			return float64(a) / float64(b) * 100
		},
		"deref": func(p *int64) int64 {
			if p == nil {
				return 0
			}
			return *p
		},
		"brokerHost": func(s string) string {
			if i := strings.LastIndex(s, ":"); i >= 0 {
				return s[:i]
			}
			return s
		},
		"brokerPort": func(s string) string {
			if i := strings.LastIndex(s, ":"); i >= 0 {
				return s[i+1:]
			}
			return ""
		},
		"buildVer":  func() string { return buildVer },
		"cacheBust": func() string { return fmt.Sprintf("%x", time.Now().UnixNano()) },
		"formatTime": func(t time.Time) template.HTML {
			if t.IsZero() {
				return template.HTML("")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
		},
		"formatTimePtr": func(t *time.Time) template.HTML {
			if t == nil {
				return template.HTML("")
			}
			return template.HTML(`<time data-utc="` + t.UTC().Format(time.RFC3339) + `">` +
				t.UTC().Format("2006-01-02 15:04:05") + ` UTC</time>`)
		},
		"json": func(v interface{}) template.JS {
			b, _ := json.Marshal(v)
			return template.JS(b)
		},
	}
	h.tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	h.eventHub.Start()
	h.eventHub.SetupEngineListeners(h.eng)

	// Wire debug log entries to SSE broadcast
	dbg.SetOnEntry(func(e debuglog.Entry) {
		h.eventHub.Broadcast(SSEEvent{Type: "debug-log", Data: e})
	})

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)
	r.Use(middleware.Compress(5))

	// Favicon: serve with no-cache headers to defeat aggressive browser caching (Safari).
	faviconData, _ := fs.ReadFile(staticFS, "static/favicon.ico")
	faviconHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/x-icon")
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		w.Write(faviconData)
	})
	r.Handle("/favicon.ico", faviconHandler)
	r.Handle("/static/favicon.ico", faviconHandler)

	// Static files (no auth)
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(StaticFS()))))

	// SSE (no auth — shop floor)
	r.Get("/events", h.eventHub.HandleSSE)

	// Public pages (shop floor — no auth required)
	r.Get("/", h.handleMaterial)
	r.Get("/material", h.handleMaterial)
	r.Get("/kanbans", h.handleKanbans)
	r.Get("/changeover", h.handleChangeover)

	// Operator display/cell views are public (shop floor monitors)
	r.Get("/operator/cell/{id}", h.handleOperatorCellDisplay)
	r.Get("/operator/display/{id}", h.handleOperatorDisplay)

	// Login/logout
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Post("/logout", h.handleLogout)

	// Admin-only pages
	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/setup", h.handleSetup)
		r.Get("/production", h.handleProduction)
		r.Get("/manual-order", h.handleManualOrder)
		r.Get("/operator", h.handleOperatorScreenList)
		r.Get("/operator/designer", h.handleOperatorDesigner)
		r.Get("/manual-message", h.handleManualMessage)
		r.Get("/diagnostics", h.handleDiagnostics)
	})

	// API endpoints (mixed: some public for shop floor, some admin-only)
	r.Route("/api", func(r chi.Router) {
		// Public API (shop floor actions)
		r.Post("/confirm-delivery/{orderID}", h.apiConfirmDelivery)
		r.Post("/confirm-anomaly/{snapshotID}", h.apiConfirmAnomaly)
		r.Post("/dismiss-anomaly/{snapshotID}", h.apiDismissAnomaly)
		r.Post("/changeover/start", h.apiChangeoverStart)
		r.Post("/changeover/advance", h.apiChangeoverAdvance)
		r.Post("/changeover/cancel", h.apiChangeoverCancel)
		r.Post("/orders/retrieve", h.apiCreateRetrieveOrder)
		r.Post("/orders/store", h.apiCreateStoreOrder)
		r.Post("/orders/move", h.apiCreateMoveOrder)
		r.Post("/orders/complex", h.apiCreateComplexOrder)
		r.Post("/orders/request", h.apiSmartRequest)
		r.Post("/orders/ingest", h.apiCreateIngestOrder)
		r.Post("/orders/{orderID}/release", h.apiReleaseOrder)
		r.Post("/orders/{orderID}/submit", h.apiSubmitOrder)
		r.Post("/orders/{orderID}/cancel", h.apiCancelOrder)
		r.Post("/orders/{orderID}/abort", h.apiCancelOrder)
		r.Post("/orders/{orderID}/redirect", h.apiRedirectOrder)
		r.Post("/orders/{orderID}/count", h.apiSetOrderCount)
		r.Put("/material-slots/{id}/count", h.apiSlotCount)
		r.Put("/material-slots/{id}/reorder-point", h.apiUpdateReorderPoint)
		r.Put("/material-slots/{id}/auto-reorder", h.apiToggleAutoReorder)
		r.Get("/orders/active", h.apiGetActiveOrders)
		r.Get("/material-slots/process/{processID}", h.apiListSlotsByProcessPublic)
		r.Get("/hourly-counts", h.apiGetHourlyCounts)
		r.Get("/core-nodes", h.apiGetCoreNodes)
		r.Get("/payload-catalog", h.apiListPayloadCatalog)

		// Admin API (setup mutations)
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)

			// PLCs / WarLink
			r.Get("/plcs", h.apiListPLCs)
			r.Get("/plcs/tags/{name}", h.apiPLCTags)
			r.Get("/plcs/all-tags/{name}", h.apiPLCAllTags)
			r.Post("/plcs/read-tag", h.apiReadTag)
			r.Get("/warlink/status", h.apiWarLinkStatus)
			r.Put("/config/warlink", h.apiUpdateWarLink)

			// Reporting points
			r.Get("/reporting-points", h.apiListReportingPoints)
			r.Post("/reporting-points", h.apiCreateReportingPoint)
			r.Put("/reporting-points/{id}", h.apiUpdateReportingPoint)
			r.Delete("/reporting-points/{id}", h.apiDeleteReportingPoint)

			// Processes
			r.Get("/processes", h.apiListProcesses)
			r.Post("/processes", h.apiCreateProcess)
			r.Put("/processes/{id}", h.apiUpdateProcess)
			r.Delete("/processes/{id}", h.apiDeleteProcess)
			r.Put("/processes/{id}/active-style", h.apiSetActiveStyle)
			r.Get("/processes/{id}/styles", h.apiListProcessStyles)
			r.Get("/processes/{id}/nodes", h.apiListProcessNodes)

			// Styles
			r.Get("/styles", h.apiListStyles)
			r.Post("/styles", h.apiCreateStyle)
			r.Put("/styles/{id}", h.apiUpdateStyle)
			r.Delete("/styles/{id}", h.apiDeleteStyle)
			r.Get("/styles/{id}/reporting-point", h.apiGetStyleReportingPoint)

			// Material slots
			r.Get("/material-slots", h.apiListSlots)
			r.Post("/material-slots", h.apiCreateSlot)
			r.Get("/material-slots/style/{styleID}", h.apiListSlotsByStyle)
			r.Put("/material-slots/{id}", h.apiUpdateSlot)
			r.Delete("/material-slots/{id}", h.apiDeleteSlot)

			// Nodes
			r.Get("/nodes", h.apiListNodes)
			r.Post("/nodes", h.apiCreateNode)
			r.Put("/nodes/{id}", h.apiUpdateNode)
			r.Delete("/nodes/{id}", h.apiDeleteNode)

			// Core nodes
			r.Post("/core-nodes/sync", h.apiSyncCoreNodes)

			// Payload catalog
			r.Post("/payload-catalog/sync", h.apiSyncPayloadCatalog)

			// Shifts
			r.Get("/shifts", h.apiListShifts)
			r.Put("/shifts", h.apiSaveShifts)

			// Config
			r.Put("/config/messaging", h.apiUpdateMessaging)
			r.Put("/config/station-id", h.apiUpdateStationID)
			r.Post("/config/kafka/test", h.apiTestKafka)
			r.Put("/config/auto-confirm", h.apiUpdateAutoConfirm)
			r.Post("/config/password", h.apiChangePassword)

			// Manual message
			r.Post("/manual-message", h.apiSendManualMessage)
			r.Post("/diagnostics/outbox/replay", h.apiReplayOutbox)
			r.Post("/diagnostics/orders/sync", h.apiRequestOrderStatusSync)

			// Operator screens (designer CRUD)
			r.Post("/operator-screens", h.apiCreateOperatorScreen)
			r.Get("/operator-screens", h.apiListOperatorScreens)
			r.Get("/operator-screens/{id}/layout", h.apiGetOperatorScreenLayout)
			r.Put("/operator-screens/{id}/layout", h.apiSaveOperatorScreenLayout)
		})
	})

	return r, func() {
		h.eventHub.Stop()
	}
}

func (h *Handlers) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := h.sessions.getUser(r)
		if !ok || username == "" {
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", "/login")
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) renderTemplate(w http.ResponseWriter, r *http.Request, name string, data interface{}) {
	if m, ok := data.(map[string]interface{}); ok {
		_, isAuth := h.sessions.getUser(r)
		m["Authenticated"] = isAuth
	}
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
