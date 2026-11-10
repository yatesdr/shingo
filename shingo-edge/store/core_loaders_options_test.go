package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// core_loaders_options_test.go — the per-loader options the core_loaders cache
// mirrors survive a write, a read, and a re-open (migrate() re-running its
// ALTERs over a table that already has the columns).

func TestCoreLoadersCache_OptionsSurviveReopen(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "opts.db")
	db, err := Open(dbPath)
	testutil.MustNoErr(t, err, "open")
	testutil.MustNoErr(t, db.ReplaceCoreLoaders([]protocol.LoaderInfo{{
		LoaderKey: "loader:OPT", Role: "consume", Name: "OPT", Layout: "shared_window",
		Replenishment: "operator", FunnelWindows: true,
		LeavesBare: true, AutoPush: true,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-W1"}},
		Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
	}, {
		LoaderKey: "loader:OPT-DP", Role: "produce", Name: "OPT-DP", Layout: "dedicated_positions",
		Replenishment: "operator", NoClearPartials: true,
		Positions: []protocol.LoaderPosition{{CoreNodeName: "OPT-H1", PayloadCode: "PART-A"}},
	}}), "write the cache")
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = Open(dbPath) // re-runs migrate()
	testutil.MustNoErr(t, err, "re-open")
	defer db.Close()
	l, err := db.GetCoreLoader("loader:OPT")
	if err != nil || l == nil {
		t.Fatalf("read the cache: loader=%v err=%v", l, err)
	}
	if !l.FunnelWindows {
		t.Error("after re-open funnel = false, want true")
	}
	if !l.LeavesBare {
		t.Error("after re-open leaves_bare = false, want true")
	}
	if !l.AutoPush {
		t.Error("after re-open auto_push = false, want true")
	}
	if l.NoClearPartials {
		t.Error("loader:OPT never said no_clear_partials, yet reads it true after re-open")
	}
	dp, err := db.GetCoreLoader("loader:OPT-DP")
	if err != nil || dp == nil || !dp.NoClearPartials {
		t.Errorf("after re-open loader:OPT-DP = %+v (%v), want no_clear_partials true", dp, err)
	}
}

// TestCoreLoadersCache_LeavesBareReplacesTheBareCodeColumn: a core_loaders
// table from before leaves_bare — it still carries bare_bin_type_code, with a
// marker code in it, and predates auto_push and no_clear_partials (v46) — gains
// leaves_bare, auto_push and no_clear_partials and loses bare_bin_type_code on
// the next open. The new columns read false (no_clear_partials false is the
// clear) until the next sync writes them; the cache is regenerated on every
// sync, so the dropped code is not carried anywhere.
func TestCoreLoadersCache_LeavesBareReplacesTheBareCodeColumn(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "older.db")
	db, err := Open(dbPath)
	testutil.MustNoErr(t, err, "open")
	_, err = db.Exec(`ALTER TABLE core_loaders DROP COLUMN leaves_bare`)
	testutil.MustNoErr(t, err, "drop leaves_bare to model an older cache")
	_, err = db.Exec(`ALTER TABLE core_loaders DROP COLUMN auto_push`)
	testutil.MustNoErr(t, err, "drop auto_push to model an older cache")
	_, err = db.Exec(`ALTER TABLE core_loaders DROP COLUMN no_clear_partials`)
	testutil.MustNoErr(t, err, "drop no_clear_partials to model an older cache")
	_, err = db.Exec(`ALTER TABLE core_loaders ADD COLUMN bare_bin_type_code TEXT NOT NULL DEFAULT ''`)
	testutil.MustNoErr(t, err, "add bare_bin_type_code to model an older cache")
	_, err = db.Exec(`INSERT INTO core_loaders (loader_key, role, name, layout, replenishment, bare_bin_type_code)
		VALUES ('loader:OLD', 'consume', 'OLD', 'shared_window', 'operator', 'HALF-TOTE')`)
	testutil.MustNoErr(t, err, "seed an older row")
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = Open(dbPath)
	testutil.MustNoErr(t, err, "re-open")
	defer db.Close()
	l, err := db.GetCoreLoader("loader:OLD")
	if err != nil || l == nil {
		t.Fatalf("read the older row: loader=%v err=%v", l, err)
	}
	if l.LeavesBare {
		t.Error("older row leaves_bare = true, want false")
	}
	if l.AutoPush {
		t.Error("older row auto_push = true, want false")
	}
	if l.NoClearPartials {
		t.Error("older row no_clear_partials = true, want false: the clear")
	}
	var n int
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT COUNT(*) FROM pragma_table_info('core_loaders') WHERE name='bare_bin_type_code'`).Scan(&n),
		"probe the old column")
	if n != 0 {
		t.Error("bare_bin_type_code survived the migration, want it dropped")
	}
}
