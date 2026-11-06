package rds

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// unreadableEmitter is mockPollerEmitter plus the optional UnreadableEmitter.
type unreadableEmitter struct {
	mockPollerEmitter
	umu        sync.Mutex
	unreadable []string // "rdsID|reason"
	readable   []string
}

func (u *unreadableEmitter) EmitOrderUnreadable(_ int64, rdsID string, _ time.Time, reason string) {
	u.umu.Lock()
	defer u.umu.Unlock()
	u.unreadable = append(u.unreadable, rdsID+"|"+reason)
}

func (u *unreadableEmitter) EmitOrderReadable(_ int64, rdsID string) {
	u.umu.Lock()
	defer u.umu.Unlock()
	u.readable = append(u.readable, rdsID)
}

// TestPollerReportsAnOrderRDSCannotReportOn pins the rule: one order whose reads
// keep failing while others read fine is reported once, in RDS's words, and
// cleared when it reads again; a pass where EVERY read fails (RDS down) reports
// nothing per order. Before 2026-11-05 a failed read was a log line and the
// order kept "in transit" on every screen indefinitely.
func TestPollerReportsAnOrderRDSCannotReportOn(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	lost, down := true, false
	write := func(w http.ResponseWriter, body string) {
		if _, err := w.Write([]byte(body)); err != nil {
			t.Errorf("write response: %v", err)
		}
	}
	srv, client := testServer(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case down:
			w.WriteHeader(http.StatusServiceUnavailable)
		case lost && strings.HasSuffix(r.URL.Path, "/rds-lost"):
			write(w, `{"code":1,"msg":"order details is empty"}`)
		default:
			id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
			write(w, `{"code":0,"msg":"ok","id":"`+id+`","state":"RUNNING","vehicle":"AMR-06"}`)
		}
	})
	defer srv.Close()

	em := &unreadableEmitter{}
	p := NewPoller(client, em, &mockResolver{}, time.Minute)
	p.unreadableAfter = time.Nanosecond
	p.Track("rds-lost")
	p.Track("rds-fine")

	p.poll()
	p.poll()
	em.umu.Lock()
	got := append([]string(nil), em.unreadable...)
	em.umu.Unlock()
	if want := "rds-lost|RDS error 1: order details is empty"; len(got) != 1 || got[0] != want {
		t.Fatalf("unreadable reports = %q, want exactly [%q] (once, in RDS's words)", got, want)
	}

	mu.Lock()
	lost = false
	mu.Unlock()
	p.poll()
	em.umu.Lock()
	if len(em.readable) != 1 || em.readable[0] != "rds-lost" {
		t.Errorf("readable reports = %q, want [rds-lost]", em.readable)
	}
	em.umu.Unlock()

	// RDS down: every read fails. Nothing per order, however long.
	mu.Lock()
	down = true
	mu.Unlock()
	p.poll()
	p.poll()
	em.umu.Lock()
	defer em.umu.Unlock()
	if len(em.unreadable) != 1 {
		t.Errorf("unreadable reports after RDS went down = %q, want still 1 — a fleet-wide outage is "+
			"not one alert per order", em.unreadable)
	}
}
