//go:build docker

package www

import (
	"bytes"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"testing"
	"time"

	"shingo/protocol/debuglog"
	"shingo/protocol/testutil"
	"shingocore/config"
	"shingocore/deviceconfig"
	"shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// The Device Config tab's doors, exercised end-to-end through the REAL
// engine and a REAL Postgres (the house convention: no handrolled mocks
// where the interfaces already exist). The fleet is the simulator wrapped
// with the one capability under test, returning the parse fixture with the
// credential RE-INJECTED — the live payload shape, plaintext on the wire —
// so the handler's scrub-before-store order is proven, not assumed
// (round-2 heron H1 / umber-lynx).

// fakeDeviceConfigFleet implements fleet.DeviceConfigReader over the
// simulator backend, recording relay starts and optionally blocking inside
// a read so the single-flight and pacing doors can be observed.
type fakeDeviceConfigFleet struct {
	*simulator.SimulatorBackend

	mu    sync.Mutex
	raw   []byte
	err   error
	calls []time.Time
	block chan struct{}
}

var _ fleet.Backend = (*fakeDeviceConfigFleet)(nil)
var _ fleet.DeviceConfigReader = (*fakeDeviceConfigFleet)(nil)

func (f *fakeDeviceConfigFleet) ReadDeviceConfig(string) ([]byte, error) {
	f.mu.Lock()
	f.calls = append(f.calls, time.Now())
	f.mu.Unlock()
	if f.block != nil {
		<-f.block
	}
	return f.raw, f.err
}

// liveShapedFixture returns the sanitized testdata model with the
// credential value put back to plaintext — exactly the bytes a robot sends.
func liveShapedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile("../deviceconfig/testdata/SJV-SW600.model")
	testutil.MustNoErr(t, err, "read fixture")
	return bytes.ReplaceAll(raw,
		[]byte(deviceconfig.Fingerprint("12345678")), []byte("12345678"))
}

// testHandlersWithDeviceConfigFleet mirrors testHandlersWithRobotFleet.
func testHandlersWithDeviceConfigFleet(t *testing.T) (*Handlers, *store.DB, *fakeDeviceConfigFleet) {
	t.Helper()

	db := testdb.Open(t)
	sim := &fakeDeviceConfigFleet{SimulatorBackend: simulator.New()}

	cfg := config.Defaults()
	cfg.Messaging.StationID = "test-www"

	eng := engine.New(engine.Config{
		AppConfig: cfg,
		DB:        db,
		Fleet:     sim,
		MsgClient: nil,
		LogFunc:   t.Logf,
	})
	eng.Start()
	t.Cleanup(func() { eng.Stop() })

	hub := NewEventHub()
	hub.Start()
	t.Cleanup(func() { hub.Stop() })

	dbgLog, _ := debuglog.New(64, nil)

	h := &Handlers{
		engine:        eng,
		orchestration: eng,
		sessions:      newSessionStore("test-secret"),
		tmpls:         make(map[string]*template.Template),
		eventHub:      hub,
		debugLog:      dbgLog,
	}
	return h, db, sim
}

// TestDeviceConfigPull_SanitizesBeforeStore: one POST through the real
// engine; BOTH stored columns carry the fingerprint and neither carries the
// plaintext; the response and the public GET carry neither.
func TestDeviceConfigPull_SanitizesBeforeStore(t *testing.T) {
	h, db, sim := testHandlersWithDeviceConfigFleet(t)
	sim.raw = liveShapedFixture(t)

	rec := postJSON(t, h.apiRobotDeviceConfigPull, "/api/robots/device-config/pull", map[string]any{"vehicle": "AMR-01"})
	if rec.Code != http.StatusOK {
		t.Fatalf("pull: %d %s", rec.Code, rec.Body.String())
	}

	var configCol, paramsCol string
	err := db.DB.QueryRow(`SELECT config::text, params::text FROM robot_deviceconfig_pulls`).Scan(&configCol, &paramsCol)
	testutil.MustNoErr(t, err, "read stored pull")
	for name, col := range map[string]string{"config": configCol, "params": paramsCol} {
		if bytes.Contains([]byte(col), []byte("12345678")) {
			t.Errorf("%s column stores PLAINTEXT credential", name)
		}
		if !bytes.Contains([]byte(col), []byte("sha256:")) {
			t.Errorf("%s column missing the fingerprint", name)
		}
	}

	// The public GET ships neither the value nor the fingerprint.
	grec := getPlain(t, h.apiRobotDeviceConfig, "/api/robots/device-config")
	if grec.Code != http.StatusOK {
		t.Fatalf("GET matrix: %d", grec.Code)
	}
	body := grec.Body.String()
	if bytes.Contains([]byte(body), []byte("12345678")) || bytes.Contains([]byte(body), []byte("sha256:")) {
		t.Error("the public matrix payload carries the credential or its fingerprint")
	}
	var view struct {
		CanPull bool `json:"can_pull"`
		Robots  []struct {
			Vehicle string `json:"vehicle"`
			HasPull bool   `json:"has_pull"`
		} `json:"robots"`
	}
	testutil.MustNoErr(t, json.NewDecoder(grec.Body).Decode(&view), "decode matrix")
	if !view.CanPull {
		t.Error("can_pull false on a fleet that implements DeviceConfigReader")
	}
	found := false
	for _, r := range view.Robots {
		if r.Vehicle == "AMR-01" && r.HasPull {
			found = true
		}
	}
	if !found {
		t.Error("AMR-01 has no pulled column after the POST")
	}
}

// TestDeviceConfigPull_RequiresCapability: the plain simulator (no
// DeviceConfigReader) is a degraded tab, answered 501 — never an error body
// pretending to be data.
func TestDeviceConfigPull_RequiresCapability(t *testing.T) {
	h, _ := testHandlers(t) // simulator fleet: no DeviceConfigReader
	rec := postJSON(t, h.apiRobotDeviceConfigPull, "/api/robots/device-config/pull", map[string]any{"vehicle": "AMR-01"})
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("pull without capability: %d %s, want 501", rec.Code, rec.Body.String())
	}
}

// TestDeviceConfigPull_SingleFlight: a second POST while one pull is inside
// the relay gets 409; the holder completes untouched.
func TestDeviceConfigPull_SingleFlight(t *testing.T) {
	h, _, sim := testHandlersWithDeviceConfigFleet(t)
	sim.raw = liveShapedFixture(t)
	sim.block = make(chan struct{})

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postJSON(t, h.apiRobotDeviceConfigPull, "/api/robots/device-config/pull", map[string]any{"vehicle": "AMR-01"})
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		sim.mu.Lock()
		started := len(sim.calls)
		sim.mu.Unlock()
		if started == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(sim.block)

	rec := postJSON(t, h.apiRobotDeviceConfigPull, "/api/robots/device-config/pull", map[string]any{"vehicle": "AMR-02"})
	if rec.Code != http.StatusConflict {
		t.Fatalf("concurrent pull: %d %s, want 409", rec.Code, rec.Body.String())
	}
	if r := <-done; r.Code != http.StatusOK {
		t.Errorf("holder's pull: %d %s, want 200", r.Code, r.Body.String())
	}
}

// TestDeviceConfigPull_ServerGap: two sequential pulls start ≥200 ms apart
// — the pacing is a server property, not client goodwill (SYNTH-round1 §d).
func TestDeviceConfigPull_ServerGap(t *testing.T) {
	h, _, sim := testHandlersWithDeviceConfigFleet(t)
	sim.raw = liveShapedFixture(t)

	for _, v := range []string{"AMR-01", "AMR-02"} {
		rec := postJSON(t, h.apiRobotDeviceConfigPull, "/api/robots/device-config/pull", map[string]any{"vehicle": v})
		if rec.Code != http.StatusOK {
			t.Fatalf("pull %s: %d %s", v, rec.Code, rec.Body.String())
		}
	}
	sim.mu.Lock()
	defer sim.mu.Unlock()
	if len(sim.calls) != 2 {
		t.Fatalf("relay called %d times, want 2", len(sim.calls))
	}
	if gap := sim.calls[1].Sub(sim.calls[0]); gap < 190*time.Millisecond {
		t.Errorf("relay starts %v apart, want ≥200 ms (10 ms scheduling slop allowed)", gap)
	}
}
