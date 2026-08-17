//go:build docker

package store_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// migration_v81_episode_role_test.go — episodes are produce or consume.
//
// v81 rewrites values inside demand_origins rather than altering a schema
// object, so it follows v72's shape exactly: seed the OLD values after
// migrations have run, re-open, and let the failing post-condition self-heal.
// That is the path a plant takes, and it is the only way to put "before" data in
// front of a migration that is already applied.

// seedLegSpelledEpisodes writes one episode per retired leg word, plus the two
// rows this must NOT touch.
func seedLegSpelledEpisodes(t *testing.T, db *store.DB) {
	t.Helper()
	stmts := []string{
		// The two cell episodes in the old vocabulary.
		`INSERT INTO demand_origins (origin_id, episode_key, kind, direction, opened_at)
		 VALUES ('81000000-0000-0000-0000-000000000001', 'cell|SNF2|PANEL-B|supply', 'cell', 'supply', NOW())`,
		`INSERT INTO demand_origins (origin_id, episode_key, kind, direction, opened_at)
		 VALUES ('81000000-0000-0000-0000-000000000002', 'cell|PRESS-2|PANEL-B|evacuate', 'cell', 'evacuate', NOW())`,
		// A THRESHOLD episode. Its key has a different shape and its direction is
		// empty; the rewrite is scoped by the cell| prefix precisely so kinds like
		// this cannot be caught by a bare value match.
		`INSERT INTO demand_origins (origin_id, episode_key, kind, direction, opened_at)
		 VALUES ('81000000-0000-0000-0000-000000000003', 'thr|SM_A_01|PANEL-B', 'threshold', '', NOW())`,
		// A CHANGEOVER episode, same reason.
		`INSERT INTO demand_origins (origin_id, episode_key, kind, direction, opened_at)
		 VALUES ('81000000-0000-0000-0000-000000000004', 'co|plant-a.line-1|7', 'changeover', '', NOW())`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed %q: %v", s, err)
		}
	}
}

func episodeKeyAndDirection(t *testing.T, db *store.DB, originID string) (key, direction string) {
	t.Helper()
	if err := db.QueryRow(
		`SELECT episode_key, direction FROM demand_origins WHERE origin_id = $1`, originID,
	).Scan(&key, &direction); err != nil {
		t.Fatalf("read back episode %s: %v", originID, err)
	}
	return key, direction
}

// TestV81_EpisodesBecomeProduceOrConsume is why this rename is a migration and
// not an edit to two constants.
//
// episode_key.go said these strings were free to change "ONLY UNTIL THE FIRST
// DEPLOY", on the evidence that no plant had run migration 59 and so no stored
// key existed to invalidate. Springfield is at schema_migrations 67 and 59 is
// what creates this table, so that precondition expired and the note's own terms
// make this owed.
//
// BOTH COLUMNS MOVE TOGETHER OR NEITHER DOES. The key is the identity Core's
// partial unique index is built on and the value the reconciler closes against;
// the direction column is what the episode surface renders and what any query
// grouping by role reads. Leaving one behind splits a cell into two rows on one
// surface and none on the other, which is the silent-divergence shape the whole
// campaign exists to stop.
//
// MUTATION (verified): drop the `direction = $2` assignment from the UPDATE and
// the direction assertions fire while the key assertions stay green — one fact
// in two columns, half moved.
func TestV81_EpisodesBecomeProduceOrConsume(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	seedLegSpelledEpisodes(t, db)

	var stale int
	if err := db.QueryRow(`
		SELECT count(*) FROM demand_origins
		 WHERE direction IN ('supply', 'evacuate')`).Scan(&stale); err != nil {
		t.Fatalf("count seeded rows: %v", err)
	}
	if stale != 2 {
		t.Fatalf("seed did not land: %d rows in the old vocabulary, want 2", stale)
	}

	// Re-open: the versioned migrations run again, v81's post-condition sees the
	// seeded rows and fails, and the self-heal re-runs it.
	migrated, err := store.Open(cfg)
	if err != nil {
		t.Fatalf("re-open to re-run migrations: %v", err)
	}
	defer migrated.Close()

	// ── THE CONSUME SIDE ──
	key, dir := episodeKeyAndDirection(t, migrated, "81000000-0000-0000-0000-000000000001")
	if want := "cell|SNF2|PANEL-B|consume"; key != want {
		t.Errorf("episode key = %q, want %q — the key is the identity the partial unique index is "+
			"built on and the value the reconciler closes against", key, want)
	}
	if dir != "consume" {
		t.Errorf("direction = %q, want consume — this column is what the episode surface renders", dir)
	}

	// ── THE PRODUCE SIDE ──
	key, dir = episodeKeyAndDirection(t, migrated, "81000000-0000-0000-0000-000000000002")
	if want := "cell|PRESS-2|PANEL-B|produce"; key != want {
		t.Errorf("episode key = %q, want %q", key, want)
	}
	if dir != "produce" {
		t.Errorf("direction = %q, want produce", dir)
	}

	// ── AND THE KINDS WITH NO CELL BEHIND THEM ARE UNTOUCHED ──
	// Scoped by the cell| prefix, not by a bare value match: matching on the
	// value alone is a predicate that happens to be equivalent today and stops
	// being so the moment another kind stores a word in that column.
	for _, tc := range []struct{ id, wantKey string }{
		{"81000000-0000-0000-0000-000000000003", "thr|SM_A_01|PANEL-B"},
		{"81000000-0000-0000-0000-000000000004", "co|plant-a.line-1|7"},
	} {
		gotKey, gotDir := episodeKeyAndDirection(t, migrated, tc.id)
		if gotKey != tc.wantKey {
			t.Errorf("non-cell episode key = %q, want %q untouched", gotKey, tc.wantKey)
		}
		if gotDir != "" {
			t.Errorf("non-cell episode direction = %q, want empty", gotDir)
		}
	}
}

// TestV81_IsSafeToRunTwice matters here for the reason v72's sibling states: a
// migration whose post-condition fails is re-run on EVERY boot, so a data
// migration that is not safe to repeat compounds itself once per restart.
//
// It matters MORE for this one, because the key rewrite is a string surgery
// rather than a value swap. `left(key, length(key) - length('supply'))` run a
// second time against an already-rewritten key would eat the wrong number of
// characters — which the LIKE '%|supply' guard is what prevents, and which this
// test is what proves.
func TestV81_IsSafeToRunTwice(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	seedLegSpelledEpisodes(t, db)

	for i := 1; i <= 3; i++ {
		again, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("migration run %d: %v", i, err)
		}
		key, dir := episodeKeyAndDirection(t, again, "81000000-0000-0000-0000-000000000001")
		again.Close()
		if want := "cell|SNF2|PANEL-B|consume"; key != want {
			t.Fatalf("run %d: episode key = %q, want %q — a repeated string surgery has eaten into "+
				"the key", i, key, want)
		}
		if dir != "consume" {
			t.Fatalf("run %d: direction = %q, want consume", i, dir)
		}
	}
}
