package www

import (
	"encoding/json"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/sessions"

	"shingo/protocol/auth"
)

const sessionName = "shingocore-session"

func newSessionStore(secret string) *sessions.CookieStore {
	if secret == "" {
		secret = "shingocore-default-secret-change-me"
	}
	s := sessions.NewCookieStore([]byte(secret))
	s.Options.HttpOnly = true
	s.Options.Secure = false // ShinGo runs on plain HTTP (factory LAN)
	s.Options.SameSite = http.SameSiteLaxMode
	return s
}


func (h *Handlers) isAuthenticated(r *http.Request) bool {
	session, err := h.sessions.Get(r, sessionName)
	if err != nil {
		return false
	}
	auth, ok := session.Values["authenticated"].(bool)
	return ok && auth
}

func (h *Handlers) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !h.isAuthenticated(r) {
			if strings.HasPrefix(r.URL.Path, "/api/") {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				json.NewEncoder(w).Encode(map[string]string{"error": "unauthorized"})
				return
			}
			http.Redirect(w, r, "/login", http.StatusSeeOther)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handlers) getUsername(r *http.Request) string {
	session, err := h.sessions.Get(r, sessionName)
	if err != nil {
		return ""
	}
	username, _ := session.Values["username"].(string)
	return username
}

// ensureDefaultAdmin seeds an "admin" user with password "admin" on a
// fresh install. Idempotent: returns immediately if any admin user
// already exists. Routed through AdminService so this file no longer
// needs a *store.DB.
func (h *Handlers) ensureDefaultAdmin() {
	exists, err := h.engine.AdminService().UserExists()
	if err != nil || exists {
		return
	}
	hash, err := auth.HashPassword("admin")
	if err != nil {
		return
	}
	if err := h.engine.AdminService().CreateUser("admin", hash); err != nil {
		log.Printf("auth: ensureDefaultAdmin: %v", err)
	}
}

func (h *Handlers) handleLoginPage(w http.ResponseWriter, r *http.Request) {
	data := map[string]any{
		"Page": "login",
	}
	h.render(w, r, "login.html", data)
}

func (h *Handlers) handleLogin(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := h.engine.AdminService().GetUser(username)
	if err != nil || !auth.CheckPassword(user.PasswordHash, password) {
		data := map[string]any{
			"Page":  "login",
			"Error": "Invalid username or password",
		}
		h.render(w, r, "login.html", data)
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
