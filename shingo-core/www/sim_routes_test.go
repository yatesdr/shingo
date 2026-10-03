//go:build sim

package www

import (
	"context"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol/clock"
	"shingocore/config"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
)

// simOnlyEngine is a ServiceAccess whose only working method is Fleet — the
// injection routes read nothing else, so they can be exercised without a
// database. Any other method panics on the nil embedded interface, which is
// the point: a route that starts reaching past the fleet fails here loudly.
type simOnlyEngine struct {
	ServiceAccess
	backend fleet.Backend
}

func (e simOnlyEngine) Fleet() fleet.Backend { return e.backend }

func simRouter(backend fleet.Backend) http.Handler {
	h := &Handlers{engine: simOnlyEngine{backend: backend}}
	r := chi.NewRouter()
	r.Route("/api", h.registerSimRoutes)
	return r
}

func doSim(t *testing.T, r http.Handler, method, url string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(method, url, nil))
	return rec
}

// The injection routes reach the simulator through the engine's fleet backend,
// validate their parameters, and the robots listing reads back what they set.
func TestSimInjectionRoutes(t *testing.T) {
	sim := simulator.New()
	m := clock.NewManual(time.Unix(0, 0))
	sim.NewDriverFromConfig(config.SimConfig{FleetSize: 2}, m, rand.New(rand.NewSource(1)))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := sim.StartDriver(ctx); err != nil {
		t.Fatalf("StartDriver: %v", err)
	}
	// The fleet snapshot is published by the driver's first tick. Its ticker is
	// registered inside the goroutine, so keep advancing until a tick lands.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if robots, _ := sim.SimRobots(); len(robots) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("driver never published its fleet")
		}
		m.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	r := simRouter(sim)

	if rec := doSim(t, r, http.MethodPost, "/api/sim/robot/fault?id=AMR-01&on=1"); rec.Code != http.StatusOK {
		t.Fatalf("fault on: %d %s", rec.Code, rec.Body)
	}
	if rec := doSim(t, r, http.MethodPost, "/api/sim/robot/deck?id=AMR-02&loaded=1"); rec.Code != http.StatusOK {
		t.Fatalf("deck loaded: %d %s", rec.Code, rec.Body)
	}
	if rec := doSim(t, r, http.MethodPost, "/api/sim/cancel-fails?on=1"); rec.Code != http.StatusOK {
		t.Fatalf("cancel-fails on: %d %s", rec.Code, rec.Body)
	}
	if !sim.CancelFails() {
		t.Fatal("cancel-fails did not reach the simulator")
	}

	// Bad input is refused, not guessed at.
	for _, url := range []string{
		"/api/sim/robot/fault?id=AMR-01",         // no on=
		"/api/sim/robot/fault?id=AMR-99&on=1",    // not in the fleet
		"/api/sim/robot/deck?id=AMR-01&loaded=x", // unreadable
		"/api/sim/cancel-fails",                  // no on=
	} {
		if rec := doSim(t, r, http.MethodPost, url); rec.Code != http.StatusBadRequest {
			t.Errorf("%s: want 400, got %d %s", url, rec.Code, rec.Body)
		}
	}

	rec := doSim(t, r, http.MethodGet, "/api/sim/robots")
	if rec.Code != http.StatusOK {
		t.Fatalf("robots: %d %s", rec.Code, rec.Body)
	}
	var body struct {
		Robots      []simRobotJSON `json:"robots"`
		CancelFails bool           `json:"cancel_fails"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !body.CancelFails {
		t.Errorf("listing must report cancel_fails on: %+v", body)
	}
	want := []simRobotJSON{
		{ID: "AMR-01", Fault: true},
		{ID: "AMR-02", DeckLoaded: true},
	}
	if len(body.Robots) != len(want) {
		t.Fatalf("want %+v, got %+v", want, body.Robots)
	}
	for i := range want {
		if body.Robots[i] != want[i] {
			t.Errorf("robot %d: want %+v, got %+v", i, want[i], body.Robots[i])
		}
	}
}

// A backend that is not the simulator answers 503, not a panic.
func TestSimInjectionRoutesWithoutSimulator(t *testing.T) {
	r := simRouter(nil)
	if rec := doSim(t, r, http.MethodGet, "/api/sim/robots"); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d %s", rec.Code, rec.Body)
	}
}
