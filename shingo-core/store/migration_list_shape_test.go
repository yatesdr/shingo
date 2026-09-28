package store

import (
	"testing"

	"shingo/protocol/migrate"
)

// TestMigrationList_Shape pins the migration list by its structure rather than
// by a head number. A hard-coded head conflicts with every stream that adds a
// migration; what it was guarding — a migration added below the head, or a
// version used twice — is a property of the list, checked here directly.
// LatestMigrationVersion is the last entry, so a strictly increasing list makes
// it the highest version too.
func TestMigrationList_Shape(t *testing.T) {
	ms := migrationList()
	if len(ms) == 0 {
		t.Fatal("empty migration list")
	}
	list := make([]migrate.Migration, len(ms))
	for i, m := range ms {
		list[i] = migrate.Migration{Version: m.version, Name: m.name}
	}
	if err := migrate.CheckOrder(list); err != nil {
		t.Error(err)
	}
	if got, last := LatestMigrationVersion(), ms[len(ms)-1].version; got != last {
		t.Errorf("LatestMigrationVersion() = %d, want the last entry's %d", got, last)
	}
}
