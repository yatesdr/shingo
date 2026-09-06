//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// v99 renames payload_manifest.quantity to parts_per_cycle AND divides by the
// payload's UOP capacity. The divide is the part worth a test: the rename on
// its own is a compiler-visible change, while the arithmetic is invisible
// until a CMS quantity ships uop_capacity times too large.
//
// Every test here first puts the table back into its PRE-v99 shape — the
// template database has already run v99 — then drops the version row so the
// re-open genuinely re-applies it.

// revertToPreV99 restores the old column name, default and values for a
// payload's manifest rows, and clears the version row. `nominal` is what the
// old column held: a full-bin count.
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

// seedPreV99Payload creates a payload and one manifest line holding `nominal`
// in the (reverted) quantity column.
func seedPreV99Payload(t *testing.T, db *store.DB, code string, capacity, nominal int64) {
	t.Helper()
	var payloadID int64
	if err := db.QueryRow(
		`INSERT INTO payloads (code, uop_capacity) VALUES ($1, $2) RETURNING id`,
		code, capacity).Scan(&payloadID); err != nil {
		t.Fatalf("seed payload %s: %v", code, err)
	}
	if _, err := db.Exec(
		`INSERT INTO payload_manifest (payload_id, part_number, quantity) VALUES ($1, $2, $3)`,
		payloadID, code+"-PART", nominal); err != nil {
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

// TestV99_DividesTheFullBinNominalByCapacity is the finding this migration
// exists for. A one-part-per-cycle payload stores the full-bin count today; a
// bare rename would leave that count in a column the CMS builder multiplies by
// uop_remaining, so a full bin would post capacity-squared parts.
func TestV99_DividesTheFullBinNominalByCapacity(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)

	// One part per cycle: 24 cycles x 1 part = 24 in a full bin.
	seedPreV99Payload(t, db, "V99-ONE", 24, 24)
	// Five parts per cycle: 24 cycles x 5 parts = 120 in a full bin.
	seedPreV99Payload(t, db, "V99-FIVE", 24, 120)

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v99: %v", err)
	}
	defer migrated.Close()

	if got := partsPerCycleFor(t, migrated, "V99-ONE"); got != 1 {
		t.Errorf("V99-ONE parts_per_cycle = %d, want 1 (24 in a 24-cycle bin is one per cycle)", got)
	}
	if got := partsPerCycleFor(t, migrated, "V99-FIVE"); got != 5 {
		t.Errorf("V99-FIVE parts_per_cycle = %d, want 5 (120 in a 24-cycle bin is five per cycle)", got)
	}
}

// TestV99_RoundTripsTheFullBinCount states the invariant the divide preserves,
// rather than restating the arithmetic: whatever a full bin held before the
// migration, uop_remaining x parts_per_cycle must still say afterwards. That
// is the property every downstream reader depends on, and it holds across the
// range rather than at one convenient point.
func TestV99_RoundTripsTheFullBinCount(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)

	cases := []struct{ capacity, perCycle int64 }{
		{1, 1}, {8, 1}, {24, 1}, {24, 5}, {50, 2}, {1000, 3},
	}
	for i, c := range cases {
		seedPreV99Payload(t, db, codeFor(i), c.capacity, c.capacity*c.perCycle)
	}

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v99: %v", err)
	}
	defer migrated.Close()

	for i, c := range cases {
		code := codeFor(i)
		ppc := partsPerCycleFor(t, migrated, code)
		if fullBin := c.capacity * ppc; fullBin != c.capacity*c.perCycle {
			t.Errorf("%s: a full bin now reads %d, was %d (capacity=%d, parts_per_cycle=%d)",
				code, fullBin, c.capacity*c.perCycle, c.capacity, ppc)
		}
	}
}

func codeFor(i int) string { return "V99-RT-" + string(rune('A'+i)) }

// TestV99_LeavesZeroCapacityAlone: with no capacity there is no ratio to
// recover, and dividing by it would error. Such a payload's bins carry
// uop_remaining = 0, so they contribute no CMS rows under either value.
func TestV99_LeavesZeroCapacityAlone(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)
	seedPreV99Payload(t, db, "V99-ZERO", 0, 17)

	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to apply v99: %v", err)
	}
	defer migrated.Close()

	if got := partsPerCycleFor(t, migrated, "V99-ZERO"); got != 17 {
		t.Errorf("zero-capacity payload parts_per_cycle = %d, want 17 untouched", got)
	}
}

// TestV99_NeverDividesTwice is the one that would bite silently. The self-heal
// re-runs any migration whose verify fails, and a second divide would drive
// every ratio to 1 — a plant would lose its multi-part templates with nothing
// in the log. The guard is that the migration returns early unless a column
// literally named `quantity` is still there.
func TestV99_NeverDividesTwice(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	revertToPreV99(t, db)
	seedPreV99Payload(t, db, "V99-TWICE", 10, 70) // 7 per cycle

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
		t.Errorf("parts_per_cycle = %d after two applies, want 7 — the divide ran twice", got)
	}
}
