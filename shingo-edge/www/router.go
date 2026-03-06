package www

import (
	"fmt"
	"html/template"
	"io/fs"
	"net/http"
	"strings"
	"time"

	"shingoedge/debuglog"
	"shingoedge/engine"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// buildVer is kept for any non-favicon cache-busting that wants a stable per-restart value.
var buildVer = time.Now().Format("20060102150405")

// Handlers holds dependencies for HTTP handlers.
type Handlers struct {
	engine   *engine.Engine
	sessions *sessionStore
	tmpl     *template.Template
	eventHub *EventHub
	debugLog *debuglog.Logger
}

// NewRouter creates the chi router and returns it along with a stop function.
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger) (http.Handler, func()) {
	h := &Handlers{
		engine:   eng,
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
		"buildVer": func() string { return buildVer },
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
	}
	h.tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	h.eventHub.Start()
	h.eventHub.SetupEngineListeners(eng)

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
	r.Get("/manual-order", h.handleManualOrder)
	r.Get("/production", h.handleProduction)

	// Login/logout
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Post("/logout", h.handleLogout)

	// Admin-only pages
	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/setup", h.handleSetup)
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
		r.Post("/orders/ingest", h.apiCreateIngestOrder)
		r.Post("/orders/{orderID}/release", h.apiReleaseOrder)
		r.Post("/orders/{orderID}/submit", h.apiSubmitOrder)
		r.Post("/orders/{orderID}/cancel", h.apiCancelOrder)
		r.Post("/orders/{orderID}/abort", h.apiAbortOrder)
		r.Post("/orders/{orderID}/redirect", h.apiRedirectOrder)
		r.Post("/orders/{orderID}/count", h.apiSetOrderCount)
		r.Put("/payloads/{id}/count", h.apiPayloadCount)
		r.Put("/payloads/{id}/reorder-point", h.apiUpdateReorderPoint)
		r.Put("/payloads/{id}/auto-reorder", h.apiToggleAutoReorder)
		r.Get("/hourly-counts", h.apiGetHourlyCounts)
		r.Get("/core-nodes", h.apiGetCoreNodes)
		r.Get("/blueprint-catalog", h.apiListBlueprintCatalog)

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

			// Production lines
			r.Get("/lines", h.apiListLines)
			r.Post("/lines", h.apiCreateLine)
			r.Put("/lines/{id}", h.apiUpdateLine)
			r.Delete("/lines/{id}", h.apiDeleteLine)
			r.Put("/lines/{id}/active-style", h.apiSetActiveStyle)
			r.Get("/lines/{id}/job-styles", h.apiListLineJobStyles)
			r.Get("/lines/{id}/location-nodes", h.apiListLineLocationNodes)

			// Job styles
			r.Get("/job-styles", h.apiListJobStyles)
			r.Post("/job-styles", h.apiCreateJobStyle)
			r.Put("/job-styles/{id}", h.apiUpdateJobStyle)
			r.Delete("/job-styles/{id}", h.apiDeleteJobStyle)
			r.Get("/job-styles/{id}/reporting-point", h.apiGetStyleReportingPoint)

			// Payloads
			r.Get("/payloads", h.apiListPayloads)
			r.Post("/payloads", h.apiCreatePayload)
			r.Get("/payloads/job-style/{jobStyleID}", h.apiListPayloadsByJobStyle)
			r.Put("/payloads/{id}", h.apiUpdatePayload)
			r.Delete("/payloads/{id}", h.apiDeletePayload)

			// Location nodes
			r.Get("/location-nodes", h.apiListLocationNodes)
			r.Post("/location-nodes", h.apiCreateLocationNode)
			r.Put("/location-nodes/{id}", h.apiUpdateLocationNode)
			r.Delete("/location-nodes/{id}", h.apiDeleteLocationNode)

			// Core nodes
			r.Post("/core-nodes/sync", h.apiSyncCoreNodes)

			// Blueprint catalog
			r.Post("/blueprint-catalog/sync", h.apiSyncBlueprintCatalog)

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

func (h *Handlers) renderTemplate(w http.ResponseWriter, name string, data interface{}) {
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
