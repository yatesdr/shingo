//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// two_stage_clear_docker_test.go — the stamp at apiBinClear. Stage 1's CLEAR
// leaves each cart bare under its OWN marker, derived from the cart's type, so
// several cart types run through one unloader with nothing configured. Stage
// 2's blank CLEAR (SEND ON) gives the cart its type back. A stage 1 with nowhere
// to send the cart is refused before the clear, not after it.

// twoStagePair creates a pair and a node for each window named, returning the
// stage ids and the windows. Where stage 1 sends a cart is derived: a pair
// with one stage-2 window sends there, one with none sends nowhere.
func twoStagePair(t *testing.T, h *Handlers, db *store.DB, name string, s1Windows, s2Windows []string) (int64, int64, []*nodes.Node, []*nodes.Node) {
	t.Helper()
	svc := h.engine.LoaderService()
	s1, s2, err := svc.CreateTwoStage(service.TwoStageCreate{Name: name})
	testutil.MustNoErr(t, err, "create pair")
	mk := func(loader int64, names []string) []*nodes.Node {
		var out []*nodes.Node
		for _, n := range names {
			node := &nodes.Node{Name: n, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(node), "create "+n)
			testutil.MustNoErr(t, svc.SetHome(loader, node.ID, "", "", 0), "window "+n)
			out = append(out, node)
		}
		return out
	}
	return s1, s2, mk(s1, s1Windows), mk(s2, s2Windows)
}

// mintMarker returns a cart type's bare marker.
func mintMarker(t *testing.T, db *store.DB, carrier *bins.BinType) *bins.BinType {
	t.Helper()
	id, err := db.EnsureBareMarker(carrier.ID)
	testutil.MustNoErr(t, err, "derive marker")
	m, err := db.GetBinType(id)
	testutil.MustNoErr(t, err, "read marker")
	return m
}

func cartOfType(t *testing.T, db *store.DB, payload string, nodeID int64, label string, bt *bins.BinType) *bins.Bin {
	t.Helper()
	b := testdb.CreateBinAtNode(t, db, payload, nodeID, label)
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, bt.ID, b.ID)
	testutil.MustNoErr(t, err, "retype "+label)
	return b
}

type clearResp struct {
	ClearedBinTypeCode string `json:"cleared_bin_type_code"`
}

// TestBinClear_StageOneStampsEachCartsOwnMarker: two cart types through one
// stage 1, each cleared with a blank code, each stamped with the marker of its
// own type. An explicit code at a stage 1 is ignored: a stage 1 never re-types a
// cart to a real type. The response names the cart's real type, never a marker.
func TestBinClear_StageOneStampsEachCartsOwnMarker(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	_, _, w1, _ := twoStagePair(t, h, db, "TSC-A", []string{"TSC-S1-W1", "TSC-S1-W2"}, []string{"TSC-S2-W1"})
	cartA := mintBinType(t, db, "TSC-CART-A")
	cartB := mintBinType(t, db, "TSC-CART-B")
	a := cartOfType(t, db, sd.Payload.Code, w1[0].ID, "BIN-TSC-A", cartA)
	b := cartOfType(t, db, sd.Payload.Code, w1[1].ID, "BIN-TSC-B", cartB)

	for _, tc := range []struct {
		win  *nodes.Node
		bin  *bins.Bin
		cart *bins.BinType
		code string
	}{
		{w1[0], a, cartA, ""},
		{w1[1], b, cartB, sd.BinType.Code}, // explicit code: ignored at a stage 1
	} {
		rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
			map[string]any{"node_name": tc.win.Name, "bin_type_code": tc.code})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d, want 200; body=%s", tc.win.Name, rec.Code, rec.Body.String())
		}
		var resp clearResp
		testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
		after, err := db.GetBin(tc.bin.ID)
		testutil.MustNoErr(t, err, "reread")
		marker, err := db.GetBinType(after.BinTypeID)
		testutil.MustNoErr(t, err, "stamped type")
		if marker.Code != tc.cart.Code+service.BareMarkerSuffix || !marker.Bare {
			t.Errorf("%s: stamped %s (bare %v), want the cart's own marker %s", tc.win.Name,
				marker.Code, marker.Bare, tc.cart.Code+service.BareMarkerSuffix)
		}
		if after.PayloadCode != "" {
			t.Errorf("%s: payload %q survived the clear", tc.win.Name, after.PayloadCode)
		}
		if resp.ClearedBinTypeCode != tc.cart.Code {
			t.Errorf("%s: cleared_bin_type_code = %q, want the cart's real type %s — the marker never reaches an operator",
				tc.win.Name, resp.ClearedBinTypeCode, tc.cart.Code)
		}
	}
}

// TestBinClear_BareCartGetsItsCarrierBack: SEND ON at stage 2 is a blank CLEAR
// on a bare cart, and it stamps the marker's carrier — no picker, no code.
func TestBinClear_BareCartGetsItsCarrierBack(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	testdb.SetupStandardData(t, db)
	_, _, _, w2 := twoStagePair(t, h, db, "TSB", []string{"TSB-S1-W1"}, []string{"TSB-S2-W1"})
	cart := mintBinType(t, db, "TSB-CART")
	marker := mintMarker(t, db, cart)
	bin := cartOfType(t, db, "", w2[0].ID, "BIN-TSB", marker)

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", map[string]any{"node_name": w2[0].Name})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var resp clearResp
	testutil.MustNoErr(t, json.NewDecoder(rec.Body).Decode(&resp), "decode")
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reread")
	if after.BinTypeID != cart.ID {
		t.Errorf("SEND ON left the cart as %s, want its carrier %s back", after.BinTypeCode, cart.Code)
	}
	if resp.ClearedBinTypeCode != cart.Code {
		t.Errorf("cleared_bin_type_code = %q, want %s", resp.ClearedBinTypeCode, cart.Code)
	}
}

// TestBinClear_BareCartIgnoresAnExplicitCode: an Edge that predates the
// one-tap SEND ON still shows PUSH AS <type> at stage 2 and posts the picked
// code. A bare cart is a known cart: it gets its own carrier back, whatever was
// picked, exactly as a stage 1 ignores a code.
func TestBinClear_BareCartIgnoresAnExplicitCode(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	testdb.SetupStandardData(t, db)
	_, _, _, w2 := twoStagePair(t, h, db, "TSX", []string{"TSX-S1-W1"}, []string{"TSX-S2-W1"})
	cart := mintBinType(t, db, "TSX-CART")
	other := mintBinType(t, db, "TSX-OTHER")
	bin := cartOfType(t, db, "", w2[0].ID, "BIN-TSX", mintMarker(t, db, cart))

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear",
		map[string]any{"node_name": w2[0].Name, "bin_type_code": other.Code})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reread")
	if after.BinTypeID != cart.ID {
		t.Errorf("an old Edge's PUSH AS %s stamped %s, want the cart's own carrier %s", other.Code, after.BinTypeCode, cart.Code)
	}
}

// TestBinClear_StageOneWithoutDestinationRefusedBeforeClear: a stage 1 whose
// cart has nowhere to go must not clear — the clear commits and the Edge's
// empty-out then has no destination, stranding a bare cart on the window.
func TestBinClear_StageOneWithoutDestinationRefusedBeforeClear(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	_, _, w1, _ := twoStagePair(t, h, db, "TSN", []string{"TSN-S1-W1"}, nil)
	bin := testdb.CreateBinAtNode(t, db, sd.Payload.Code, w1[0].ID, "BIN-TSN")

	rec := postJSON(t, h.apiBinClear, "/api/telemetry/bin-clear", map[string]any{"node_name": w1[0].Name})
	if rec.Code != http.StatusConflict {
		t.Fatalf("status %d, want 409 before the clear; body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "nowhere to send the cart") {
		t.Errorf("body = %s, want it to say the station has nowhere to send the cart", rec.Body.String())
	}
	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "reread")
	if after.PayloadCode != sd.Payload.Code {
		t.Errorf("payload = %q after the refusal, want the bin untouched (%s)", after.PayloadCode, sd.Payload.Code)
	}
}
