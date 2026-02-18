package www

import (
	"net/http"

	"github.com/gorilla/sessions"
	"golang.org/x/crypto/bcrypt"

	"warpath/store"
)

const sessionName = "warpath-session"

func newSessionStore(secret string) *sessions.CookieStore {
	if secret == "" {
		secret = "warpath-default-secret-change-me"
	}
	s := sessions.NewCookieStore([]byte(secret))
	s.Options.HttpOnly = true
	s.Options.Secure = false // WarPath runs on plain HTTP (factory LAN)
	s.Options.SameSite = http.SameSiteLaxMode
	return s
}

func hashPassword(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
	return string(hash), err
}

func checkPassword(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
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

func (h *Handlers) ensureDefaultAdmin(db *store.DB) {
	exists, err := db.AdminUserExists()
	if err != nil || exists {
		return
	}
	hash, err := hashPassword("admin")
	if err != nil {
		return
	}
	db.CreateAdminUser("admin", hash)
}
