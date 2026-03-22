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
	"shingoedge/backup"
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
	backup   *backup.Service
	sessions *sessionStore
	tmpl     *template.Template
	eventHub *EventHub
	debugLog *debuglog.Logger
}

// NewRouter creates the chi router and returns it along with a stop function.
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger, backupSvc *backup.Service) (http.Handler, func()) {
	h := &Handlers{
		engine:   eng,
		eng:      eng,
		backup:   backupSvc,
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

	// Operator station HMI views are public (shop floor monitors)
	r.Get("/operator/station/{id}", h.handleOperatorStationDisplay)

	// Login/logout
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Post("/logout", h.handleLogout)

	// Admin-only pages
	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/setup", h.handleSetup)
		r.Get("/config", h.handleConfig)
		r.Get("/processes", h.handleProcesses)
		r.Get("/production", h.handleProduction)
		r.Get("/manual-order", h.handleManualOrder)
		r.Get("/operator", h.handleOperatorStationAdmin)
		r.Get("/manual-message", h.handleManualMessage)
		r.Get("/diagnostics", h.handleDiagnostics)
	})

	// API endpoints (mixed: some public for shop floor, some admin-only)
	r.Route("/api", func(r chi.Router) {
		// Public API (shop floor actions)
		r.Post("/confirm-delivery/{orderID}", h.apiConfirmDelivery)
		r.Post("/confirm-anomaly/{snapshotID}", h.apiConfirmAnomaly)
		r.Post("/dismiss-anomaly/{snapshotID}", h.apiDismissAnomaly)
		r.Get("/operator-stations/{id}/view", h.apiGetOperatorStationView)
		r.Post("/op-nodes/{id}/request", h.apiRequestOpNodeMaterial)
		r.Post("/op-nodes/{id}/release-empty", h.apiReleaseOpNodeEmpty)
		r.Post("/op-nodes/{id}/release-partial", h.apiReleaseOpNodePartial)
		r.Post("/op-nodes/{id}/manifest/confirm", h.apiConfirmOpNodeManifest)
		r.Post("/processes/{id}/changeover/start", h.apiStartProcessChangeoverV2)
		r.Post("/processes/{id}/changeover/phase", h.apiAdvanceProcessChangeoverPhase)
		r.Post("/processes/{id}/changeover/cutover", h.apiCompleteProcessProductionCutover)
		r.Post("/processes/{id}/changeover/cancel", h.apiCancelProcessChangeoverV2)
		r.Post("/processes/{id}/changeover/stage-node/{nodeID}", h.apiStageOpNodeChangeoverMaterial)
		r.Post("/processes/{id}/changeover/empty-node/{nodeID}", h.apiEmptyOpNodeForToolChange)
		r.Post("/processes/{id}/changeover/release-node/{nodeID}", h.apiReleaseOpNodeIntoProduction)
		r.Post("/processes/{id}/changeover/switch-station/{stationID}", h.apiSwitchOperatorStationToTarget)
		r.Post("/processes/{id}/changeover/switch-node/{nodeID}", h.apiSwitchOpNodeToTarget)
		r.Post("/orders/retrieve", h.apiCreateRetrieveOrder)
		r.Post("/orders/store", h.apiCreateStoreOrder)
		r.Post("/orders/move", h.apiCreateMoveOrder)
		r.Post("/orders/complex", h.apiCreateComplexOrder)
		r.Post("/orders/ingest", h.apiCreateIngestOrder)
		r.Post("/orders/{orderID}/release", h.apiReleaseOrder)
		r.Post("/orders/{orderID}/submit", h.apiSubmitOrder)
		r.Post("/orders/{orderID}/cancel", h.apiCancelOrder)
		r.Post("/orders/{orderID}/abort", h.apiCancelOrder)
		r.Post("/orders/{orderID}/redirect", h.apiRedirectOrder)
		r.Post("/orders/{orderID}/count", h.apiSetOrderCount)
		r.Get("/orders/active", h.apiGetActiveOrders)
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
			r.Get("/processes/{id}/counter-binding", h.apiGetProcessCounterBinding)
			r.Put("/processes/{id}/counter-binding", h.apiUpsertProcessCounterBinding)
			r.Get("/processes/{id}/styles", h.apiListProcessStyles)
			r.Get("/processes/{id}/nodes", h.apiListProcessNodes)

			// Styles
			r.Get("/styles", h.apiListStyles)
			r.Post("/styles", h.apiCreateStyle)
			r.Put("/styles/{id}", h.apiUpdateStyle)
			r.Delete("/styles/{id}", h.apiDeleteStyle)
			r.Get("/styles/{id}/reporting-point", h.apiGetStyleReportingPoint)

			// Operator stations
			r.Get("/operator-stations", h.apiListOperatorStations)
			r.Post("/operator-stations", h.apiCreateOperatorStation)
			r.Put("/operator-stations/{id}", h.apiUpdateOperatorStation)
			r.Delete("/operator-stations/{id}", h.apiDeleteOperatorStation)

			// Operator station nodes
			r.Get("/op-station-nodes", h.apiListOpStationNodes)
			r.Get("/op-station-nodes/station/{stationID}", h.apiListOpStationNodesByStation)
			r.Post("/op-station-nodes", h.apiCreateOpStationNode)
			r.Put("/op-station-nodes/{id}", h.apiUpdateOpStationNode)
			r.Delete("/op-station-nodes/{id}", h.apiDeleteOpStationNode)

			// Style assignments
			r.Get("/op-node-assignments/process/{processID}", h.apiListOpNodeAssignmentsByProcess)
			r.Get("/op-node-assignments/node/{opNodeID}", h.apiListOpNodeAssignmentsByNode)
			r.Get("/op-node-assignments/style/{styleID}", h.apiListOpNodeAssignmentsByStyle)
			r.Post("/op-node-assignments", h.apiUpsertOpNodeAssignment)
			r.Delete("/op-node-assignments/{id}", h.apiDeleteOpNodeAssignment)

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
			r.Get("/backups", h.apiListBackups)
			r.Get("/backups/status", h.apiBackupStatus)
			r.Put("/backups/config", h.apiUpdateBackupConfig)
			r.Post("/backups/test", h.apiTestBackupConfig)
			r.Post("/backups/run", h.apiRunBackup)
			r.Post("/backups/restore", h.apiStageBackupRestore)

			// Manual message
			r.Post("/manual-message", h.apiSendManualMessage)
			r.Post("/diagnostics/outbox/replay", h.apiReplayOutbox)
			r.Post("/diagnostics/orders/sync", h.apiRequestOrderStatusSync)

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
