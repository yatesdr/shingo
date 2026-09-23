package engine

import (
	"context"
	"testing"

	"shingo/protocol"
)

// TestBuildView_CarriesTheLoadersBareBinType: a window of an unloader Core
// configured with a bare type carries that code on its tile, so the board can
// render the one-tap CLEAR; a loader without one carries "". Read from the same
// loader snapshot the engine's ClearBin substitution reads, so the board and
// the engine agree about which CLEAR stamps bare.
func TestBuildView_CarriesTheLoadersBareBinType(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct{ name, window, bare string }{
		{"bare unloader", "BV-W1", "HALF-TOTE"},
		{"ordinary unloader", "BV-W2", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			eng.loaderStore = newLoaderStore(eng)
			eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
			sid := buildMultiWindowStation(t, db, tc.window+"-PROC", tc.window)
			info := sharedLoaderInfo(tc.window, "consume", "operator", "PART-A", 0, 0)
			info.BareBinTypeCode = tc.bare
			eng.SetCoreLoaders([]protocol.LoaderInfo{info})

			view, err := eng.stationService.BuildView(context.Background(), sid)
			if err != nil {
				t.Fatalf("BuildView: %v", err)
			}
			if len(view.Nodes) != 1 {
				t.Fatalf("nodes = %d, want 1", len(view.Nodes))
			}
			if got := view.Nodes[0].BareBinTypeCode; got != tc.bare {
				t.Errorf("tile bare_bin_type_code = %q, want %q", got, tc.bare)
			}
		})
	}
}
