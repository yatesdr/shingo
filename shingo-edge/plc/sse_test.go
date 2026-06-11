package plc

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingoedge/config"
)

// mockEmitter records emitted events for test assertions.
type mockEmitter struct {
	mu     sync.Mutex
	events []string
}

func (e *mockEmitter) EmitCounterRead(rpID int64, plcName, tagName string, value int64) {}
func (e *mockEmitter) EmitCounterDelta(rpID, processID, styleID, delta, newCount int64, anomaly string) {
}
func (e *mockEmitter) EmitCounterAnomaly(snapID, rpID int64, plc, tag string, old, new int64, atype string) {
}

func (e *mockEmitter) EmitPLCConnected(plcName string) {
	e.mu.Lock()
	e.events = append(e.events, "plc_connected:"+plcName)
	e.mu.Unlock()
}

func (e *mockEmitter) EmitPLCDisconnected(plcName string, err error) {
	e.mu.Lock()
	e.events = append(e.events, "plc_disconnected:"+plcName)
	e.mu.Unlock()
}

func (e *mockEmitter) EmitPLCHealthAlert(plcName string, errMsg string) {
	e.mu.Lock()
	e.events = append(e.events, "plc_health_alert:"+plcName)
	e.mu.Unlock()
}

func (e *mockEmitter) EmitPLCHealthRecover(plcName string) {
	e.mu.Lock()
	e.events = append(e.events, "plc_health_recover:"+plcName)
	e.mu.Unlock()
}

func (e *mockEmitter) EmitCounterReadError(rpID int64, plcName, tagName, errMsg string) {}

func (e *mockEmitter) EmitWarLinkConnected() {
	e.mu.Lock()
	e.events = append(e.events, "warlink_connected")
	e.mu.Unlock()
}

func (e *mockEmitter) EmitWarLinkDisconnected(err error) {
	e.mu.Lock()
	e.events = append(e.events, "warlink_disconnected")
	e.mu.Unlock()
}

func (e *mockEmitter) getEvents() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := make([]string, len(e.events))
	copy(cp, e.events)
	return cp
}

func (e *mockEmitter) waitFor(t *testing.T, event string, timeout time.Duration) {
	t.Helper()
	testutil.EventuallyWithInterval(t, 10*time.Millisecond, timeout, func() bool {
		for _, ev := range e.getEvents() {
			if ev == event {
				return true
			}
		}
		return false
	})
}

// setTestURL parses a httptest.Server URL and sets cfg.WarLink.Host/Port.
func setTestURL(cfg *config.Config, tsURL string) {
	u, _ := url.Parse(tsURL)
	cfg.WarLink.Host = u.Hostname()
	p, _ := strconv.Atoi(u.Port())
	cfg.WarLink.Port = p
}

// newTestServer creates a mock WarLink server that serves both REST endpoints
// and SSE events. restPLCs is the JSON response for GET /api/, sseEvents are
// written to /api/events after connection.
func newTestServer(restPLCs string, sseHandler func(w http.ResponseWriter, r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/":
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, restPLCs)
		case r.URL.Path == "/api/events":
			sseHandler(w, r)
		default:
			// Tags endpoint: return empty map for any PLC
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, "{}")
		}
	}))
}

func TestSSE_RESTBootstrapAndValueChange(t *testing.T) {
	t.Parallel()
	ts := newTestServer(
		`[{"name":"PLC1","status":"Connected","product_name":"1756-L83E"}]`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			// Wait for REST bootstrap to complete, then send value-change
			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(100 * time.Millisecond)

			fmt.Fprintf(w, "event: value-change\ndata: {\"plc\":\"PLC1\",\"tag\":\"Counter1\",\"value\":42,\"type\":\"DINT\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(200 * time.Millisecond)
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	emitter.waitFor(t, "warlink_connected", 2*time.Second)
	emitter.waitFor(t, "plc_connected:PLC1", 2*time.Second)

	// Wait for value-change to be processed
	// Poll until the value-change event is reflected in ReadTag.
	testutil.Eventually(t, 2*time.Second, func() bool {
		val, err := m.ReadTag("PLC1", "Counter1")
		if err != nil {
			return false
		}
		v, ok := val.(float64)
		return ok && v == 42
	})

	val, err := m.ReadTag("PLC1", "Counter1")
	if err != nil {
		t.Fatalf("ReadTag: %v", err)
	}
	if v, ok := val.(float64); !ok || v != 42 {
		t.Errorf("tag value = %v (%T), want 42", val, val)
	}

	m.StopWarLinkPoller()
	m.Stop()
}

func TestSSE_StatusChange(t *testing.T) {
	t.Parallel()
	ts := newTestServer(
		`[{"name":"PLC1","status":"Connected"}]`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(100 * time.Millisecond)

			// PLC disconnects
			fmt.Fprintf(w, "event: status-change\ndata: {\"plc\":\"PLC1\",\"status\":\"disconnected\",\"error\":\"timeout\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(200 * time.Millisecond)
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	emitter.waitFor(t, "plc_connected:PLC1", 2*time.Second)
	emitter.waitFor(t, "plc_disconnected:PLC1", 2*time.Second)
	emitter.waitFor(t, "plc_health_alert:PLC1", 2*time.Second)

	// Verify status normalized to title case
	mp := m.GetPLC("PLC1")
	if mp == nil {
		t.Fatal("PLC1 not found")
	}
	if mp.Status != "Disconnected" {
		t.Errorf("status = %q, want Disconnected", mp.Status)
	}

	m.StopWarLinkPoller()
	m.Stop()
}

func TestSSE_HealthEvent(t *testing.T) {
	t.Parallel()
	ts := newTestServer(
		`[{"name":"PLC1","status":"Connected"}]`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(100 * time.Millisecond)

			// First health: online
			fmt.Fprintf(w, "event: health\ndata: {\"plc\":\"PLC1\",\"driver\":\"ab-eip\",\"online\":true,\"status\":\"ok\",\"error\":\"\",\"timestamp\":\"2025-01-01T00:00:00Z\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(50 * time.Millisecond)

			// Second health: offline
			fmt.Fprintf(w, "event: health\ndata: {\"plc\":\"PLC1\",\"driver\":\"ab-eip\",\"online\":false,\"status\":\"error\",\"error\":\"connection refused\",\"timestamp\":\"2025-01-01T00:00:10Z\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(50 * time.Millisecond)

			// Third health: back online
			fmt.Fprintf(w, "event: health\ndata: {\"plc\":\"PLC1\",\"driver\":\"ab-eip\",\"online\":true,\"status\":\"ok\",\"error\":\"\",\"timestamp\":\"2025-01-01T00:00:20Z\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(200 * time.Millisecond)
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	// Wait for health events to be processed
	emitter.waitFor(t, "plc_health_alert:PLC1", 2*time.Second)
	emitter.waitFor(t, "plc_health_recover:PLC1", 2*time.Second)

	// Verify health data
	h := m.GetPLCHealth("PLC1")
	if h == nil {
		t.Fatal("PLC1 health not found")
	}
	if !h.Online {
		t.Error("expected PLC1 to be online after recovery")
	}
	if h.Driver != "ab-eip" {
		t.Errorf("driver = %q, want ab-eip", h.Driver)
	}

	m.StopWarLinkPoller()
	m.Stop()
}

func TestSSE_StopCancellation(t *testing.T) {
	t.Parallel()
	ts := newTestServer(
		`[]`,
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			fmt.Fprintf(w, ": connected\n\n")
			flusher.Flush()

			// Block until client disconnects
			<-r.Context().Done()
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	emitter.waitFor(t, "warlink_connected", 2*time.Second)

	// Stop should return promptly (not hang)
	done := make(chan struct{})
	go func() {
		m.StopWarLinkPoller()
		close(done)
	}()

	select {
	case <-done:
		// OK
	case <-time.After(3 * time.Second):
		t.Fatal("StopWarLinkPoller did not return in time")
	}

	m.Stop()
}

func TestSSE_Reconnection(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	connectCount := 0

	ts := newTestServer(
		`[]`,
		func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			connectCount++
			n := connectCount
			mu.Unlock()

			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			fmt.Fprintf(w, ": connected\n\n")
			flusher.Flush()

			if n == 1 {
				// First connection: close immediately to trigger reconnect
				return
			}
			// Second connection: stay open
			<-r.Context().Done()
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	// Wait for at least two connections (reconnect after first drop)
	testutil.EventuallyWithInterval(t, 50*time.Millisecond, 5*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return connectCount >= 2
	})

	m.StopWarLinkPoller()
	m.Stop()
}

func TestSSE_PollModeDefault(t *testing.T) {
	t.Parallel()
	// Verify that without mode="sse", StartWarLinkPoller uses poll mode.
	var mu sync.Mutex
	paths := []string{}

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()

		// Return valid PLC list for polling
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, "[]")
	}))
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "" // default = poll

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	// Wait for at least one poll
	emitter.waitFor(t, "warlink_connected", 2*time.Second)

	m.StopWarLinkPoller()

	mu.Lock()
	defer mu.Unlock()
	// Should have hit "/api/" (poll), not "/api/events" (SSE)
	for _, p := range paths {
		if p == "/api/events" {
			t.Errorf("poll mode should not hit /api/events")
		}
	}
	if len(paths) == 0 {
		t.Error("expected at least one request")
	}
}

func TestSSE_ValueChangeCreatesUnknownPLC(t *testing.T) {
	t.Parallel()
	// SSE value-change for a PLC not in REST bootstrap should create the PLC entry
	ts := newTestServer(
		`[]`, // No PLCs in REST bootstrap
		func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			flusher := w.(http.Flusher)

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(100 * time.Millisecond)

			fmt.Fprintf(w, "event: value-change\ndata: {\"plc\":\"NewPLC\",\"tag\":\"Tag1\",\"value\":99,\"type\":\"INT\"}\n\n")
			flusher.Flush()

			// KEEP: localhost server-side event pacing — deterministic, not async wait.
			time.Sleep(200 * time.Millisecond)
		},
	)
	defer ts.Close()

	emitter := &mockEmitter{}
	cfg := config.Defaults()
	setTestURL(cfg, ts.URL)
	cfg.WarLink.Mode = "sse"

	m := NewManager(nil, cfg, emitter, nil)

	m.StartWarLinkPoller()

	emitter.waitFor(t, "warlink_connected", 2*time.Second)
	// Poll until the value-change event is reflected in ReadTag.
	testutil.Eventually(t, 2*time.Second, func() bool {
		val, err := m.ReadTag("NewPLC", "Tag1")
		if err != nil {
			return false
		}
		v, ok := val.(float64)
		return ok && v == 99
	})

	val, err := m.ReadTag("NewPLC", "Tag1")
	if err != nil {
		t.Fatalf("ReadTag: %v", err)
	}
	if v, ok := val.(float64); !ok || v != 99 {
		t.Errorf("tag value = %v (%T), want 99", val, val)
	}

	m.StopWarLinkPoller()
	m.Stop()
}
