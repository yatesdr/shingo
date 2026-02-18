package www

import (
	"html/template"
	"io/fs"
	"log"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/gorilla/sessions"

	"warpath/engine"
)

type Handlers struct {
	engine   *engine.Engine
	sessions *sessions.CookieStore
	tmpls    map[string]*template.Template
	eventHub *EventHub
}

func NewRouter(eng *engine.Engine) (http.Handler, func()) {
	hub := NewEventHub()
	hub.Start()
	hub.SetupEngineListeners(eng)

	sessionStore := newSessionStore(eng.AppConfig().Web.SessionSecret)

	// Parse layout + partials as a base template set. Each page is cloned separately
	// to avoid the "last define wins" problem with {{define "content"}}.
	base := template.New("").Funcs(templateFuncs())
	base = template.Must(base.ParseFS(templateFS, "templates/layout.html", "templates/partials/*.html"))

	pages := []string{
		"templates/dashboard.html",
		"templates/nodes.html",
		"templates/orders.html",
		"templates/corrections.html",
		"templates/diagnostics.html",
		"templates/login.html",
		"templates/config.html",
		"templates/rds_explorer.html",
		"templates/robots.html",
		"templates/payload_types.html",
		"templates/payloads.html",
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

	// Public routes
	r.Get("/", h.handleDashboard)
	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Get("/logout", h.handleLogout)
	r.Get("/nodes", h.handleNodes)
	r.Get("/orders", h.handleOrders)
	r.Get("/orders/detail", h.handleOrderDetail)
	r.Get("/robots", h.handleRobots)

	// API routes (no auth required for read)
	r.Route("/api", func(r chi.Router) {
		r.Get("/nodes", h.apiListNodes)
		r.Get("/nodes/inventory", h.apiNodePayloads)
		r.Get("/orders", h.apiListOrders)
		r.Get("/orders/detail", h.apiGetOrder)
		r.Get("/nodestate", h.apiNodeState)
		r.Get("/robots", h.apiRobotsStatus)
		r.Get("/health", h.apiHealthCheck)
		r.Get("/payload-types", h.apiListPayloadTypes)
		r.Get("/payloads", h.apiListPayloads)
		r.Get("/payloads/detail", h.apiGetPayload)
		r.Get("/payloads/manifest", h.apiListManifest)
		r.Get("/bins/status", h.apiBinOccupancy)
	})

	// Protected routes
	r.Group(func(r chi.Router) {
		r.Use(h.requireAuth)
		r.Post("/nodes/create", h.handleNodeCreate)
		r.Post("/nodes/update", h.handleNodeUpdate)
		r.Post("/nodes/delete", h.handleNodeDelete)
		r.Post("/nodes/sync-rds", h.handleNodeSyncRDS)
		r.Post("/nodes/sync-scene", h.handleSceneSync)
		r.Get("/payload-types", h.handlePayloadTypes)
		r.Post("/payload-types/create", h.handlePayloadTypeCreate)
		r.Post("/payload-types/update", h.handlePayloadTypeUpdate)
		r.Post("/payload-types/delete", h.handlePayloadTypeDelete)
		r.Get("/payloads", h.handlePayloads)
		r.Post("/payloads/create", h.handlePayloadCreate)
		r.Post("/payloads/update", h.handlePayloadUpdate)
		r.Post("/payloads/delete", h.handlePayloadDelete)
		r.Post("/api/payloads/manifest/create", h.apiCreateManifestItem)
		r.Post("/api/payloads/manifest/update", h.apiUpdateManifestItem)
		r.Post("/api/payloads/manifest/delete", h.apiDeleteManifestItem)
		r.Get("/corrections", h.handleCorrections)
		r.Post("/corrections/create", h.handleCorrectionCreate)
		r.Get("/diagnostics", h.handleDiagnostics)
		r.Get("/config", h.handleConfig)
		r.Post("/config/save", h.handleConfigSave)
		r.Get("/rds", h.handleRDSExplorer)
		r.Post("/api/rds/proxy", h.apiRDSProxy)
		r.Post("/api/robots/dispatchable", h.apiRobotSetDispatchable)
		r.Post("/api/robots/redo-failed", h.apiRobotRedoFailed)
		r.Post("/api/robots/manual-finish", h.apiRobotManualFinish)
		r.Post("/api/orders/terminate", h.apiTerminateOrder)
		r.Post("/api/orders/priority", h.apiSetOrderPriority)
	})

	stopFn := func() {
		hub.Stop()
	}

	return r, stopFn
}

func (h *Handlers) render(w http.ResponseWriter, name string, data any) {
	tmpl, ok := h.tmpls[name]
	if !ok {
		log.Printf("render: template %q not found", name)
		http.Error(w, "template not found", http.StatusInternalServerError)
		return
	}
	if err := tmpl.ExecuteTemplate(w, "layout", data); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "template error", http.StatusInternalServerError)
	}
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page":          "login",
		"Authenticated": h.isAuthenticated(r),
	}
	h.render(w, "login.html", data)
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.engine.DB().GetAdminUser(username)
	if err != nil || !checkPassword(user.PasswordHash, password) {
		data := map[string]any{
			"Page":  "login",
			"Error": "Invalid username or password",
		}
		h.render(w, "login.html", data)
		return
	}

	session, _ := h.sessions.Get(r, sessionName)
	session.Values["authenticated"] = true
	session.Values["username"] = username
	if err := session.Save(r, w); err != nil {
		log.Printf("auth: session save error: %v", err)
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (h *Handlers) handleLogout(w http.ResponseWriter, r *http.Request) {
	session, _ := h.sessions.Get(r, sessionName)
	session.Values["authenticated"] = false
	session.Values["username"] = ""
	session.Save(r, w)
	http.Redirect(w, r, "/", http.StatusSeeOther)
}
