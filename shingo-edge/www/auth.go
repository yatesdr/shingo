package www

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"

	"github.com/gorilla/sessions"
)

const sessionName = "shingoedge_session"

type sessionStore struct {
	store *sessions.CookieStore
}

func newSessionStore(secret string) *sessionStore {
	var key []byte
	if secret != "" {
		key, _ = base64.StdEncoding.DecodeString(secret)
	}
	if len(key) < 32 {
		key = make([]byte, 32)
		rand.Read(key)
	}
	cs := sessions.NewCookieStore(key)
	cs.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   7 * 24 * 60 * 60, // 7 days
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
	}
	return &sessionStore{store: cs}
}

func (s *sessionStore) get(r *http.Request) *sessions.Session {
	sess, _ := s.store.Get(r, sessionName)
	return sess
}

func (s *sessionStore) getUser(r *http.Request) (username string, ok bool) {
	sess := s.get(r)
	u, exists := sess.Values["username"]
	if !exists {
		return "", false
	}
	username, ok = u.(string)
	return
}

func (s *sessionStore) setUser(w http.ResponseWriter, r *http.Request, username string) {
	sess := s.get(r)
	sess.Values["username"] = username
	sess.Save(r, w)
}

func (s *sessionStore) clear(w http.ResponseWriter, r *http.Request) {
	sess := s.get(r)
	delete(sess.Values, "username")
	sess.Options.MaxAge = -1
	sess.Save(r, w)
}

