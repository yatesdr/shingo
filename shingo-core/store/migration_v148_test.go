//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestV148_SourcingBecomesQueuedWithACause: every order an old Core left
// `sourcing` is `queued` after v148, carrying its own cause or else
// waiting_for_material; order_history is not rewritten; a second run changes
// nothing.
func TestV148_SourcingBecomesQueuedWithACause(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	seed := func(uuid, code, reason, payload string) int64 {
		var id int64
		if err := db.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity,
			queue_code, queue_reason, payload_code)
			VALUES ($1, 'line-1', 'retrieve', 'sourcing', 1, $2, $3, $4) RETURNING id`,
			uuid, code, reason, payload).Scan(&id); err != nil {
			t.Fatalf("seed %s: %v", uuid, err)
		}
		if _, err := db.Exec(`INSERT INTO order_history (order_id, status, detail) VALUES ($1, 'sourcing', 'old')`, id); err != nil {
			t.Fatalf("seed history %s: %v", uuid, err)
		}
		return id
	}
	bare := seed("v148-bare", "", "", "PART-A")
	named := seed("v148-named", "waiting_for_slot", "Waiting for a slot at ALN_007", "PART-A")
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 148`); err != nil {
		t.Fatalf("forget v148: %v", err)
	}
	for run := 1; run <= 2; run++ {
		migrated, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("run %d: re-run migrations: %v", run, err)
		}
		migrated.Close()
		if run == 1 {
			if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 148`); err != nil {
				t.Fatalf("forget v148 again: %v", err)
			}
		}
	}
	for _, c := range []struct {
		id           int64
		code, reason string
	}{
		{bare, "waiting_for_material", "Waiting for material: PART-A"},
		{named, "waiting_for_slot", "Waiting for a slot at ALN_007"},
	} {
		var status, code, reason string
		if err := db.QueryRow(`SELECT status, queue_code, queue_reason FROM orders WHERE id = $1`, c.id).
			Scan(&status, &code, &reason); err != nil {
			t.Fatalf("read order %d: %v", c.id, err)
		}
		if status != "queued" || code != c.code || reason != c.reason {
			t.Errorf("order %d after v148: %s / %q / %q, want queued / %q / %q", c.id, status, code, reason, c.code, c.reason)
		}
	}
	var hist int
	if err := db.QueryRow(`SELECT COUNT(*) FROM order_history WHERE status = 'sourcing'`).Scan(&hist); err != nil {
		t.Fatalf("count history: %v", err)
	}
	if hist != 2 {
		t.Errorf("%d sourcing history rows after v148, want the 2 seeded: order_history is never rewritten", hist)
	}
}
