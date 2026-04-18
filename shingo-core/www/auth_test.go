//go:build docker

package www

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"shingo/protocol/auth"
)

// Characterization tests for auth.go — pinned before the Stage 1 refactor
// that replaces h.engine.DB() with named query methods AND moves
// ensureDefaultAdmin off of the Handlers struct. These tests lock the
// observable behaviors:
//
//   - ensureDefaultAdmin must create an "admin"/"admin" user when the table
//     is empty, and must be a no-op when any admin already exists.
//   - handleLogin must, on valid credentials, set session.Values to
//     {authenticated:true, username:<username>} and redirect 303 to "/".
//   - handleLogin must, on invalid credentials, re-render the login page
//     (Status 200, no authenticated session cookie).
//   - handleLogout must flip authenticated to false.

// --- ensureDefaultAdmin ----------------------------------------------------

// TestEnsureDefaultAdmin_CreatesWhenEmpty pins the bootstrap: on an empty
// admin_users table the helper creates "admin" with a bcrypt hash of
// "admin" (verified via auth.CheckPassword).
func TestEnsureDefaultAdmin_CreatesWhenEmpty(t *testing.T) {
	h, db := testHandlers(t)

	exists, err := db.AdminUserExists()
	if err != nil {
		t.Fatalf("admin exists pre: %v", err)
	}
	if exists {
		t.Fatalf("expected no admin users pre-call")
	}

	h.ensureDefaultAdmin(db)

	u, err := db.GetAdminUser("admin")
	if err != nil {
		t.Fatalf("get admin after ensureDefaultAdmin: %v", err)
	}
	if u.Username != "admin" {
		t.Errorf("username: got %q, want admin", u.Username)
	}
	if !auth.CheckPassword(u.PasswordHash, "admin") {
		t.Error("default password should be admin")
	}
}

// TestEnsureDefaultAdmin_IdempotentWhenUserExists pins the no-op branch:
// calling ensureDefaultAdmin when any admin already exists must NOT add a
// second row (the guard is AdminUserExists, not username-specific).
func TestEnsureDefaultAdmin_IdempotentWhenUserExists(t *testing.T) {
	h, db := testHandlers(t)

	// Seed a non-"admin" user so the helper's AdminUserExists check returns
	// true. If the helper ever ignored this guard it would also try to
	// insert the default "admin" row.
	hash, _ := auth.HashPassword("s3cret")
	if err := db.CreateAdminUser("operator", hash); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	h.ensureDefaultAdmin(db)

	// operator is still there; "admin" should NOT have been created.
	if _, err := db.GetAdminUser("operator"); err != nil {
		t.Errorf("pre-existing operator lost: %v", err)
	}
	if _, err := db.GetAdminUser("admin"); err == nil {
		t.Error("ensureDefaultAdmin added an admin row despite existing user")
	}
}

// --- handleLogin ------------------------------------------------------------

// TestHandleLogin_HappyPathSetsSession pins the successful-login contract:
// valid credentials set a session cookie that records
// {authenticated:true, username:<username>} and redirect 303 to "/".
func TestHandleLogin_HappyPathSetsSession(t *testing.T) {
	h, db := testHandlers(t)
	h.ensureDefaultAdmin(db) // admin/admin

	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "admin")

	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.handleLogin(rec, req)

	if rec.Code != http.StatusSeeOther {
		t.Fatalf("status: got %d, want 303; body=%s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "/" {
		t.Errorf("Location: got %q, want /", loc)
	}

	// Session cookie should be set.
	cookies := rec.Result().Cookies()
	var sessionCookie *http.Cookie
	for _, c := range cookies {
		if c.Name == sessionName {
			sessionCookie = c
			break
		}
	}
	if sessionCookie == nil {
		t.Fatalf("no %s cookie in response; got %d cookies", sessionName, len(cookies))
	}

	// Replay the cookie on a follow-up request and verify the session is
	// authenticated with the correct username.
	req2 := httptest.NewRequest(http.MethodGet, "/", nil)
	req2.AddCookie(sessionCookie)
	if !h.isAuthenticated(req2) {
		t.Error("follow-up request should be authenticated via session cookie")
	}
	if got := h.getUsername(req2); got != "admin" {
		t.Errorf("username: got %q, want admin", got)
	}
}

// TestHandleLogin_WrongPasswordRendersLogin pins the invalid-credentials
// branch: the handler renders the login template with an error (Status 200,
// NOT a redirect), and no authenticated session is established.
func TestHandleLogin_WrongPasswordRendersLogin(t *testing.T) {
	h, db := testHandlers(t)
	h.ensureDefaultAdmin(db)

	form := url.Values{}
	form.Set("username", "admin")
	form.Set("password", "not-the-password")

	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.handleLogin(rec, req)

	// Must NOT redirect (303) — the render path returns 200 with the page.
	if rec.Code == http.StatusSeeOther {
		t.Fatalf("wrong password should not redirect; got 303 -> %q",
			rec.Header().Get("Location"))
	}

	// Any session cookie that was set MUST NOT mark the session as
	// authenticated (this is the invariant that matters; whether a cookie
	// is emitted is an implementation detail of gorilla/sessions).
	for _, c := range rec.Result().Cookies() {
		if c.Name != sessionName {
			continue
		}
		req2 := httptest.NewRequest(http.MethodGet, "/", nil)
		req2.AddCookie(c)
		if h.isAuthenticated(req2) {
			t.Error("session should NOT be authenticated after wrong password")
		}
	}
}

// TestHandleLogin_UnknownUserRendersLogin pins the
// GetAdminUser-miss branch. The error path must not leak that the user is
// unknown vs. the password is wrong — both just re-render login.
func TestHandleLogin_UnknownUserRendersLogin(t *testing.T) {
	h, _ := testHandlers(t)

	form := url.Values{}
	form.Set("username", "ghost")
	form.Set("password", "whatever")

	req := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := httptest.NewRecorder()

	h.handleLogin(rec, req)

	if rec.Code == http.StatusSeeOther {
		t.Fatalf("unknown user should not redirect; got 303 -> %q",
			rec.Header().Get("Location"))
	}
}

// --- handleLogout -----------------------------------------------------------

// TestHandleLogout_ClearsAuthenticatedFlag pins the logout contract: after
// logout the session's authenticated flag is false, so a follow-up request
// carrying the rewritten cookie fails isAuthenticated.
func TestHandleLogout_ClearsAuthenticatedFlag(t *testing.T) {
	h, db := testHandlers(t)
	h.ensureDefaultAdmin(db)

	// Establish a logged-in session.
	loginForm := url.Values{}
	loginForm.Set("username", "admin")
	loginForm.Set("password", "admin")
	loginReq := httptest.NewRequest(http.MethodPost, "/login",
		strings.NewReader(loginForm.Encode()))
	loginReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRec := httptest.NewRecorder()
	h.handleLogin(loginRec, loginReq)

	var sessCookie *http.Cookie
	for _, c := range loginRec.Result().Cookies() {
		if c.Name == sessionName {
			sessCookie = c
			break
		}
	}
	if sessCookie == nil {
		t.Fatalf("login did not set session cookie")
	}

	// Call logout with that cookie.
	logoutReq := httptest.NewRequest(http.MethodPost, "/logout", nil)
	logoutReq.AddCookie(sessCookie)
	logoutRec := httptest.NewRecorder()
	h.handleLogout(logoutRec, logoutReq)

	if logoutRec.Code != http.StatusSeeOther {
		t.Fatalf("logout status: got %d, want 303", logoutRec.Code)
	}

	// Use the rewritten cookie from the logout response; it should no
	// longer authenticate.
	var postLogoutCookie *http.Cookie
	for _, c := range logoutRec.Result().Cookies() {
		if c.Name == sessionName {
			postLogoutCookie = c
			break
		}
	}
	if postLogoutCookie == nil {
		t.Fatalf("logout did not rewrite session cookie")
	}

	checkReq := httptest.NewRequest(http.MethodGet, "/", nil)
	checkReq.AddCookie(postLogoutCookie)
	if h.isAuthenticated(checkReq) {
		t.Error("session should NOT be authenticated after logout")
	}
}
