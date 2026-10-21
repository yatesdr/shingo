//go:build docker

package www

import (
	"net/http"
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// TestBinEdit_TotesOnCart: the Bins page's edit (apiBinAction "update") sets a
// cart's count of totes, with a totes_edited ledger row carrying the from and
// to counts; null sets it unknown; an edit that does not carry it, or carries
// the value it already has, leaves it standing and writes no row. A re-type
// blanks it, whatever the same edit sends, and the re-type's own ledger row
// records the count it blanked.
func TestBinEdit_TotesOnCart(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	eight := 8
	tote := toteType(t, db, "BET-TOTE", &eight)
	kd := mintBinType(t, db, "BET-KD")

	cart := func(label string) int64 {
		b := &bins.Bin{BinTypeID: tote.ID, Label: label, NodeID: &sd.StorageNode.ID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "create "+label)
		testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, sd.Payload.Code, 40), "load "+label)
		return b.ID
	}
	// ledger is the bin's edit rows: op, the uop either side, and the count's
	// from and to ("-" for unknown).
	ledger := func(db *store.DB, id int64) []string {
		rows, err := db.Query(`SELECT op || ' ' || before_uop || '>' || after_uop || ' ' ||
			COALESCE(detail->>'totes_from', '-') || '>' || COALESCE(detail->>'totes_to', '-')
			FROM bin_uop_ledger WHERE bin_id=$1 AND op IN ('totes_edited', 'bin_retyped') ORDER BY id`, id)
		testutil.MustNoErr(t, err, "read the ledger")
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			testutil.MustNoErr(t, rows.Scan(&s), "scan")
			out = append(out, s)
		}
		return out
	}
	update := func(t *testing.T, id int64, params map[string]any) {
		t.Helper()
		code, _, raw := pinBinPost(t, h, id, "update", params)
		if code != http.StatusOK {
			t.Fatalf("update %v: status %d; body=%s", params, code, raw)
		}
	}
	expect := func(t *testing.T, id int64, totes *int, rows []string) {
		t.Helper()
		if got := totesOnCart(t, db, id); (got == nil) != (totes == nil) || (got != nil && *got != *totes) {
			t.Errorf("totes_on_cart = %v, want %v", deref(got), deref(totes))
		}
		if got := ledger(db, id); !reflect.DeepEqual(got, rows) {
			t.Errorf("ledger = %v, want %v", got, rows)
		}
	}

	t.Run("a count is set, then set unknown, each with a row", func(t *testing.T) {
		id := cart("BET-CART-1")
		update(t, id, map[string]any{"totes_on_cart": 16})
		expect(t, id, intPtr(16), []string{"totes_edited 40>40 ->16"})
		update(t, id, map[string]any{"totes_on_cart": nil})
		expect(t, id, nil, []string{"totes_edited 40>40 ->16", "totes_edited 40>40 16>-"})
	})

	t.Run("0 is a base, and is a count", func(t *testing.T) {
		id := cart("BET-CART-0")
		update(t, id, map[string]any{"totes_on_cart": 0})
		expect(t, id, intPtr(0), []string{"totes_edited 40>40 ->0"})
	})

	t.Run("an edit that does not carry it, or carries what it is, leaves it and writes nothing", func(t *testing.T) {
		id := cart("BET-CART-2")
		update(t, id, map[string]any{"totes_on_cart": 16})
		update(t, id, map[string]any{"label": "BET-CART-2B"})
		update(t, id, map[string]any{"label": "BET-CART-2C", "bin_type_id": tote.ID, "totes_on_cart": 16})
		expect(t, id, intPtr(16), []string{"totes_edited 40>40 ->16"})
	})

	t.Run("a negative count is refused and the count stands", func(t *testing.T) {
		id := cart("BET-CART-NEG")
		update(t, id, map[string]any{"totes_on_cart": 16})
		code, _, _ := pinBinPost(t, h, id, "update", map[string]any{"totes_on_cart": -1})
		if code != http.StatusBadRequest {
			t.Errorf("a count of -1: status %d, want 400", code)
		}
		expect(t, id, intPtr(16), []string{"totes_edited 40>40 ->16"})
	})

	t.Run("a re-type blanks it, whatever the same edit sends", func(t *testing.T) {
		id := cart("BET-CART-3")
		update(t, id, map[string]any{"totes_on_cart": 16})
		update(t, id, map[string]any{"bin_type_id": kd.ID, "totes_on_cart": 16})
		expect(t, id, nil, []string{"totes_edited 40>40 ->16", "bin_retyped 40>40 16>-"})
	})

	t.Run("a re-type of a cart with no count records none", func(t *testing.T) {
		id := cart("BET-CART-4")
		update(t, id, map[string]any{"bin_type_id": kd.ID})
		expect(t, id, nil, []string{"bin_retyped 40>40 ->-"})
	})
}
