package store

import (
	"strings"
	"testing"

	"shingo/protocol/migrate"
)

// TestMigrationList_Shape pins the migration list by its structure rather than
// by a head number. A hard-coded head conflicts with every stream that adds a
// migration; what it was guarding — a migration added below the head, or a
// version used twice — is a property of the list, checked here directly.
//
// Since the squash at v184 the list holds only the migrations above the
// retired chain, so it may be empty, and every entry must sit above
// RetiredChain: one at or below it would never run on a database born on the
// baseline (which records 1..RetiredChain) and would on an upgraded one.
// LatestMigrationVersion is the last entry, or RetiredChain while there is
// none; it is what CoreSchemaVersion reports and an Edge reads.
//
// The entries also run on from RetiredChain with no gap, each with its down
// step: the rollback install-core.sh prints runs `migrate down v` for every v
// between the version a deploy found and the one it reached, so a missing
// version would put a down in the recipe that this build refuses.
func TestMigrationList_Shape(t *testing.T) {
	ms := migrationList()
	list := make([]migrate.Migration, len(ms))
	for i, m := range ms {
		list[i] = migrate.Migration{Version: m.version, Name: m.name}
		if m.version <= RetiredChain {
			t.Errorf("migration v%d (%s) is at or below the retired chain's v%d: the baseline records it as "+
				"applied, so it would run on an upgraded database and never on a new one. Number it above v%d.",
				m.version, m.name, RetiredChain, RetiredChain)
		}
		if want := RetiredChain + 1 + i; m.version != want {
			t.Errorf("migration %d of the list is v%d, want v%d: the versions above the retired chain run on with no "+
				"gap, since the rollback install-core.sh prints runs one `migrate down` per version. Number it v%d.",
				i+1, m.version, want, want)
		}
		if m.down == nil {
			t.Errorf("migration v%d (%s) has no down step: give its entry the step that puts the shape before it "+
				"back, so `shingocore migrate down %d` and the rollback install-core.sh prints can undo it.",
				m.version, m.name, m.version)
		}
	}
	if err := migrate.CheckOrder(list); err != nil {
		t.Error(err)
	}
	want := RetiredChain
	if len(ms) > 0 {
		want = ms[len(ms)-1].version
	}
	if got := LatestMigrationVersion(); got != want {
		t.Errorf("LatestMigrationVersion() = %d, want %d (the last entry, or RetiredChain with none)", got, want)
	}
}

// TestRefusal_NamesMissingVersionsAsRanges pins the refusal's spelling of the
// versions a database lacks: a hole named alone, a run as its two ends.
func TestRefusal_NamesMissingVersionsAsRanges(t *testing.T) {
	for _, c := range []struct {
		in   []int
		want string
	}{
		{[]int{72}, "v72"},
		{[]int{179, 180, 181, 182, 183}, "v179–v183"},
		{[]int{72, 179, 180, 181, 182, 183}, "v72, v179–v183"},
		{[]int{3, 5, 6}, "v3, v5–v6"},
	} {
		if got := formatVersions(c.in); got != c.want {
			t.Errorf("formatVersions(%v) = %q, want %q", c.in, got, c.want)
		}
	}
	if got := formatVersions(nil); strings.TrimSpace(got) != "" {
		t.Errorf("formatVersions(nil) = %q, want empty", got)
	}
}
