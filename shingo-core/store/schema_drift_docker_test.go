//go:build docker

package store_test

import (
	"slices"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// TestSchemaDrift_ADatabaseBornOnTheBaselineHasNone: the boot's drift report
// finds nothing on a database this build made. A report that found something
// here would be a parser of the snapshot or of the catalog that disagrees with
// pg_dump, and every plant would wear its warning.
func TestSchemaDrift_ADatabaseBornOnTheBaselineHasNone(t *testing.T) {
	t.Parallel()
	_, cfg := testdb.OpenWithConfig(t)
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if drift := db.DriftAtBoot(); len(drift) != 0 {
		t.Fatalf("a database born on the baseline drifts:\n  %s", strings.Join(drift, "\n  "))
	}
}

// TestSchemaDrift_NamesAHandDroppedIndexAnAddedColumnAndAMissingTable: each
// hand change to the schema is exactly one line of the boot's report, naming
// its object, and the boot goes on. A dropped table is one line, not one per
// column it took with it.
func TestSchemaDrift_NamesAHandDroppedIndexAnAddedColumnAndAMissingTable(t *testing.T) {
	t.Parallel()
	raw, cfg := testdb.OpenWithConfig(t)
	for _, q := range []string{
		`DROP INDEX idx_orders_status`,
		`ALTER TABLE orders ADD COLUMN zz_hand_added integer`,
		`DROP TABLE tte_samples`,
	} {
		if _, err := raw.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("the boot refused a drifted database; the report refuses nothing: %v", err)
	}
	defer db.Close()
	want := []string{
		"schema drift: column orders.zz_hand_added is in the database and not in this build's schema",
		"schema drift: index idx_orders_status is missing (this build ships it)",
		"schema drift: table tte_samples is missing (this build ships it)",
	}
	if got := db.DriftAtBoot(); !slices.Equal(got, want) {
		t.Fatalf("the boot's drift report:\n  got  %q\n  want %q", got, want)
	}
}
