package www

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"
)

// nudge_after_write_test.go — the one door every /api write goes through.
//
// The board does not poll, so a write it draws must say so. Rather than each
// handler remembering to (STOPS-tote-stacks §4 listed eleven that did not),
// one middleware on the /api subtree nudges after every write that answered
// 2xx: every route today and every future one, by construction. The real
// router's every route is walked against it in door_walk_test.go
// (cmd/shingoedge).

func TestNudgeAfterWrite_OnlyASuccessfulWriteNudges(t *testing.T) {
	hub := NewEventHub()
	hub.nudgeWindow = 10 * time.Millisecond
	hub.Start()
	defer hub.Stop()
	h := &Handlers{eventHub: hub}

	r := chi.NewRouter()
	r.Use(h.nudgeAfterWrite)
	ok := func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, map[string]string{"status": "ok"}) }
	r.Post("/write", ok)
	r.Put("/put", ok)
	r.Delete("/delete", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	r.Post("/silent", func(http.ResponseWriter, *http.Request) {})
	r.Post("/refused", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusBadRequest, "no") })
	r.Post("/failed", func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusInternalServerError, "no") })
	r.Get("/read", ok)

	cases := []struct {
		method, path string
		nudge        bool
	}{
		{"POST", "/write", true},
		{"PUT", "/put", true},
		{"DELETE", "/delete", true},
		{"POST", "/silent", true}, // nothing written is net/http's 200
		{"POST", "/refused", false},
		{"POST", "/failed", false},
		{"GET", "/read", false},
	}
	for _, c := range cases {
		cl := &sseClient{events: make(chan SSEEvent, 16), lossy: make(chan SSEEvent, 1)}
		hub.register(cl)
		r.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(c.method, c.path, nil))
		got := waitForType(cl, sseNudgeTopic, 150*time.Millisecond)
		hub.unregister(cl)
		if got != c.nudge {
			t.Errorf("%s %s: nudge = %v, want %v", c.method, c.path, got, c.nudge)
		}
	}
}
