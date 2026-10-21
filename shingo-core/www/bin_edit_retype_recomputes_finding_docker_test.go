//go:build docker

package www

import (
	"database/sql"
	"net/http"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// TestBinEdit_ReTypeOfALoadedBinRecomputesUndeclaredCarrier: the Bins page's
// edit (apiBinAction "update") that changes a loaded bin's type re-derives
// bins.undeclared_carrier_at against the new type, both ways.
//
// The finding says "the payload this carrier holds is not declared to travel
// in it". It is a statement about the pair (payload, type), and every other
// writer of either half keeps it true in the same transaction: the produce
// door (writeProductionTx), the clear (ClearForReuseTx) and the payload rule's
// edit (payloads.SetBinTypes). The admin edit wrote the type through
// store.UpdateBin and left the finding as it was, so a re-type onto a type the
// part may not ride raised nothing, and a re-type that fixed one left the bin
// on the operator's list with nothing left to fix. The re-type is also in the
// ledger, with its from and to types as the CLEAR records a re-stamp; a label
// edit writes no ledger row.
func TestBinEdit_ReTypeOfALoadedBinRecomputesUndeclaredCarrier(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	sd := testdb.SetupStandardData(t, db)
	declared := mintBinType(t, db, "BER-TOTE-1")
	undeclared := mintBinType(t, db, "BER-KD-1")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(sd.Payload.ID, []int64{declared.ID}),
		"the part rides only "+declared.Code)

	loaded := func(label string, bt *bins.BinType, finding bool) int64 {
		b := &bins.Bin{BinTypeID: bt.ID, Label: label, NodeID: &sd.StorageNode.ID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "create "+label)
		testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, sd.Payload.Code, 40), "load "+label)
		_, err := db.Exec(`UPDATE bins SET undeclared_carrier_at = CASE WHEN $2 THEN NOW() ELSE NULL END
			WHERE id=$1`, b.ID, finding)
		testutil.MustNoErr(t, err, "the finding as it stands for "+label)
		return b.ID
	}
	findingOf := func(db *store.DB, id int64) bool {
		var at sql.NullTime
		testutil.MustNoErr(t, db.QueryRow(`SELECT undeclared_carrier_at FROM bins WHERE id=$1`, id).Scan(&at),
			"read the finding")
		return at.Valid
	}

	t.Run("onto a type the part may not ride raises it", func(t *testing.T) {
		id := loaded("BER-CART-1", declared, false)
		code, _, raw := pinBinPost(t, h, id, "update", map[string]any{"bin_type_id": undeclared.ID})
		if code != http.StatusOK {
			t.Fatalf("update: status %d; body=%s", code, raw)
		}
		if !findingOf(db, id) {
			t.Errorf("bin %d now holds %s in %s, which its rule does not declare, and "+
				"undeclared_carrier_at is still NULL", id, sd.Payload.Code, undeclared.Code)
		}
	})

	t.Run("onto a type the part may ride clears it", func(t *testing.T) {
		id := loaded("BER-CART-2", undeclared, true)
		code, _, raw := pinBinPost(t, h, id, "update", map[string]any{"bin_type_id": declared.ID})
		if code != http.StatusOK {
			t.Fatalf("update: status %d; body=%s", code, raw)
		}
		if findingOf(db, id) {
			t.Errorf("bin %d now holds %s in %s, which its rule declares, and "+
				"undeclared_carrier_at is still set", id, sd.Payload.Code, declared.Code)
		}
	})

	t.Run("the re-type is in the ledger with its from and to types", func(t *testing.T) {
		id := loaded("BER-CART-3", declared, false)
		ledger := func() []string {
			rows, err := db.Query(`SELECT op || ' ' || before_uop || '>' || after_uop || ' ' ||
				COALESCE(detail->>'dunnage_from', '-') || '>' || COALESCE(detail->>'dunnage_to', '-')
				FROM bin_uop_ledger WHERE bin_id=$1 AND op <> 'set_for_production' ORDER BY id`, id)
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
		code, _, raw := pinBinPost(t, h, id, "update", map[string]any{"label": "BER-CART-3B"})
		if code != http.StatusOK {
			t.Fatalf("label update: status %d; body=%s", code, raw)
		}
		if got := ledger(); len(got) != 0 {
			t.Errorf("a label edit wrote ledger rows %v, want none", got)
		}
		code, _, raw = pinBinPost(t, h, id, "update", map[string]any{"bin_type_id": undeclared.ID})
		if code != http.StatusOK {
			t.Fatalf("update: status %d; body=%s", code, raw)
		}
		want := "bin_retyped 40>40 " + declared.Code + ">" + undeclared.Code
		if got := ledger(); len(got) != 1 || got[0] != want {
			t.Errorf("ledger after the re-type = %v, want [%s]", got, want)
		}
	})
}
