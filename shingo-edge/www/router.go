package www

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"sync"
	"time"

	"shingo/protocol/debuglog"
	"shingo/shared"
	"shingoedge/backup"
	"shingoedge/engine"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
)

// buildVer is kept for any non-favicon cache-busting that wants a stable per-restart value.
var buildVer = time.Now().Format("20060102150405")

// Handlers holds dependencies for HTTP handlers.
//
// Phase 6.5 (2026-04-25) split the engine dependency into two fields
// of different interface types so that compile-time enforcement
// constrains where orchestration verbs can be reached:
//
//   - h.engine (ServiceAccess) — narrow surface, 16 methods. CRUD-only
//     handlers use this. Calling orchestration verbs through h.engine
//     fails to compile because those methods are not on ServiceAccess.
//   - h.orchestration (EngineOrchestration) — wide surface, 51 methods.
//     Material flow, changeover, lifecycle, and WarLink handlers use
//     this. Embeds ServiceAccess so it can also reach service accessors.
//
// In production both fields point to the same *engine.Engine. In tests
// they may differ (a service-only test fixture can leave orchestration
// nil so any accidental orchestration call panics with a clear stack).
type Handlers struct {
	engine        ServiceAccess
	orchestration EngineOrchestration
	backup        *backup.Service
	sessions      *sessionStore
	tmpl          *template.Template
	eventHub      *EventHub
	debugLog      *debuglog.Logger

	// stationViews coalesces concurrent builds of the same operator-station
	// view. See stationViewGroup — the short version is that a station view is
	// expensive, every DB read serialises on one connection, and without this
	// N clients asking for the same board each start their own build.
	stationViews *stationViewGroup

	// specChangeCh is a single-slot channel that coalesces concurrent
	// admin style/claim mutations into ONE plant-claims re-publish. The
	// publisher emits a full snapshot, so collapsing N rapid edits into one
	// publish loses no information (last writer wins on the snapshot view).
	//
	// Named claimSync* until 2026-07-21, when the retired SendClaimSync()
	// no-op it also drove was deleted. Publishing the plant-claims snapshot
	// is now the only work on this signal.
	specChangeCh   chan struct{}
	specChangeStop chan struct{}
	// specChangeOnce guards the close of specChangeStop in the cleanup
	// closure NewRouter returns, for the same reason EventHub.Stop carries
	// one: an unguarded close panics if the cleanup ever runs twice.
	specChangeOnce sync.Once

	// onPlantSpecChange is the plant-claims publisher's spec-change hook.
	// Set by main after constructing the publisher; fired from
	// specChangeLoop so the snapshot re-publishes once per BATCH of edits
	// rather than once per edit. Optional; nil when the publisher is not
	// wired (e.g. tests), in which case the loop simply does nothing.
	onPlantSpecChange func()
}

// NewRouter registers all HTTP endpoints for shingo-edge.
//
// To find a handler: grep for the URL path → handler func name → handlers_*.go.
//
// Route layout:
//
//	/events                — SSE stream (shop floor live updates)
//	/                      — Public pages (production, orders, changeover, operator HMI)
//	/login, /logout        — Authentication
//	/config, /processes, …  — Admin-only pages (adminMiddleware)
//	/api/* (public)        — Shop floor actions (confirm, request, release, changeover, orders)
//	/api/* (admin)         — Setup mutations (PLCs, processes, styles, stations, config, backups)
//
// Auth boundary: h.adminMiddleware. Public = shop floor operator access (no login).
// Handlers live in handlers_*.go files grouped by domain.
func NewRouter(eng *engine.Engine, dbg *debuglog.Logger, backupSvc *backup.Service) (*Handlers, http.Handler, func()) {
	h := &Handlers{
		engine:         eng, // ServiceAccess — narrow surface for CRUD handlers
		orchestration:  eng, // EngineOrchestration — wide surface for flow handlers
		backup:         backupSvc,
		sessions:       newSessionStore(eng.AppConfig().Web.SessionSecret),
		eventHub:       NewEventHub(),
		debugLog:       dbg,
		stationViews:   newStationViewGroup(),
		specChangeCh:   make(chan struct{}, 1),
		specChangeStop: make(chan struct{}),
	}
	go h.specChangeLoop()

	funcMap := templateFuncs()
	h.tmpl = template.Must(template.New("").Funcs(funcMap).ParseFS(templatesFS, "templates/*.html", "templates/partials/*.html"))

	h.eventHub.Start()
	// Phase 6.0c: SSE wiring uses the local *engine.Engine parameter
	// directly; no need for the field on *Handlers since no handler
	// method reads it. Mirrors core/www/router.go's pattern.
	h.eventHub.SetupEngineListeners(eng)

	// Wire debug log entries to SSE broadcast
	dbg.SetOnEntry(func(e debuglog.Entry) {
		h.eventHub.Broadcast(SSEEvent{Type: "debug-log", Data: e})
	})

	r := chi.NewRouter()
	r.Use(middleware.Recoverer)

	// SSE — must be outside compression middleware. Compression buffers
	// defeat streaming flushes, fill the per-client send queue, and cause
	// stale connection buildup when navigating between pages.
	r.Get("/events", h.eventHub.HandleSSE)

	// Everything else gets compressed
	r.Group(func(r chi.Router) {
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

		// Static files (no auth) — ETag keyed on serverInstance + path so every
		// rebuild invalidates every /static/* URL. Files are embedded at compile
		// time, but embed.FS reports modtime=0 across the board, so the default
		// http.ServeContent ETag (name+size+0) survives rebuilds when byte length
		// happens to match — which is what was leaving the operator-station ES
		// modules stuck on stale code after a restart. Tying the ETag to
		// serverInstance forces every request to miss-cache exactly once after
		// each restart, then revalidate cleanly thereafter.
		// Shared UI assets (tokens.css, status-classes.css, utils.js)
		// served from the shingo/shared module via go:embed. Registered
		// BEFORE /static/* so the prefix match wins.
		r.Handle("/static/shared/*", http.StripPrefix("/static/shared/",
			serverInstanceETag(http.FileServer(http.FS(shared.Files))),
		))

		r.Handle("/static/*", http.StripPrefix("/static/",
			serverInstanceETag(http.FileServer(http.FS(StaticFS()))),
		))

		// ── On-call diagnostic surface ──────────────────────────
		// Public so curl from a monitoring host doesn't need auth.
		// kafka_connected and subscribers_wired are LOAD-BEARING —
		// they surface the deaf-but-running mode the Kafka
		// reconnect path makes possible.
		r.Get("/status", h.apiStatus)

		// ── Public pages (shop floor — no auth) ─────────────────
		r.Get("/", h.handleProduction)
		// Permanent redirect from the /material era.
		r.Get("/material", func(w http.ResponseWriter, req *http.Request) {
			target := "/production"
			if q := req.URL.RawQuery; q != "" {
				target += "?" + q
			}
			http.Redirect(w, req, target, http.StatusMovedPermanently)
		})
		r.Get("/material/partial", func(w http.ResponseWriter, req *http.Request) {
			target := "/production/partial"
			if q := req.URL.RawQuery; q != "" {
				target += "?" + q
			}
			http.Redirect(w, req, target, http.StatusMovedPermanently)
		})
		r.Get("/orders", h.handleOrders)
		// Permanent redirect for any operator bookmark from the
		// /kanbans era. The URL was renamed to /orders to match
		// Core's terminology and the user-facing nav label.
		r.Get("/kanbans", func(w http.ResponseWriter, req *http.Request) {
			target := "/orders"
			if q := req.URL.RawQuery; q != "" {
				target += "?" + q
			}
			http.Redirect(w, req, target, http.StatusMovedPermanently)
		})
		r.Get("/kanbans/partial", func(w http.ResponseWriter, req *http.Request) {
			target := "/orders/partial"
			if q := req.URL.RawQuery; q != "" {
				target += "?" + q
			}
			http.Redirect(w, req, target, http.StatusMovedPermanently)
		})
		r.Get("/production", h.handleProduction)
		r.Get("/changeover", h.handleChangeover)
		r.Get("/changeover/partial", h.handleChangeoverPartial)
		r.Get("/orders/partial", h.handleOrdersPartial)
		r.Get("/production/partial", h.handleProductionPartial)

		// Operator station HMI views are public (shop floor monitors)
		r.Get("/operator/station/{id}", h.handleOperatorStationDisplay)

		// ── Login/logout ────────────────────────────────────────
		r.Get("/login", h.handleLoginPage)
		r.Post("/login", h.handleLogin)
		// GET logout backs the header's plain anchor. The handler only
		// clears the session cookie, so a cross-site GET is a nuisance at
		// worst (forced logout), not a privilege change.
		r.Get("/logout", h.handleLogout)
		r.Post("/logout", h.handleLogout)

		// ── Admin pages (auth required) ─────────────────────────
		r.Group(func(r chi.Router) {
			r.Use(h.adminMiddleware)
			r.Get("/config", h.handleConfig)
			r.Get("/processes", h.handleProcesses)
			r.Get("/manual-order", h.handleManualOrder)
			r.Get("/manual-message", h.handleManualMessage)
			r.Get("/diagnostics", h.handleDiagnostics)
			r.Get("/replenishment", h.handleReplenishment)
		})

		// ── API routes ──────────────────────────────────────────
		r.Route("/api", func(r chi.Router) {

			// ── Public API (shop floor actions, no auth) ────────

			// Dev sim control (live speed toggle + data seeders). Sim
			// builds only — a no-op stub in production
			// (sim_routes_stub.go).
			h.registerSimRoutes(r)

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
			r.Post("/process-nodes/{id}/finalize", h.apiRequestProduceSwap)
			r.Post("/process-nodes/{id}/load-bin", h.apiLoadBin)
			r.Post("/process-nodes/{id}/clear-bin", h.apiClearBin)
			r.Post("/process-nodes/{id}/record-count", h.apiRecordCount)
			r.Post("/process-nodes/{id}/clear-loader-home", h.apiClearLoaderHome)
			r.Get("/process-nodes/{id}/market-bins", h.apiGetMarketBins)
			r.Post("/process-nodes/{id}/pull-from-market", h.apiPullFromMarket)
			r.Post("/process-nodes/{id}/push-empty", h.apiPushEmptyOut)
			r.Post("/process-nodes/{id}/request-empty", h.apiRequestEmptyBin)
			r.Post("/process-nodes/{id}/supply-refusal", h.apiRefuseSupply)
			r.Delete("/process-nodes/{id}/supply-refusal", h.apiUndoSupplyRefusal)
			r.Post("/process-nodes/{id}/supply-refusal/ack", h.apiAckSupplyRefusal)
			r.Post("/process-nodes/{id}/request-full", h.apiRequestFullBin)
			r.Post("/process-nodes/{id}/clear-orders", h.apiClearNodeOrders)
			r.Post("/process-nodes/{id}/flip-ab", h.apiFlipABNode)

			// Changeover lifecycle
			// Read-only gate status behind the live "waiting on:" panel.
			// GET + no mutation, so the HMI can poll it; sits with the other
			// shop-floor changeover routes rather than behind admin auth
			// because the operator station is the consumer.
			r.Get("/processes/{id}/changeover/gate-status", h.apiChangeoverGateStatus)
			r.Post("/processes/{id}/changeover/preview", h.apiPreviewProcessChangeover)
			r.Post("/processes/{id}/changeover/start", h.apiStartProcessChangeover)
			r.Post("/processes/{id}/changeover/cutover", h.apiCompleteProcessProductionCutover)
			r.Post("/processes/{id}/changeover/cancel", h.apiCancelProcessChangeover)
			r.Get("/processes/{id}/post-cutover-flag", h.apiGetPostCutoverFlag)
			r.Post("/processes/{id}/post-cutover-flag/confirm", h.apiConfirmPostCutoverFlag)
			r.Post("/processes/{id}/changeover/stage-node/{nodeID}", h.apiStageNodeChangeoverMaterial)
			r.Post("/processes/{id}/changeover/evacuate-node/{nodeID}", h.apiEvacuateNode)
			r.Post("/processes/{id}/changeover/deliver-material/{nodeID}", h.apiDeliverNewMaterialForChangeover)
			r.Post("/processes/{id}/changeover/switch-station/{stationID}", h.apiSwitchOperatorStationToTarget)
			r.Post("/processes/{id}/changeover/switch-node/{nodeID}", h.apiSwitchNodeToTarget)
			r.Post("/processes/{id}/changeover/abandon-node/{nodeID}", h.apiAbandonChangeoverNode)
			// The changeover-wide release, wired to a button this time. Its
			// ancestor /changeover/release-wait was retired in 2026-08 for
			// having no caller — "a registered route with no caller is a door
			// nobody checks" — with the note that a future release-everything
			// button would compose the engine methods again in a handler
			// written for it. That button is the operator marking the setup
			// finished, and it is the only door that expresses what the floor
			// actually does: one click, every leg of the press moves in.
			r.Post("/processes/{id}/changeover/release", h.apiReleaseChangeoverProcess)
			r.Post("/processes/{id}/changeover/sequential-cutover/{nodeID}", h.apiSequentialChangeoverCutover)

			// Orders — LIFECYCLE ONLY here. These act on an order that already
			// exists and are driven by the operator station, which is a shop-floor
			// monitor with no login, so they stay public. Order CREATION moved
			// behind admin auth (see the admin group below).
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
			r.Get("/daily-counts", h.apiGetDailyCounts)
			r.Get("/core-nodes", h.apiGetCoreNodes)
			r.Get("/payload-catalog", h.apiListPayloadCatalog)

			// Lineside buckets (public — embedded on Production page)
			r.Post("/lineside/buckets/{id}/clear", h.apiAdminClearLinesideBucket)
			r.Post("/lineside/buckets/{id}/qty", h.apiAdminEditLinesideBucketQty)

			// ── Admin API (auth required) ───────────────────────
			r.Group(func(r chi.Router) {
				r.Use(h.adminMiddleware)

				// ORDER CREATION. These make new work for robots out of
				// nothing but a POST body, and they sat in the public group
				// — no auth, reachable by anything that can see the Edge.
				//
				// Nobody has been able to say who calls them in production
				// (the open plant question), which is the reason to bound
				// them rather than a reason to wait: an unknown caller is
				// exactly what should not have an unauthenticated door onto
				// order creation.
				//
				// Admin auth rather than a localhost bind because it is the
				// smaller change that keeps the one caller we CAN see: the
				// /manual-order page is already admin-gated, so its fetches
				// carry the same session and keep working. A localhost bind
				// would need server-level config and still would not tell an
				// operator apart from a script.
				//
				// The per-order lifecycle routes stay public, deliberately —
				// release / submit / cancel / count are the operator
				// station's, and that is a shop-floor monitor with no login.
				// Acting on an order that already exists is a different
				// authority from minting one.
				r.Post("/orders/retrieve", h.apiCreateRetrieveOrder)
				r.Post("/orders/move", h.apiCreateMoveOrder)
				r.Post("/orders/complex", h.apiCreateComplexOrder)
				r.Post("/orders/ingest", h.apiCreateIngestOrder)

				// PLCs / WarLink
				r.Get("/plcs", h.apiListPLCs)
				r.Get("/plcs/tags/{name}", h.apiPLCTags)
				r.Get("/plcs/all-tags/{name}", h.apiPLCAllTags)
				r.Post("/plcs/read-tag", h.apiReadTag)
				r.Get("/warlink/status", h.apiWarLinkStatus)
				r.Put("/config/warlink", h.apiUpdateWarLink)

				// UOP backfill (Item 3)
				r.Post("/admin/uop/backfill", h.apiBackfillBuckets)

				// Cell-side autoreorder. The loader-threshold routes that sat
				// here were deleted with the dead Edge threshold surface —
				// Core owns that value (engine/replenishment_admin.go).
				r.Put("/replenishment/cell-reorder", h.apiUpdateCellReorder)

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

			// Process groups (UI taxonomy for the Processes admin sidebar)
			r.Get("/process-groups", h.apiListProcessGroups)
			r.Post("/process-groups", h.apiCreateProcessGroup)
			r.Put("/process-groups/{id}", h.apiUpdateProcessGroup)
			r.Delete("/process-groups/{id}", h.apiDeleteProcessGroup)
			r.Get("/process-groups/{id}/member-count", h.apiCountProcessGroupMembers)
			r.Put("/processes/{id}/group", h.apiSetProcessGroup)

				// Styles & node claims
				r.Get("/styles", h.apiListStyles)
				r.Post("/styles", h.apiCreateStyle)
				r.Put("/styles/{id}", h.apiUpdateStyle)
				r.Get("/styles/{id}/delete-impact", h.apiStyleDeleteImpact)
				r.Delete("/styles/{id}", h.apiDeleteStyle)
				r.Post("/styles/{id}/restore", h.apiRestoreStyle)
				r.Post("/styles/{id}/clone", h.apiCloneStyle)
				r.Post("/styles/{id}/generate", h.apiGenerateStyles)
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
				// A Core loader with no operator screen anywhere on this edge.
				r.Post("/loader-boards", h.apiCreateLoaderBoard)

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
	})

	return h, r, func() {
		h.eventHub.Stop()
		h.specChangeOnce.Do(func() { close(h.specChangeStop) })
	}
}

// SetPlantSpecChangeHook wires the plant-claims publisher's publish callback
// so the coalesced spec-change signal (specChangeLoop) re-publishes the
// plant-claims snapshot on every style/claim edit. Optional; main calls it
// after constructing the publisher. The hook fires inside specChangeLoop's
// recover wrapper, so a panic in the publisher cannot orphan the loop.
func (h *Handlers) SetPlantSpecChangeHook(fn func()) {
	h.onPlantSpecChange = fn
}

// specChangeLoop owns plant-claims re-publishing. Multiple concurrent admin
// style/claim edits collapse into one channel send (capacity 1 with a
// non-blocking sender); this loop drains the channel and publishes
// sequentially, which is the same effective behaviour as spawning a goroutine
// per edit but without the concurrent DB-write race.
//
// The recover wrapper is the same shape as goSafe in main.go but inline here
// because the loop must self-heal — a panic in the publisher should not leave
// the channel orphaned.
func (h *Handlers) specChangeLoop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC www-specChangeLoop: %v (claim sync coalescer exiting)", r)
		}
	}()
	for {
		select {
		case <-h.specChangeStop:
			return
		case <-h.specChangeCh:
			// Re-publish the plant-claims snapshot on the coalesced signal —
			// spec edits changed what every process can source. This used to
			// also call orchestration.SendClaimSync(); that was a retired
			// no-op and is gone. The coalescing and the publish are the
			// live work.
			if h.onPlantSpecChange != nil {
				func() {
					defer func() {
						if r := recover(); r != nil {
							log.Printf("PANIC onPlantSpecChange: %v", r)
						}
					}()
					h.onPlantSpecChange()
				}()
			}
		}
	}
}

// requestSpecChangePublish queues a non-blocking publish request. If one is
// already pending this is a no-op — the pending request will see the updated
// state when it runs, and the publisher emits a full snapshot, so coalescing
// is information-preserving.
func (h *Handlers) requestSpecChangePublish() {
	select {
	case h.specChangeCh <- struct{}{}:
	default:
		// Already pending; coalesce.
	}
}

// serverInstanceETag wraps a static-file handler so every response carries
// an ETag derived from serverInstance + the request path. Each edge restart
// bumps serverInstance, which invalidates every cached /static/* URL exactly
// once. Subsequent revalidations within the same process return 304s.
//
// Why this exists: the embed.FS package zeros modtimes on all bundled files,
// so the default http.ServeContent ETag (name+size+0) doesn't change across
// rebuilds. Browsers happily 304 against the stale ETag and reuse the cache,
// which is what made the operator-station ES modules stick on old code after
// restarts despite the cacheBust query string on the parent <script> tag
// (ES module imports inherit no version query). Tying ETag to serverInstance
// makes the busting unconditional.
func serverInstanceETag(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		etag := `"` + serverInstance + `:` + r.URL.Path + `"`
		w.Header().Set("ETag", etag)
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		if match := r.Header.Get("If-None-Match"); match == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) adminMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		username, ok := h.sessions.getUser(r)
		if !ok || username == "" {
			// Preserve target URL so post-login lands the operator
			// back on the page they were trying to reach instead of
			// dumping them on /config. GETs only — POSTs would lose
			// the body, and re-driving a form submission across the
			// auth bounce is a separate concern.
			loginURL := "/login"
			if r.Method == http.MethodGet {
				if target := shared.SafeNextPath(r.URL.RequestURI()); target != "" {
					loginURL = "/login?next=" + url.QueryEscape(target)
				}
			}
			if r.Header.Get("HX-Request") == "true" {
				w.Header().Set("HX-Redirect", loginURL)
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			http.Redirect(w, r, loginURL, http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) renderTemplate(w http.ResponseWriter, r *http.Request, name string, data any) {
	if m, ok := data.(map[string]any); ok {
		_, isAuth := h.sessions.getUser(r)
		m["Authenticated"] = isAuth
	}
	// Before the first write: the compression middleware reads Content-Type at
	// WriteHeader and skips compression when it is empty. See shared.SetHTMLContentType.
	shared.SetHTMLContentType(w)
	if err := h.tmpl.ExecuteTemplate(w, name, data); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}
