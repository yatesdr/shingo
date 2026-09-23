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
		Replenishment: "operator", FunnelWindows: true, ChangeoverLoadDirective: true,
		BareBinTypeCode: "HALF-TOTE",
		Positions:       []protocol.LoaderPosition{{CoreNodeName: "OPT-W1", Kind: "window"}},
		Payloads:        []protocol.LoaderPayloadInfo{{PayloadCode: "PART-A"}},
	}}), "write the cache")
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = Open(dbPath) // re-runs migrate()
	testutil.MustNoErr(t, err, "re-open")
	defer db.Close()
	l, err := db.GetCoreLoader("loader:OPT")
	if err != nil || l == nil {
		t.Fatalf("read the cache: loader=%v err=%v", l, err)
	}
	if !l.FunnelWindows || !l.ChangeoverLoadDirective {
		t.Errorf("after re-open funnel/directive = %v/%v, want true/true", l.FunnelWindows, l.ChangeoverLoadDirective)
	}
	if l.BareBinTypeCode != "HALF-TOTE" {
		t.Errorf("after re-open bare bin type = %q, want HALF-TOTE", l.BareBinTypeCode)
	}
}

// TestCoreLoadersCache_BareColumnReachesAnOlderCache: a core_loaders table that
// predates bare_bin_type_code gains it on the next open (the idempotent ALTER),
// and the column reads as "" until the next sync writes it.
func TestCoreLoadersCache_BareColumnReachesAnOlderCache(t *testing.T) {
	t.Parallel()
	dbPath := filepath.Join(t.TempDir(), "older.db")
	db, err := Open(dbPath)
	testutil.MustNoErr(t, err, "open")
	_, err = db.Exec(`ALTER TABLE core_loaders DROP COLUMN bare_bin_type_code`)
	testutil.MustNoErr(t, err, "drop the column to model an older cache")
	_, err = db.Exec(`INSERT INTO core_loaders (loader_key, role, name, layout, replenishment)
		VALUES ('loader:OLD', 'consume', 'OLD', 'shared_window', 'operator')`)
	testutil.MustNoErr(t, err, "seed an older row")
	testutil.MustNoErr(t, db.Close(), "close")

	db, err = Open(dbPath)
	testutil.MustNoErr(t, err, "re-open")
	defer db.Close()
	l, err := db.GetCoreLoader("loader:OLD")
	if err != nil || l == nil {
		t.Fatalf("read the older row: loader=%v err=%v", l, err)
	}
	if l.BareBinTypeCode != "" {
		t.Errorf("older row bare bin type = %q, want \"\"", l.BareBinTypeCode)
	}
}
