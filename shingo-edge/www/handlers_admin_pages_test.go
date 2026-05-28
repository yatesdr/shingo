package www

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"shingo/protocol/auth"

	"github.com/go-chi/chi/v5"
)

// ═══════════════════════════════════════════════════════════════════════
// Test router — exercises the pieces of handlers_admin_pages.go that
// don't require a working html/template (renderTemplate is a no-op in
// the test harness) or a non-nil PLCManager.
//
// Covered:
// - Admin-middleware redirects on all 5 admin pages (/config,
//   /processes, /manual-order, /manual-message, /diagnostics).
// - handleLoginPage authenticated-redirect branch (no template render).
// - handleLogin: first-admin bootstrap, valid creds, invalid creds.
// - handleLogout.
//
// handleConfig and handleProcesses themselves panic on PLCManager().
// Only their admin-gate is exercised.
// ═══════════════════════════════════════════════════════════════════════

func newAdminPagesRouter(t *testing.T) (*Handlers, *chi.Mux) {
	t.Helper()
	h, r := newTestHandlers(t)

	r.Get("/login", h.handleLoginPage)
	r.Post("/login", h.handleLogin)
	r.Post("/logout", h.handleLogout)

	r.Group(func(r chi.Router) {
		r.Use(h.adminMiddleware)
		r.Get("/config", h.handleConfig)
		r.Get("/processes", h.handleProcesses)
		r.Get("/manual-order", h.handleManualOrder)
		r.Get("/manual-message", h.handleManualMessage)
		r.Get("/diagnostics", h.handleDiagnostics)
	})

	return h, r
}

// ═══════════════════════════════════════════════════════════════════════
// Admin gating — one check per admin page. Each redirects to /login
// when the caller has no session.
// ═══════════════════════════════════════════════════════════════════════

func TestAdminPages_AdminGate_Redirects(t *testing.T) {
	_, router := newAdminPagesRouter(t)

	paths := []string{
		"/config",
		"/processes",
		"/manual-order",
		"/manual-message",
		"/diagnostics",
	}
	for _, p := range paths {
		t.Run(strings.TrimPrefix(p, "/"), func(t *testing.T) {
			resp := doRequest(t, router, "GET", p, nil, nil)
			if resp.StatusCode != http.StatusSeeOther {
				t.Errorf("GET %s unauthenticated: got %d, want 303", p, resp.StatusCode)
			}
			wantLoc := "/login?next=" + url.QueryEscape(p)
			if loc := resp.Header.Get("Location"); loc != wantLoc {
				t.Errorf("GET %s redirect target: got %q, want %q (next= preservation)", p, loc, wantLoc)
			}
		})
	}
}

// TestAdminGate_LoginRoundTripPreservesNext exercises the Field-notes
// Note 3 round-trip: hit an admin-gated page logged out → land on
// /login?next=<path> → POST /login with credentials → final 303 lands
// on the original target, not /config.
func TestAdminGate_LoginRoundTripPreservesNext(t *testing.T) {
	_, router := newAdminPagesRouter(t)

	// Seed a known user.
	hash, err := auth.HashPassword("rt-pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	testDB.Exec("DELETE FROM admin_users WHERE username = 'roundtripuser'")
	if _, err := testDB.CreateAdminUser("roundtripuser", hash); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}

	// 1) GET admin-gated page while logged out — middleware bounces with
	//    next= preserved.
	resp := doRequest(t, router, "GET", "/diagnostics", nil, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated GET: got %d, want 303", resp.StatusCode)
	}
	loc := resp.Header.Get("Location")
	if want := "/login?next=" + url.QueryEscape("/diagnostics"); loc != want {
		t.Fatalf("redirect location: got %q, want %q", loc, want)
	}

	// 2) POST /login with the next field carried over from the redirect.
	form := url.Values{}
	form.Set("username", "roundtripuser")
	form.Set("password", "rt-pw")
	form.Set("next", "/diagnostics")
	resp = postForm(t, router, "/login", form, nil)
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login POST: got %d, want 303", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/diagnostics" {
		t.Errorf("post-login destination: got %q, want %q (Note 3 regression)", got, "/diagnostics")
	}
}

// TestLogin_NextOpenRedirectRejected pins the safety guard: an attacker-
// supplied off-origin next value must not turn the login into an open
// redirect.
func TestLogin_NextOpenRedirectRejected(t *testing.T) {
	_, router := newAdminPagesRouter(t)

	hash, err := auth.HashPassword("safe-pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	testDB.Exec("DELETE FROM admin_users WHERE username = 'safelogin'")
	if _, err := testDB.CreateAdminUser("safelogin", hash); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}

	cases := []string{
		"https://evil.example/",
		"//evil.example/",
		"http://localhost/",
		"javascript:alert(1)",
	}
	for _, hostile := range cases {
		t.Run(hostile, func(t *testing.T) {
			form := url.Values{}
			form.Set("username", "safelogin")
			form.Set("password", "safe-pw")
			form.Set("next", hostile)
			resp := postForm(t, router, "/login", form, nil)
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("status: got %d, want 303", resp.StatusCode)
			}
			if got := resp.Header.Get("Location"); got != "/config" {
				t.Errorf("hostile next %q: got %q, want /config (safe fallback)", hostile, got)
			}
		})
	}
}

// ═══════════════════════════════════════════════════════════════════════
// handleLoginPage
// ═══════════════════════════════════════════════════════════════════════

func TestLoginPage_AuthenticatedRedirectsToConfig(t *testing.T) {
	h, router := newAdminPagesRouter(t)
	cookie := authCookie(t, h)

	resp := doRequest(t, router, "GET", "/login", nil, cookie)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("expected 303 redirect for authenticated user, got %d", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/config" {
		t.Errorf("redirect target: got %q, want /config", loc)
	}
}

// ═══════════════════════════════════════════════════════════════════════
// handleLogin
// ═══════════════════════════════════════════════════════════════════════

// postForm submits an application/x-www-form-urlencoded POST and returns
// the response. handleLogin uses r.FormValue, which requires a URL-encoded
// body — not JSON.
func postForm(t *testing.T, router *chi.Mux, path string, form url.Values, cookie *http.Cookie) *http.Response {
	t.Helper()
	req := httptest.NewRequest("POST", path, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Result()
}

func TestLogin_BootstrapsFirstAdminUser(t *testing.T) {
	_, router := newAdminPagesRouter(t)

	// Start from a clean admin_users table so AdminUserExists returns false
	// and the handler bootstraps a user from the form values.
	if _, err := testDB.Exec("DELETE FROM admin_users"); err != nil {
		t.Fatalf("clear admin_users: %v", err)
	}

	form := url.Values{}
	form.Set("username", "bootstrapuser")
	form.Set("password", "bootstrap-pw")
	resp := postForm(t, router, "/login", form, nil)

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/config" {
		t.Errorf("redirect target: got %q, want /config", loc)
	}

	// DB state: user was created and its password is stored hashed.
	user, err := testDB.GetAdminUser("bootstrapuser")
	if err != nil {
		t.Fatalf("GetAdminUser: %v", err)
	}
	if user.PasswordHash == "" {
		t.Error("expected password hash to be set")
	}
	if !auth.CheckPassword(user.PasswordHash, "bootstrap-pw") {
		t.Error("stored hash does not verify against bootstrap password")
	}
}

func TestLogin_ValidCredentialsRedirects(t *testing.T) {
	_, router := newAdminPagesRouter(t)

	// Seed a known user (separate from bootstrap test to avoid interleaving).
	hash, err := auth.HashPassword("good-pw")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	testDB.Exec("DELETE FROM admin_users WHERE username = 'validlogin'")
	if _, err := testDB.CreateAdminUser("validlogin", hash); err != nil {
		t.Fatalf("seed admin user: %v", err)
	}

	form := url.Values{}
	form.Set("username", "validlogin")
	form.Set("password", "good-pw")
	resp := postForm(t, router, "/login", form, nil)

	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/config" {
		t.Errorf("redirect target: got %q, want /config", loc)
	}
	// Session cookie should have been issued.
	found := false
	for _, c := range resp.Cookies() {
		if c.Name == sessionName {
			found = true
			break
		}
	}
	if !found {
		t.Error("expected session cookie on successful login")
	}
}

// ═══════════════════════════════════════════════════════════════════════
// handleLogout
// ═══════════════════════════════════════════════════════════════════════

func TestLogout_ClearsSessionAndRedirects(t *testing.T) {
	h, router := newAdminPagesRouter(t)
	cookie := authCookie(t, h)

	resp := postForm(t, router, "/logout", url.Values{}, cookie)
	if resp.StatusCode != http.StatusSeeOther {
		t.Errorf("status: got %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/" {
		t.Errorf("redirect target: got %q, want /", loc)
	}
}
