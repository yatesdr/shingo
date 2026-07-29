package service

import (
	"testing"
	"time"

	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/stations"
)

// touchStation seeds one station and returns its id.
func touchStation(t *testing.T, db *store.DB) int64 {
	t.Helper()
	processID, err := db.CreateProcess("T-PROC", "touch", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	id, err := db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "T-ST", Name: "Touch Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	return id
}

// sentinel stamps a recognisable last_seen_at so a later write is detectable
// without depending on the column's one-second resolution.
func sentinel(t *testing.T, db *store.DB, id int64) {
	t.Helper()
	if _, err := db.Exec(`UPDATE operator_stations SET last_seen_at='1999-01-01 00:00:00' WHERE id=?`, id); err != nil {
		t.Fatalf("stamp sentinel: %v", err)
	}
}

func lastSeen(t *testing.T, db *store.DB, id int64) string {
	t.Helper()
	var v string
	if err := db.QueryRow(`SELECT COALESCE(last_seen_at,'') FROM operator_stations WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatalf("read last_seen_at: %v", err)
	}
	return v
}

// A board polling steadily must not produce a write per poll. Touch is called on
// every view request — including requests that only joined a running build — and
// it is a WRITE on the single connection, so it queues ahead of the reads the
// boards are waiting for.
func TestTouch_ThrottlesRepeatedSameStatus(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)
	id := touchStation(t, db)

	if err := svc.Touch(id, "online"); err != nil {
		t.Fatalf("first Touch: %v", err)
	}

	sentinel(t, db, id)
	for i := 0; i < 5; i++ {
		if err := svc.Touch(id, "online"); err != nil {
			t.Fatalf("throttled Touch %d: %v", i, err)
		}
	}
	if got := lastSeen(t, db, id); got != "1999-01-01 00:00:00" {
		t.Errorf("last_seen_at = %q — a repeated same-status Touch wrote through the throttle", got)
	}
}

// A status CHANGE must always write immediately: that is the transition an
// operator or dashboard is actually waiting to see.
func TestTouch_StatusChangeWritesThrough(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)
	id := touchStation(t, db)

	if err := svc.Touch(id, "online"); err != nil {
		t.Fatalf("first Touch: %v", err)
	}
	sentinel(t, db, id)
	if err := svc.Touch(id, "degraded"); err != nil {
		t.Fatalf("status-change Touch: %v", err)
	}
	if got := lastSeen(t, db, id); got == "1999-01-01 00:00:00" {
		t.Error("a status change did not write through the throttle")
	}

	var status string
	if err := db.QueryRow(`SELECT health_status FROM operator_stations WHERE id=?`, id).Scan(&status); err != nil {
		t.Fatalf("read health_status: %v", err)
	}
	if status != "degraded" {
		t.Errorf("health_status = %q, want degraded", status)
	}
}

// Once the window has passed the liveness write resumes, so last_seen_at cannot
// go stale indefinitely on a board that keeps polling.
func TestTouch_WritesAgainAfterWindow(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	svc := NewStationService(db)
	id := touchStation(t, db)

	if err := svc.Touch(id, "online"); err != nil {
		t.Fatalf("first Touch: %v", err)
	}
	// Age the recorded write past the window rather than sleeping for it.
	svc.touchMu.Lock()
	svc.touched[id] = touchState{status: "online", at: time.Now().Add(-touchThrottle - time.Second)}
	svc.touchMu.Unlock()

	sentinel(t, db, id)
	if err := svc.Touch(id, "online"); err != nil {
		t.Fatalf("post-window Touch: %v", err)
	}
	if got := lastSeen(t, db, id); got == "1999-01-01 00:00:00" {
		t.Error("liveness write never resumed after the throttle window elapsed")
	}
}
