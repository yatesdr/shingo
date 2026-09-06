//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// v99 renames payload_manifest.quantity to parts_per_cycle and changes nothing
// else. The value is ALREADY the ratio.
//
// The tests here exist because an earlier version of this migration divided by
// payloads.uop_capacity, on an argument read off the code: resolveTemplateManifest
// copies `quantity` verbatim into a bin manifest in the same call that writes
// uop_remaining = uop_capacity, which implies the two describe one full bin.
// They do not. The fields disagreed because the manifest's copy was meaningless.
//
// Measured at both plants before this ran anywhere: Springfield 127 rows, 122 of
// them at quantity=1, and ZERO rows where quantity = uop_capacity; Hopkinsville
// 17 rows, 16 at 1. A full-bin nominal for a 4500-cycle payload would read 4500.
// So the tests below pin PRESERVATION, and the ones that matter are the values
// a divide would have silently changed.

// revertToPreV99 puts the table back into its pre-v99 shape (the template
// database has already run v99) and clears the version row.
func revertToPreV99(t *testing.T, db *store.DB) {
	t.Helper()
	if _, err := db.Exec(`ALTER TABLE payload_manifest RENAME COLUMN parts_per_cycle TO quantity`); err != nil {
		t.Fatalf("revert column name: %v", err)
	}
	if _, err := db.Exec(`ALTER TABLE payload_manifest ALTER COLUMN quantity SET DEFAULT 0`); err != nil {
		t.Fatalf("revert column default: %v", err)
	}
	if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 99`); err != nil {
		t.Fatalf("clear v99 row: %v", err)
	}
}

// seedPreV99Payload creates a payload and one manifest line holding `stored` in
// the (reverted) quantity column.
func seedPreV99Payload(t *testing.T, db *store.DB, code string, capacity, stored int64) {
	t.Helper()
	var payloadID int64
	if err := db.QueryRow(
		`INSERT INTO payloads (code, uop_capacity) VALUES ($1, $2) RETURNING id`,
		code, capacity).Scan(&payloadID); err != nil {
		t.Fatalf("seed payload %s: %v", code, err)
	}
	if _, err := db.Exec(
		`INSERT INTO payload_manifest (payload_id, part_number, quantity) VALUES ($1, $2, $3)`,
		payloadID, code+"-PART", stored); err != nil {
		t.Fatalf("seed manifest for %s: %v", code, err)
	}
}

func partsPerCycleFor(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	var got int64
	if err := db.QueryRow(`SELECT pm.parts_per_cycle FROM payload_manifest pm
		JOIN payloads p ON p.id = pm.payload_id WHERE p.code = $1`, code).Scan(&got); err != nil {
		t.Fatalf("read parts_per_cycle for %s: %v", code, err)
	}
	return got
}

// TestV99_PreservesEveryStoredValue is the contract. The migration renames a
// column; a rename that changes a number is not a rename.
//
// The cases are the real plant shapes, and the last three are the ones a
// divide-by-capacity would have altered: it flattens a genuine 2-per-cycle
// template to 1 and turns a deliberate 0 into a 1, inventing parts in an
// inventory ledger.
func TestV99_PreservesEveryStoredValue(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)

	cases := []struct {
		code             string
		capacity, stored int64
		why              string
	}{
		{"V99-ORDINARY", 4500, 1, "the overwhelming majority at both plants"},
		{"V99-BIG-CAP", 18000, 1, "the largest capacity at Springfield; a divide rounds this to 0"},
		{"V99-SMALL-CAP", 190, 1, "the smallest; still one per cycle"},
		{"V99-MULTI", 300, 2, "the one genuine multi-part template at Springfield — a divide halves it"},
		{"V99-ZERO", 2400, 0, "four of these at Springfield — a divide invents a part that is not counted"},
		{"V99-EQUAL", 24, 24, "Hopkinsville's Test-Payload, the only row where the two are equal"},
	}
	for _, c := range cases {
		seedPreV99Payload(t, db, c.code, c.capacity, c.stored)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v99: %v", err)
	}
	defer migrated.Close()

	for _, c := range cases {
		if got := partsPerCycleFor(t, migrated, c.code); got != c.stored {
			t.Errorf("%s: parts_per_cycle = %d, want %d unchanged (%s)", c.code, got, c.stored, c.why)
		}
	}
}

// TestV99_DefaultBecomesOne: a template line added with no stated ratio is one
// per cycle. The old default was 0, which contributes nothing to any count
// while looking configured.
func TestV99_DefaultBecomesOne(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v99: %v", err)
	}
	defer migrated.Close()

	var payloadID int64
	if err := migrated.QueryRow(
		`INSERT INTO payloads (code, uop_capacity) VALUES ('V99-DEFAULT', 100) RETURNING id`).
		Scan(&payloadID); err != nil {
		t.Fatalf("seed payload: %v", err)
	}
	// Insert WITHOUT naming parts_per_cycle.
	if _, err := migrated.Exec(
		`INSERT INTO payload_manifest (payload_id, part_number) VALUES ($1, 'P')`, payloadID); err != nil {
		t.Fatalf("insert without a ratio: %v", err)
	}
	if got := partsPerCycleFor(t, migrated, "V99-DEFAULT"); got != 1 {
		t.Errorf("default parts_per_cycle = %d, want 1 — a line with no stated ratio is one per cycle", got)
	}
}

// TestV99_IsIdempotent: the self-heal re-runs any migration whose verify fails,
// and a rename cannot run twice. The guard is the column-name check.
func TestV99_IsIdempotent(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)
	seedPreV99Payload(t, db, "V99-TWICE", 10, 7)

	for i := 0; i < 2; i++ {
		migrated, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("apply %d: %v", i+1, err)
		}
		migrated.Close()
		// Drop the row so the next open re-runs v99 rather than skipping it on
		// the row the previous apply recorded. The column is already renamed,
		// so this is exactly the self-heal's re-entry.
		if _, err := db.Exec(`DELETE FROM schema_migrations WHERE version = 99`); err != nil {
			t.Fatalf("re-clear v99 row: %v", err)
		}
	}

	if got := partsPerCycleFor(t, db, "V99-TWICE"); got != 7 {
		t.Errorf("parts_per_cycle = %d after two applies, want 7 unchanged", got)
	}
}
