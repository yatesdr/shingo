package store

import (
	"path/filepath"
	"testing"
)

// Edge v42 (drop_processes_auto_cutover_enabled). The frozen chain re-added the
// column on every boot, so a versioned drop alone came back at boot 2; the
// step was removed with it. The proof is the v36 shape: the schema boot 1
// leaves is the schema boot 2 leaves, and the column is not in it.

const autoCutoverProbe = `SELECT COUNT(*) FROM pragma_table_info('processes') WHERE name = 'auto_cutover_enabled'`

// TestDropAutoCutover_StaysDroppedOnTheSecondBoot: a DB carrying the column
// (a pre-v42 Edge's) loses it at the first boot and does not get it back at
// the second. Before, the frozen chain's ALTER re-added it at every boot.
func TestDropAutoCutover_StaysDroppedOnTheSecondBoot(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "aged.db")
	db, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, stmt := range []string{
		`ALTER TABLE processes ADD COLUMN auto_cutover_enabled INTEGER NOT NULL DEFAULT 0`,
		`DELETE FROM schema_migrations WHERE version = 42`,
	} {
		if _, err := db.Exec(stmt); err != nil && probeRaw(t, path, autoCutoverProbe) == 0 {
			t.Fatalf("plant the pre-v42 shape (%s): %v", stmt, err)
		}
	}
	db.Close()

	first := bootAndDump(t, path)
	if probeRaw(t, path, autoCutoverProbe) != 0 {
		t.Fatal("auto_cutover_enabled survived the first boot")
	}
	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("the schema after boot 2 differs from boot 1 — something re-creates the dropped column.\n%s",
			firstDumpDiff(first, second))
	}
	if probeRaw(t, path, autoCutoverProbe) != 0 {
		t.Fatal("auto_cutover_enabled came back at the second boot")
	}
}

// TestDropAutoCutover_FreshNeverHasIt: a fresh DB never carries the column.
func TestDropAutoCutover_FreshNeverHasIt(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "fresh.db")
	first := bootAndDump(t, path)
	second := bootAndDump(t, path)
	if first != second {
		t.Fatalf("fresh DB: boot 2 schema differs from boot 1.\n%s", firstDumpDiff(first, second))
	}
	if probeRaw(t, path, autoCutoverProbe) != 0 {
		t.Fatal("a fresh DB has auto_cutover_enabled")
	}
}
