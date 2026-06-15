package engine

import (
	"testing"

	"shingo/protocol"
)

// TestCoreLoaderCache_ReplaceAndRead pins the Edge persistent cache: the
// node-list sync fully replaces the cached Core loader config, and the reads
// reassemble loaders with their positions/payloads. Full-state-replace semantics
// (a later sync with fewer/no loaders wipes the rest) are the contract that keeps
// Edge from acting on a stale loader Core deleted.
func TestCoreLoaderCache_ReplaceAndRead(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)

	loaders := []protocol.LoaderInfo{
		{
			Name: "L", LoaderKey: "loader:LOADER-X", Role: "produce",
			Layout: "dedicated_positions", Replenishment: "auto", OutboundDest: "FG-MARKET", ConfigGen: 3,
			Positions: []protocol.LoaderPosition{{CoreNodeName: "POS-1", PayloadCode: "PART-A", MinStock: 2, UOPThreshold: 100}},
		},
		{
			Name: "U", LoaderKey: "loader:UNLOADER-Y", Role: "consume",
			Layout: "shared_window", Replenishment: "operator", ConfigGen: 1,
			Payloads: []protocol.LoaderPayloadInfo{{PayloadCode: "PART-B"}},
		},
	}
	if err := db.ReplaceCoreLoaders(loaders); err != nil {
		t.Fatalf("ReplaceCoreLoaders: %v", err)
	}

	got, err := db.ListCoreLoaders()
	if err != nil || len(got) != 2 {
		t.Fatalf("ListCoreLoaders = %d err=%v, want 2", len(got), err)
	}

	l, err := db.GetCoreLoader("loader:LOADER-X")
	if err != nil || l == nil {
		t.Fatalf("GetCoreLoader: %v", err)
	}
	if l.Layout != "dedicated_positions" || l.ConfigGen != 3 {
		t.Errorf("loader = %+v, want dedicated_positions / gen 3", l)
	}
	if len(l.Positions) != 1 || l.Positions[0].PositionNode != "POS-1" || l.Positions[0].PayloadCode != "PART-A" || l.Positions[0].UOPThreshold != 100 {
		t.Errorf("positions = %+v, want POS-1/PART-A/100", l.Positions)
	}

	// Full-state replace: a later sync with fewer loaders wipes the rest.
	if err := db.ReplaceCoreLoaders(loaders[:1]); err != nil {
		t.Fatalf("re-replace: %v", err)
	}
	if got, _ = db.ListCoreLoaders(); len(got) != 1 {
		t.Errorf("after re-replace = %d, want 1 (full-state replace)", len(got))
	}
	// Empty replace clears the cache.
	if err := db.ReplaceCoreLoaders(nil); err != nil {
		t.Fatalf("empty replace: %v", err)
	}
	if got, _ = db.ListCoreLoaders(); len(got) != 0 {
		t.Errorf("after empty replace = %d, want 0", len(got))
	}
}
