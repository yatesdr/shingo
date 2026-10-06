//go:build docker

package store_test

import (
	"database/sql"
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestV142_VendorTimesReadAsSeconds: v142 re-reads vendor stamps the writer
// stored as epoch milliseconds when the fleet had sent seconds. A wrong-unit row
// is corrected, stamps and duration; a correct row and a row with no vendor
// times are untouched; and a second run changes nothing.
func TestV142_VendorTimesReadAsSeconds(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)

	// What the old writer stored for createTime 1759600000, terminalTime 1759600247.
	wrongCreated := time.UnixMilli(1759600000).UTC()
	wrongCompleted := time.UnixMilli(1759600247).UTC()
	rightCreated := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	rightCompleted := rightCreated.Add(4*time.Minute + 7*time.Second)
	seed := []struct {
		order              int64
		created, completed *time.Time
		vendorDurationMS   int64
	}{
		{9142001, &wrongCreated, &wrongCompleted, 247},
		{9142002, &rightCreated, &rightCompleted, 247000},
		{9142003, nil, nil, 0},
	}
	for _, s := range seed {
		if _, err := db.Exec(`INSERT INTO mission_telemetry (order_id, vendor_created, vendor_completed, vendor_duration_ms)
			VALUES ($1, $2, $3, $4)`, s.order, s.created, s.completed, s.vendorDurationMS); err != nil {
			t.Fatalf("seed mission %d: %v", s.order, err)
		}
	}

	type row struct {
		created, completed sql.NullTime
		durationMS         int64
	}
	read := func(m *store.DB, order int64) row {
		t.Helper()
		var r row
		if err := m.QueryRow(`SELECT vendor_created, vendor_completed, vendor_duration_ms FROM mission_telemetry WHERE order_id=$1`,
			order).Scan(&r.created, &r.completed, &r.durationMS); err != nil {
			t.Fatalf("read mission %d: %v", order, err)
		}
		return r
	}
	apply := func() *store.DB {
		t.Helper()
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 142`); err != nil {
			t.Fatalf("clear v142 row: %v", err)
		}
		m, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("re-open to apply v142: %v", err)
		}
		t.Cleanup(func() { m.Close() })
		return m
	}
	check := func(m *store.DB, pass string) {
		t.Helper()
		wantCreated := time.Date(2025, 10, 4, 17, 46, 40, 0, time.UTC)
		wantCompleted := wantCreated.Add(4*time.Minute + 7*time.Second)
		w := read(m, 9142001)
		if !w.created.Time.Equal(wantCreated) || !w.completed.Time.Equal(wantCompleted) || w.durationMS != 247000 {
			t.Errorf("%s: wrong-unit row = %v → %v, %d ms; want %v → %v, 247000 ms",
				pass, w.created.Time.UTC(), w.completed.Time.UTC(), w.durationMS, wantCreated, wantCompleted)
		}
		r := read(m, 9142002)
		if !r.created.Time.Equal(rightCreated) || !r.completed.Time.Equal(rightCompleted) || r.durationMS != 247000 {
			t.Errorf("%s: correct row changed: %v → %v, %d ms", pass, r.created.Time.UTC(), r.completed.Time.UTC(), r.durationMS)
		}
		n := read(m, 9142003)
		if n.created.Valid || n.completed.Valid || n.durationMS != 0 {
			t.Errorf("%s: row with no vendor times changed: %+v", pass, n)
		}
	}

	check(apply(), "first run")
	check(apply(), "second run")
}
