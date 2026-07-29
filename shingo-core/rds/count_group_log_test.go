package rds

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// The count-group poll runs at 500ms and produced 334,361 journal lines/day at
// Springfield — 53% of the journal — almost all of it "still empty". These pin
// the edge trigger that replaced the per-tick pair: one line per transition,
// nothing in steady state, and the poll itself untouched.

// scriptedCountGroupClient serves the given bodies in order (the last one
// repeats) and captures every debug line the client emits.
func scriptedCountGroupClient(t *testing.T, bodies ...string) (*Client, func() []string, func()) {
	t.Helper()
	var mu sync.Mutex
	var n int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		body := bodies[min(n, len(bodies)-1)]
		n++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))

	client := NewClient(srv.URL, 2*time.Second)
	var logMu sync.Mutex
	var lines []string
	client.DebugLog = func(format string, args ...any) {
		logMu.Lock()
		defer logMu.Unlock()
		lines = append(lines, fmt.Sprintf(format, args...))
	}
	return client, func() []string {
		logMu.Lock()
		defer logMu.Unlock()
		return append([]string(nil), lines...)
	}, srv.Close
}

func TestCountGroupLog_SteadyStateIsSilentAfterFirstPoll(t *testing.T) {
	client, logged, closeSrv := scriptedCountGroupClient(t, `[]`)
	defer closeSrv()

	for i := range 50 {
		if _, err := client.GetRobotsInCountGroup("Crosswalk_001"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	lines := logged()
	if len(lines) != 1 {
		t.Fatalf("50 identical polls should log once, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "group=Crosswalk_001") || !strings.Contains(lines[0], "empty") {
		t.Fatalf("first-poll line does not name the group and outcome: %q", lines[0])
	}
}

// The acceptance test from the brief: an occupancy change logs exactly once
// per transition, in both directions.
func TestCountGroupLog_OneLinePerTransition(t *testing.T) {
	client, logged, closeSrv := scriptedCountGroupClient(t,
		`[]`, `[]`, // empty, settled
		`["AMR-03"]`, `["AMR-03"]`, `["AMR-03"]`, // becomes occupied
		`[]`, `[]`, // clears
	)
	defer closeSrv()

	for i := range 7 {
		if _, err := client.GetRobotsInCountGroup("Crosswalk_001"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	lines := logged()
	if len(lines) != 3 {
		t.Fatalf("want 3 lines (first poll, occupied, cleared), got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], "occupied by [AMR-03]") || !strings.Contains(lines[1], "was empty") {
		t.Fatalf("transition line should carry both sides: %q", lines[1])
	}
	if !strings.Contains(lines[2], "empty") || !strings.Contains(lines[2], "was occupied by [AMR-03]") {
		t.Fatalf("recovery line should carry both sides: %q", lines[2])
	}
}

// A reordered but identical membership answer is not a transition.
func TestCountGroupLog_ReorderIsNotAChange(t *testing.T) {
	client, logged, closeSrv := scriptedCountGroupClient(t,
		`["AMR-01","AMR-05"]`, `["AMR-05","AMR-01"]`, `["AMR-01","AMR-05"]`,
	)
	defer closeSrv()

	for i := range 3 {
		if _, err := client.GetRobotsInCountGroup("Crosswalk_001"); err != nil {
			t.Fatalf("poll %d: %v", i, err)
		}
	}

	if lines := logged(); len(lines) != 1 {
		t.Fatalf("reordering is not a transition, want 1 line, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
}

// Two groups share one client (main.go builds a single short-timeout client
// for the runner), so the memo has to be keyed per group or one group's
// transitions mask the other's.
func TestCountGroupLog_StatePerGroup(t *testing.T) {
	var mu sync.Mutex
	bodies := map[string]string{"A": `[]`, "B": `[]`}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Group string `json:"group"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		mu.Lock()
		body := bodies[req.Group]
		mu.Unlock()
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, 2*time.Second)
	var lines []string
	client.DebugLog = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	for _, g := range []string{"A", "B", "A", "B"} {
		if _, err := client.GetRobotsInCountGroup(g); err != nil {
			t.Fatalf("group %s: %v", g, err)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("want one first-poll line per group, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}

	mu.Lock()
	bodies["A"] = `["AMR-02"]`
	mu.Unlock()

	for _, g := range []string{"A", "B", "A", "B"} {
		if _, err := client.GetRobotsInCountGroup(g); err != nil {
			t.Fatalf("group %s: %v", g, err)
		}
	}
	if len(lines) != 3 {
		t.Fatalf("only group A changed, want 3 lines total, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[2], "group=A") {
		t.Fatalf("the third line should be group A's transition: %q", lines[2])
	}
}

// An RDS outage keys on "unreachable", not the error text, so a varying
// message (dial vs timeout vs reset) does not re-log at 2Hz. Recovery is a
// transition and does log.
func TestCountGroupLog_OutageLogsOnceAndOnRecovery(t *testing.T) {
	var mu sync.Mutex
	down := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		isDown := down
		mu.Unlock()
		if isDown {
			// Close without a response: a transport-level failure.
			hj, ok := w.(http.Hijacker)
			if !ok {
				return
			}
			conn, _, err := hj.Hijack()
			if err == nil {
				_ = conn.Close()
			}
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, 2*time.Second)
	var lines []string
	client.DebugLog = func(format string, args ...any) { lines = append(lines, fmt.Sprintf(format, args...)) }

	for range 5 {
		if _, err := client.GetRobotsInCountGroup("Crosswalk_001"); err == nil {
			t.Fatal("expected a transport error while the server is down")
		}
	}
	if len(lines) != 1 {
		t.Fatalf("a sustained outage should log once, got %d:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[0], "unreachable") {
		t.Fatalf("outage line should say unreachable: %q", lines[0])
	}

	mu.Lock()
	down = false
	mu.Unlock()

	for range 3 {
		if _, err := client.GetRobotsInCountGroup("Crosswalk_001"); err != nil {
			t.Fatalf("after recovery: %v", err)
		}
	}
	if len(lines) != 2 {
		t.Fatalf("recovery is one transition, got %d lines:\n%s", len(lines), strings.Join(lines, "\n"))
	}
	if !strings.Contains(lines[1], "was unreachable") {
		t.Fatalf("recovery line should name the previous state: %q", lines[1])
	}
}
