//go:build docker

package engine

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store/bins"
)

// TestClearDeclare_InTheCMSClearsTransaction: at a CMS boundary the clear owns
// the transaction that books the departure (ClearForReuseStampAndBookDeparture's
// second arm), and the declared height is written in that same transaction,
// after the clear: layers × the cart's own totes per layer, with the
// departure booked as it always was. The other arm is the www door's
// TestBinClear_DeclaresTheHeight.
func TestClearDeclare_InTheCMSClearsTransaction(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	eng := newUnstartedEngine(t, db, simulator.New())

	bin, nodeID := clearBoundaryBin(t, db, "TD-CMS-PAYLOAD", 24, 10)
	eight := 8
	tote := &bins.BinType{Code: "TD-CMS-TOTE", TotesPerLayer: &eight}
	testutil.MustNoErr(t, db.CreateBinType(tote), "create the tote carrier")
	_, err := db.Exec(`UPDATE bins SET bin_type_id=$2 WHERE id=$1`, bin.ID, tote.ID)
	testutil.MustNoErr(t, err, "type the cart")

	two := 2
	_, err = eng.ClearForReuseStampAndBookDeparture(bin.ID, nodeID, nil, bins.StampNone, &two)
	testutil.MustNoErr(t, err, "clear and declare at a boundary")

	var totes, declared int
	testutil.MustNoErr(t, db.QueryRow(`SELECT COALESCE(totes_on_cart, -1) FROM bins WHERE id=$1`, bin.ID).Scan(&totes), "read the count")
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM bin_uop_ledger WHERE bin_id=$1 AND op='totes_declared'
		AND source='engine/cms_transactions.go:ClearForReuseAndBookDeparture'`, bin.ID).Scan(&declared), "read the ledger")
	if totes != 16 || declared != 1 {
		t.Errorf("after the boundary clear: totes_on_cart %d (-1 unknown), totes_declared rows %d; want 16 and 1", totes, declared)
	}
	txns, err := db.ListAllCMSTransactions(50, 0)
	testutil.MustNoErr(t, err, "list cms transactions")
	if len(txns) != 1 || txns[0].SourceType != "clear" {
		t.Errorf("cms transactions = %d, want the one clear departure", len(txns))
	}
}
