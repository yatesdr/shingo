package store

import (
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// core_loaders_options_test.go — the per-loader options the core_loaders cache
// mirrors survive a write, a read, and a re-open (a start writes nothing over
// a table that already has the columns).

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

	db, err = Open(dbPath) // as a restart does
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
