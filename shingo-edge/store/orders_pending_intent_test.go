package store

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"shingo/protocol"
)

// Edge v4 (orders_pending_intent) and the order-row intent doors.

func hasPendingIntentColumn(t *testing.T, db *DB) bool {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM pragma_table_info('orders') WHERE name='pending_intent'`).Scan(&n); err != nil {
		t.Fatalf("read orders columns: %v", err)
	}
	return n == 1
}

func v3Recorded(t *testing.T, db *DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version=?`,
		edgeMigrationVersion(t, "orders_pending_intent")).Scan(&n); err != nil {
		t.Fatalf("read schema_migrations: %v", err)
	}
	return n
}

// The plant path: an aged DB whose orders table predates the column and which
// has no schema_migrations. The first boot adds the column (existing rows read
// empty); the second boot applies nothing.
func TestV3PendingIntent_AgedThenSecondBoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.CreateOrder("aged-uuid", protocol.OrderTypeMove, nil, false, 1, "D", "", "S", "", true, "P", "", ""); err != nil {
		t.Fatalf("seed order: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE orders DROP COLUMN pending_intent`); err != nil {
		t.Fatalf("simulate pre-v4 orders table: %v", err)
	}
	if _, err := db.Exec(`DROP TABLE schema_migrations`); err != nil {
		t.Fatalf("simulate pre-adoption DB: %v", err)
	}
	db.Close()

	first, err := Open(path)
	if err != nil {
		t.Fatalf("first boot: %v", err)
	}
	if !hasPendingIntentColumn(t, first) {
		t.Fatal("after the first boot orders.pending_intent is missing")
	}
	var intent string
	if err := first.QueryRow(`SELECT pending_intent FROM orders WHERE uuid='aged-uuid'`).Scan(&intent); err != nil || intent != "" {
		t.Fatalf("existing order's pending_intent = (%q, %v), want (\"\", nil)", intent, err)
	}
	rows := schemaRows(t, first)
	if want := 1 + len(edgeMigrations()); rows != want {
		t.Fatalf("first boot recorded %d schema_migrations rows, want %d", rows, want)
	}
	first.Close()

	second, err := Open(path)
	if err != nil {
		t.Fatalf("second boot: %v", err)
	}
	defer second.Close()
	if got := schemaRows(t, second); got != rows {
		t.Fatalf("second boot changed schema_migrations: %d rows, want %d (applies nothing)", got, rows)
	}
	if v3Recorded(t, second) != 1 {
		t.Fatal("v4 not recorded exactly once")
	}
}

// A DB that already carries the column (fresh schema.Apply, or a hand-run fix)
// but has no v4 row: v4 records without touching the table.
func TestV3PendingIntent_NoOpWhenColumnPresent(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "present.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version=?`,
		edgeMigrationVersion(t, "orders_pending_intent")); err != nil {
		t.Fatalf("un-record v4: %v", err)
	}
	db.Close()

	again, err := Open(path)
	if err != nil {
		t.Fatalf("re-open with the column present and v4 unrecorded: %v", err)
	}
	defer again.Close()
	if !hasPendingIntentColumn(t, again) || v3Recorded(t, again) != 1 {
		t.Fatal("v4 did not record cleanly over an existing column")
	}
}

// Exactly-once: take-twice returns false the second time, a stale expected
// value never takes, and of N racers on one intent exactly one wins.
func TestPendingIntentTakeIsExactlyOnce(t *testing.T) {
	t.Parallel()
	db, err := Open(filepath.Join(t.TempDir(), "take.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	id, err := db.CreateOrder("take-uuid", protocol.OrderTypeMove, nil, false, 1, "D", "", "S", "", true, "P", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}

	const intent = `{"kind":"pullback","node_id":7}`
	if err := db.SetOrderPendingIntent(id, intent); err != nil {
		t.Fatalf("set: %v", err)
	}
	if got, err := db.GetOrderPendingIntent(id); err != nil || got != intent {
		t.Fatalf("get = (%q, %v), want (%q, nil)", got, err, intent)
	}
	if ok, err := db.TakeOrderPendingIntent(id, `{"kind":"pullback","node_id":8}`); err != nil || ok {
		t.Fatalf("take with a stale expected value = (%v, %v), want (false, nil)", ok, err)
	}
	if ok, err := db.TakeOrderPendingIntent(id, intent); err != nil || !ok {
		t.Fatalf("first take = (%v, %v), want (true, nil)", ok, err)
	}
	if ok, err := db.TakeOrderPendingIntent(id, intent); err != nil || ok {
		t.Fatalf("second take = (%v, %v), want (false, nil) — an intent fires once", ok, err)
	}
	if got, _ := db.GetOrderPendingIntent(id); got != "" {
		t.Fatalf("after the take pending_intent = %q, want \"\"", got)
	}

	// Race.
	if err := db.SetOrderPendingIntent(id, intent); err != nil {
		t.Fatalf("re-arm: %v", err)
	}
	const racers = 8
	var wins atomic.Int32
	var wg sync.WaitGroup
	for range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := db.TakeOrderPendingIntent(id, intent)
			if err != nil {
				t.Errorf("racing take: %v", err)
			}
			if ok {
				wins.Add(1)
			}
		}()
	}
	wg.Wait()
	if w := wins.Load(); w != 1 {
		t.Fatalf("%d of %d racing takers won one intent, want exactly 1", w, racers)
	}
}
