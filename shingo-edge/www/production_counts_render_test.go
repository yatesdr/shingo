package www

import (
	"bytes"
	"strings"
	"testing"

	"shingoedge/domain"
	"shingoedge/store"
)

// TestProductionCountsRenderInUoP: the Production page's counts the step-5
// stop left in parts read as every count does (owner, 2026-11-19): at ratio 8
// the UoP, the parts small beside it; at ratio 1 the count as it was, and no
// new attribute. Rows: a manual_swap position's count, the View button's ratio
// for a held partial's words, and a Lineside Buckets pile.
func TestProductionCountsRenderInUoP(t *testing.T) {
	t.Parallel()
	set := edgePageSet(t)
	bin := int64(5)
	manifest := `{"items":[{"part_number":"P"}]}`
	body := func(ratio int) string {
		claim := &domain.NodeClaim{PayloadCode: "P", SwapMode: "manual_swap", PiecesPerCycle: ratio, UOPCapacity: 800}
		view := store.StationNodeView{
			Node:        domain.Node{ID: 1, Name: "N1"},
			Runtime:     &domain.RuntimeState{ActiveBinID: &bin, RemainingUOPCached: 404},
			ActiveClaim: claim,
			BinState:    &domain.NodeBinState{BinLabel: "B1", Occupied: true, Manifest: &manifest},
		}
		var b bytes.Buffer
		data := map[string]any{"StationViews": []domain.OperatorStationView{{Nodes: []store.StationNodeView{view}}}}
		if err := set.ExecuteTemplate(&b, "production-body", data); err != nil {
			t.Fatalf("render production-body at %d: %v", ratio, err)
		}
		return b.String()
	}
	at8, at1 := body(8), body(1)
	if !strings.Contains(at8, `<strong>50</strong> <span class="text-muted">404 parts</span>`) {
		t.Errorf("at 8 the manual_swap count does not read 50 UoP with its parts beside it:\n%s", at8)
	}
	if !strings.Contains(at8, `data-uop-ratio="8" title="View bin contents"`) {
		t.Errorf("at 8 the View button does not carry the ratio for its held partial's words:\n%s", at8)
	}
	if !strings.Contains(at1, `<strong>404</strong>`) || strings.Contains(at1, "404 parts") ||
		strings.Contains(at1, `data-uop-ratio="1" title="View bin contents"`) {
		t.Errorf("at 1 the row is not the one it was:\n%s", at1)
	}

	pile := func(ratio int) string {
		var b bytes.Buffer
		row := linesideBucketRow{BucketID: 1, NodeName: "N1", PayloadCode: "P", Qty: 404, State: "active", Ratio: ratio}
		if err := set.ExecuteTemplate(&b, "production.html", map[string]any{"LinesideRows": []linesideBucketRow{row}}); err != nil {
			t.Fatalf("render production.html at %d: %v", ratio, err)
		}
		return b.String()
	}
	if got := pile(8); !strings.Contains(got, `class="mono">50 <span class="text-muted">404 parts</span></td>`) {
		t.Errorf("at 8 the lineside pile does not read 50 UoP with its parts beside it")
	}
	if got := pile(1); !strings.Contains(got, `class="mono">404</td>`) {
		t.Errorf("at 1 the lineside pile does not read 404 as it did")
	}
}
