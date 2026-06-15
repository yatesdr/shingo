package engine

import (
	"testing"

	"shingoedge/store"
)

// TestProjectCoreLoader_BranchesOnLayout pins the C1 live-bug fix at its surviving
// home: projectCoreLoader (the single cache→*domain.Loader projection point) branches
// on the loader's LAYOUT, not on "does it have positions." The regression case is a
// shared_window loader that HAS window homes — the old len(Positions)>0 heuristic took
// the dedicated branch and emitted blank-payload members; it must now project windows
// carrying the shared set. (Migrated from the deleted loaderMemberNodes shim helper —
// same intent, asserted on the aggregate the runtime actually resolves through.)
func TestProjectCoreLoader_BranchesOnLayout(t *testing.T) {
	t.Parallel()

	t.Run("shared_window with window homes -> windows + shared set (no blank payload)", func(t *testing.T) {
		l, err := projectCoreLoader(store.CoreLoader{
			LoaderKey: "loader:WELD-1", Role: "produce", Layout: "shared_window",
			InboundSource: "EMPTY-SUPER", OutboundDest: "FG-MARKET",
			Positions: []store.CoreLoaderPosition{
				{PositionNode: "WELD-1-W1", PayloadCode: "", Kind: "window"},
				{PositionNode: "WELD-1-W2", PayloadCode: "", Kind: "window"},
			},
			Payloads: []store.CoreLoaderPayload{{PayloadCode: "PART-A"}, {PayloadCode: "PART-B"}},
		})
		if err != nil {
			t.Fatalf("projectCoreLoader: %v", err)
		}
		if !l.IsShared() {
			t.Fatalf("layout = %v, want shared_window", l.Layout())
		}
		ws := l.Windows()
		if len(ws) != 2 || ws[0].Node != "WELD-1-W1" || ws[1].Node != "WELD-1-W2" {
			t.Errorf("windows = %+v, want [WELD-1-W1 WELD-1-W2]", ws)
		}
		ps := l.PayloadSet()
		if len(ps) != 2 || ps[0] != "PART-A" || ps[1] != "PART-B" {
			t.Errorf("payload set = %v, want [PART-A PART-B]", ps)
		}
		for _, p := range ps {
			if p == "" {
				t.Fatalf("regression: shared loader projected a blank payload: %v", ps)
			}
		}
	})

	t.Run("dedicated_positions -> one position per home with its payload", func(t *testing.T) {
		l, err := projectCoreLoader(store.CoreLoader{
			LoaderKey: "loader:SLN-2", Role: "produce", Layout: "dedicated_positions",
			Positions: []store.CoreLoaderPosition{
				{PositionNode: "HOME-1", PayloadCode: "PART-A", Kind: "dedicated", MinStock: 2},
				{PositionNode: "HOME-2", PayloadCode: "PART-B", Kind: "dedicated", MinStock: 3},
			},
		})
		if err != nil {
			t.Fatalf("projectCoreLoader: %v", err)
		}
		if !l.IsDedicated() {
			t.Fatalf("layout = %v, want dedicated_positions", l.Layout())
		}
		pos := l.Positions()
		if len(pos) != 2 {
			t.Fatalf("positions = %d, want 2", len(pos))
		}
		if pos[0].Node != "HOME-1" || pos[0].Payload != "PART-A" || pos[0].MinStock != 2 {
			t.Errorf("position 0 = %+v, want HOME-1/PART-A/2", pos[0])
		}
		if pos[1].Node != "HOME-2" || pos[1].Payload != "PART-B" {
			t.Errorf("position 1 = %+v, want HOME-2/PART-B", pos[1])
		}
	})

	t.Run("unknown layout fails closed (no positions heuristic)", func(t *testing.T) {
		// The deleted loaderMemberNodes shim fell back to a positions heuristic for a
		// blank layout; the aggregate projection deliberately fails closed (Refresh
		// then skips the loader), since post-cutover Core always stamps a layout
		// (BuildLoaderInfos). Pin the fail-closed contract.
		if _, err := projectCoreLoader(store.CoreLoader{
			LoaderKey: "loader:LEG-1", Role: "produce", Layout: "",
			Positions: []store.CoreLoaderPosition{{PositionNode: "LEG-1-P", PayloadCode: "PART-X", Kind: "dedicated"}},
		}); err == nil {
			t.Fatal("projectCoreLoader with blank layout should error (fail closed), got nil")
		}
	})
}
