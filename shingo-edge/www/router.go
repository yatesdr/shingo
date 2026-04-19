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

// NewRouter registers all HTTP endpoints for shingo-edge.
//
// To find a handler: grep for the URL path → handler func name → handlers_*.go.
//
// Route layout:
//   /events                — SSE stream (shop floor live updates)
//   /                      — Public pages (material, kanbans, production, changeover, operator HMI)
//   /login, /logout        — Authentication
//   /config, /processes, …  — Admin-only pages (adminMiddleware)
//   /api/* (public)        — Shop floor actions (confirm, request, release, changeover, orders)
//   /api/* (admin)         — Setup mutations (PLCs, processes, styles, stations, config, backups)
//
// Auth boundary: h.adminMiddleware. Public = shop floor operator access (no login).
// Handlers live in handlers_*.go files grouped by domain.
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

	// Static files (no auth) — no-cache to force revalidation after deploys.
	// Files are embedded at compile time, so they only change when the binary
	// is updated. Browsers will revalidate on each load but use cache if
	// the content hasn't changed.
	r.Handle("/static/*", http.StripPrefix("/static/",
		noCacheMiddleware(http.FileServer(http.FS(StaticFS()))),
	))

	// ── SSE (no auth — shop floor) ─────────────────────────
	r.Get("/events", h.eventHub.HandleSSE)

	// ── Public pages (shop floor — no auth) ─────────────────
	r.Get("/", h.handleMaterial)
	r.Get("/material", h.handleMaterial)
	r.Get("/kanbans", h.handleKanbans)
	r.Get("/production", h.handleProduction)
	r.Get("/changeover", h.handleChangeover)
	r.Get("/changeover/partial", h.handleChangeoverPartial)
	r.Get("/kanbans/partial", h.handleKanbansPartial)
	r.Get("/material/partial", h.handleMaterialPartial)

	// Operator station HMI views are public (shop floor monitors)
	r.Get("/operator/station/{id}", h.handleOperatorStationDisplay)

	// ── Login/logout ────────────────────────────────────────
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Post("/logout", h.handleLogout)

	// ── Admin pages (auth required) ─────────────────────────
	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/config", h.handleConfig)
		r.Get("/traffic", h.handleTraffic)
		r.Get("/processes", h.handleProcesses)
		r.Get("/manual-order", h.handleManualOrder)
		r.Get("/manual-message", h.handleManualMessage)
		r.Get("/diagnostics", h.handleDiagnostics)
	})

	// ── API routes ──────────────────────────────────────────
	r.Route("/api", func(r chi.Router) {

		// ── Public API (shop floor actions, no auth) ────────

		// Delivery confirmation & anomalies
		r.Post("/confirm-delivery/{orderID}", h.apiConfirmDelivery)
		r.Post("/confirm-anomaly/{snapshotID}", h.apiConfirmAnomaly)
		r.Post("/dismiss-anomaly/{snapshotID}", h.apiDismissAnomaly)

		// Operator station views
		r.Get("/operator-stations/{id}/view", h.apiGetOperatorStationView)

		// Process node operations (material request, release, produce, bin ops)
		r.Post("/process-nodes/{id}/request", h.apiRequestNodeMaterial)
		r.Post("/process-nodes/{id}/release-empty", h.apiReleaseNodeEmpty)
		r.Post("/process-nodes/{id}/release-partial", h.apiReleaseNodePartial)
		r.Post("/process-nodes/{id}/release-staged", h.apiReleaseNodeStagedOrders)
		r.Post("/process-nodes/{id}/manifest/confirm", h.apiConfirmNodeManifest)
		r.Post("/process-nodes/{id}/finalize", h.apiFinalizeProduceNode)
		r.Post("/process-nodes/{id}/load-bin", h.apiLoadBin)
		r.Post("/process-nodes/{id}/clear-bin", h.apiClearBin)
		r.Post("/process-nodes/{id}/request-empty", h.apiRequestEmptyBin)
		r.Post("/process-nodes/{id}/request-full", h.apiRequestFullBin)
		r.Post("/process-nodes/{id}/clear-orders", h.apiClearNodeOrders)
		r.Post("/process-nodes/{id}/flip-ab", h.apiFlipABNode)

		// Changeover lifecycle
		r.Post("/processes/{id}/changeover/start", h.apiStartProcessChangeover)
		r.Post("/processes/{id}/changeover/cutover", h.apiCompleteProcessProductionCutover)
		r.Post("/processes/{id}/changeover/cancel", h.apiCancelProcessChangeover)
		r.Post("/processes/{id}/changeover/stage-node/{nodeID}", h.apiStageNodeChangeoverMaterial)
		r.Post("/processes/{id}/changeover/empty-node/{nodeID}", h.apiEmptyNodeForToolChange)
		r.Post("/processes/{id}/changeover/release-node/{nodeID}", h.apiReleaseNodeIntoProduction)
		r.Post("/processes/{id}/changeover/switch-station/{stationID}", h.apiSwitchOperatorStationToTarget)
		r.Post("/processes/{id}/changeover/switch-node/{nodeID}", h.apiSwitchNodeToTarget)
		r.Post("/processes/{id}/changeover/release-wait", h.apiReleaseChangeoverWait)

		// Orders (create, lifecycle, manual)
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

		// Lookups
		r.Get("/node/{name}/children", h.apiNodeChildren)
		r.Get("/payload/{code}/manifest", h.apiPayloadManifest)
		r.Get("/hourly-counts", h.apiGetHourlyCounts)
		r.Get("/core-nodes", h.apiGetCoreNodes)
		r.Get("/payload-catalog", h.apiListPayloadCatalog)

		// ── Admin API (auth required) ───────────────────────
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

			// Styles & node claims
			r.Get("/styles", h.apiListStyles)
			r.Post("/styles", h.apiCreateStyle)
			r.Put("/styles/{id}", h.apiUpdateStyle)
			r.Delete("/styles/{id}", h.apiDeleteStyle)
			r.Get("/styles/{id}/node-claims", h.apiListStyleNodeClaims)
			r.Post("/style-node-claims", h.apiUpsertStyleNodeClaim)
			r.Delete("/style-node-claims/{id}", h.apiDeleteStyleNodeClaim)

			// Operator stations
			r.Get("/operator-stations", h.apiListOperatorStations)
			r.Post("/operator-stations", h.apiCreateOperatorStation)
			r.Put("/operator-stations/{id}", h.apiUpdateOperatorStation)
			r.Post("/operator-stations/{id}/move", h.apiMoveOperatorStation)
			r.Delete("/operator-stations/{id}", h.apiDeleteOperatorStation)
			r.Get("/operator-stations/{id}/claimed-nodes", h.apiGetStationClaimedNodes)
			r.Put("/operator-stations/{id}/claimed-nodes", h.apiSetStationClaimedNodes)

			// Process nodes
			r.Get("/process-nodes", h.apiListConfiguredProcessNodes)
			r.Get("/process-nodes/station/{stationID}", h.apiListConfiguredProcessNodesByStation)
			r.Post("/process-nodes", h.apiCreateProcessNode)
			r.Put("/process-nodes/{id}", h.apiUpdateProcessNode)
			r.Delete("/process-nodes/{id}", h.apiDeleteProcessNode)

			// Sync (core nodes, payload catalog)
			r.Post("/core-nodes/sync", h.apiSyncCoreNodes)
			r.Post("/payload-catalog/sync", h.apiSyncPayloadCatalog)

			// Shifts
			r.Get("/shifts", h.apiListShifts)
			r.Put("/shifts", h.apiSaveShifts)

			// Traffic (count-group bindings)
			r.Get("/traffic/bindings", h.apiTrafficBindings)
			r.Put("/traffic/heartbeat", h.apiTrafficSaveHeartbeat)
			r.Post("/traffic/bindings", h.apiTrafficAddBinding)
			r.Post("/traffic/bindings/delete", h.apiTrafficDeleteBinding)

			// Config & backups
			r.Put("/config/core-api", h.apiUpdateCoreAPI)
			r.Post("/config/core-api/test", h.apiTestCoreAPI)
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

			// Diagnostics & manual tools
			r.Post("/manual-message", h.apiSendManualMessage)
			r.Post("/diagnostics/outbox/replay", h.apiReplayOutbox)
			r.Post("/diagnostics/orders/sync", h.apiRequestOrderStatusSync)
		})
	})

	return r, func() {
		h.eventHub.Stop()
	}
}

// noCacheMiddleware wraps a handler with Cache-Control headers that force
// browser revalidation. Used for embedded static assets that change on deploy.
func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		next.ServeHTTP(w, r)
	})
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
