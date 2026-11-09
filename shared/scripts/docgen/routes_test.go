package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// routesFixture writes a www package (Go files plus templates) under a temp
// root and returns the root.
func routesFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for name, body := range files {
		p := filepath.Join(root, "mod", "www", filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

var routesFixtureFiles = map[string]string{
	"router.go": `package www

func NewRouter() {
	r := chi.NewRouter()
	r.Get("/events", hub.SSEHandler)
	r.Group(func(r chi.Router) {
		r.Use(middleware.Compress(5))
		r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(fs)))
		r.Get("/", h.handleHome)
		r.Get("/rows", h.handleRows)
		r.Get("/old", func(w http.ResponseWriter, req *http.Request) {
			http.Redirect(w, req, "/", http.StatusMovedPermanently)
		})
		r.Get("/wall/{id}", h.handleWall)
		r.Route("/api", func(api chi.Router) {
			h.registerSimRoutes(api)
			api.Get("/nodes", h.apiNodes)
			api.Group(func(g chi.Router) {
				g.Use(h.requireAuth)
				g.Put("/config", h.apiConfigSave)
			})
		})
		r.Group(func(r chi.Router) {
			r.Use(h.requireAuth)
			r.Get("/admin", h.handleAdmin)
		})
	})
}
`,
	"sim_routes.go":      "//go:build sim\n\npackage www\n\nfunc (h *Handlers) registerSimRoutes(r chi.Router) {\n\tr.Get(\"/sim/status\", h.apiSimStatus)\n}\n",
	"sim_routes_stub.go": "//go:build !sim\n\npackage www\n\nfunc (h *Handlers) registerSimRoutes(r chi.Router) {}\n",
	"handlers.go": `package www

var wallTemplates = map[string]string{"map": "wall-map.html"}

func (h *Handlers) handleHome(w http.ResponseWriter, r *http.Request) { h.renderPage(w, r) }
func (h *Handlers) renderPage(w http.ResponseWriter, r *http.Request) { h.render(w, r, "home.html", nil) }
func (h *Handlers) handleRows(w http.ResponseWriter, r *http.Request) {
	tmpl := h.tmpls["home.html"]
	tmpl.ExecuteTemplate(w, "rows", nil)
}
func (h *Handlers) handleWall(w http.ResponseWriter, r *http.Request) {
	h.renderBare(w, wallTemplates["map"], nil)
}
func (h *Handlers) handleAdmin(w http.ResponseWriter, r *http.Request) { h.render(w, r, "admin.html", nil) }
`,
	"templates/home.html":     "{{define \"content\"}}<h1 class=\"x\">Home  page</h1>{{end}}",
	"templates/admin.html":    "{{define \"content\"}}<h1>{{.Name}}</h1>{{end}}",
	"templates/wall-map.html": "<title>{{.Name}} map</title>",
	"templates/layout.html":   "<nav><a href=\"/admin\" class=\"x\">Admin desk</a></nav>",
}

// The route parse, each rule pinned on a row that only exists if it fired:
// Route's prefix and Group's middleware are scoped to their closures (and
// a closure's own parameter name is the router), a register helper is walked
// in place with its sim-only file marked, an inline redirect and a file
// server are named for what they do.
func TestRoutes_TableFromFixture(t *testing.T) {
	m := routesModule{dir: "mod/www", auth: "requireAuth"}
	got, err := routesEmitTable(routesFixture(t, routesFixtureFiles), m)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"10 routes (2 behind `requireAuth`, 1 registered only in a `sim` build)",
		"| `GET` | `/events` | `SSEHandler` | public |",
		"| `ANY` | `/static/*` | (static files) | public |",
		"| `GET` | `/old` | (redirect to `/`) | public |",
		"| `GET` | `/admin` | `handleAdmin` | login |",
		"| `GET` | `/api/nodes` | `apiNodes` | public |",
		"| `PUT` | `/api/config` | `apiConfigSave` | login |",
		"| `GET` | `/api/sim/status` | `apiSimStatus` (sim build) | public |",
		"### Pages and forms",
		"### `/api/config`",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("missing %s\n--- got:\n%s", w, got)
		}
	}
	// Pages and forms come before every /api section.
	if strings.Index(got, "### Pages and forms") > strings.Index(got, "### `/api/") {
		t.Errorf("an /api section precedes Pages and forms:\n%s", got)
	}
}

// A page is a GET whose handler renders a template: through a helper it
// calls, or a var it reads. A literal used as an index (a fragment handler
// reaching into a page's set) is not one; a redirect is not one. The title
// falls back from <h1> to <title> to the nav link's label.
func TestRoutes_PagesFromFixture(t *testing.T) {
	m := routesModule{dir: "mod/www", auth: "requireAuth"}
	root := routesFixture(t, routesFixtureFiles)
	got, err := routesEmitPages(root, m, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "| Path | Template (in `mod/www/templates/`) | Title | Auth |\n|---|---|---|---|\n" +
		"| `/` | `home.html` | Home page | public |\n" +
		"| `/admin` | `admin.html` | Admin desk | login |\n" +
		"| `/wall/{id}` | `wall-map.html` |  | public |\n"
	if got != want {
		t.Errorf("pages:\n%s\nwant:\n%s", got, want)
	}
	pub, err := routesEmitPages(root, m, []string{"public"})
	if err != nil || strings.Contains(pub, "/admin") || !strings.Contains(pub, "`/`") {
		t.Errorf("the public filter kept a login page or dropped a public one (err %v):\n%s", err, pub)
	}
	login, err := routesEmitPages(root, m, []string{"login"})
	if err != nil || !strings.Contains(login, "/admin") || strings.Contains(login, "`/`") {
		t.Errorf("the login filter dropped a login page or kept a public one (err %v):\n%s", err, login)
	}
	if _, err := routesEmitPages(root, m, []string{"admins"}); err == nil {
		t.Error("an unknown filter was accepted")
	}
}

// A router shape the parse does not read is an error, not a route quietly
// left out of the table.
func TestRoutes_RefusesWhatItCannotRead(t *testing.T) {
	files := map[string]string{"router.go": "package www\n\nfunc NewRouter() {\n\tr := chi.NewRouter()\n\tr.Mount(\"/debug\", middleware.Profiler())\n}\n"}
	_, err := routesEmitTable(routesFixture(t, files), routesModule{dir: "mod/www", auth: "requireAuth"})
	if err == nil || !strings.Contains(err.Error(), "Mount") {
		t.Errorf("r.Mount was not refused: %v", err)
	}
}

// On the real tree: the routes and pages that carry each module's auth and
// build boundaries, and every templates/*.html that is not the base layout
// or a shared partial is some route's page.
func TestRoutes_RealTree(t *testing.T) {
	core, err := routesEmitTable(repoRoot, routesModules["shingo-core"])
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{
		"| `GET` | `/api/health` | `apiHealthCheck` | public |",
		"| `PUT` | `/api/config` | `apiConfigSave` | login |",
		"| `GET` | `/api/sim/status` | `apiSimStatus` (sim build) | public |",
		"| `GET` | `/dashboards` | `handleWallDisplaysMoved` | public |",
	} {
		if !strings.Contains(core, w) {
			t.Errorf("core route table: missing %s", w)
		}
	}

	structural := map[string]bool{"layout.html": true, "header.html": true, "footer.html": true}
	for name, m := range routesModules {
		pages, err := routesPages(repoRoot, m)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		reached := map[string]bool{}
		for _, pg := range pages {
			for _, tm := range pg.templates {
				reached[tm] = true
			}
		}
		files, err := filepath.Glob(filepath.Join(repoRoot, filepath.FromSlash(m.dir), "templates", "*.html"))
		if err != nil {
			t.Fatal(err)
		}
		if len(files) == 0 {
			t.Fatalf("%s: no templates found", name)
		}
		for _, f := range files {
			if b := filepath.Base(f); !structural[b] && !reached[b] {
				t.Errorf("%s: templates/%s is no route's page — the parse misses how its handler renders it", name, b)
			}
		}
	}

	edge, err := routesEmitPages(repoRoot, routesModules["shingo-edge"], nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(edge, "| `/operator/station/{id}` | `operator-display.html` |") {
		t.Errorf("edge roster misses the operator station display:\n%s", edge)
	}
	if strings.Contains(edge, "`/material`") {
		t.Errorf("edge roster lists /material, a redirect:\n%s", edge)
	}
}
