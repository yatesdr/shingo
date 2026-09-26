package engine

import (
	"context"
	"testing"

	"shingo/protocol"
)

// TestBuildView_CarriesLeavesBare: a window of the stage-1 half of a
// two-stage unloader carries leaves_bare on its tile, so the board renders the
// one-tap Full off; an ordinary unloader's window does not. The tile carries a
// flag, never a bin-type code: the marker is Core's and no operator sees it.
func TestBuildView_CarriesLeavesBare(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, window string
		leavesBare   bool
	}{
		{"stage-1 unloader", "BV-W1", true},
		{"ordinary unloader", "BV-W2", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			eng.loaderStore = newLoaderStore(eng)
			eng.stationService.SetLoaderResolver(stationLoaderResolver{eng})
			sid := buildMultiWindowStation(t, db, tc.window+"-PROC", tc.window)
			info := sharedLoaderInfo(tc.window, "consume", "operator", "PART-A", 0, 0)
			info.LeavesBare = tc.leavesBare
			eng.SetCoreLoaders([]protocol.LoaderInfo{info})

			view, err := eng.stationService.BuildView(context.Background(), sid)
			if err != nil {
				t.Fatalf("BuildView: %v", err)
			}
			if len(view.Nodes) != 1 {
				t.Fatalf("nodes = %d, want 1", len(view.Nodes))
			}
			if got := view.Nodes[0].LeavesBare; got != tc.leavesBare {
				t.Errorf("tile leaves_bare = %v, want %v", got, tc.leavesBare)
			}
		})
	}
}
