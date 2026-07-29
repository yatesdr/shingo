//go:build docker

package store_test

// Tests for the delta-drop panel's read. They live beside the query at the
// outer store/ level because that is where it had to go: it spans the audit
// and bins aggregates, which store/bins is not allowed to do.

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// seedDrop writes one observation row of the applier's drop shape: before ==
// after (the count did NOT move) with the dropped quantity in metadata.
func seedDrop(t *testing.T, db *store.DB, binID, at60 int, op, payload string, delta, standing int, base time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$2,$3,'test',$4,'test',$5::jsonb,$6)`,
		binID, standing, op, payload,
		fmt.Sprintf(`{"sequence_id":1,"delta":%d}`, delta),
		base.Add(time.Duration(at60)*time.Minute))
	testutil.MustNoErr(t, err, "seed drop row")
}

// seedBinWithUOP creates a bin holding uop of payload, so the panel has a
// ledger total to set the drops against.
func seedBinWithUOP(t *testing.T, db *store.DB, label, payload string, uop int) int64 {
	t.Helper()
	bt, err := db.GetBinTypeByCode("DEFAULT")
	if err != nil {
		bt = &bins.BinType{Code: "DEFAULT", Description: "test"}
		testutil.MustNoErr(t, db.CreateBinType(bt), "create bin type")
	}
	b := &bins.Bin{BinTypeID: bt.ID, Label: label, Status: "available"}
	testutil.MustNoErr(t, db.CreateBin(b), "create bin")
	_, err = db.Exec(`UPDATE bins SET payload_code=$1, uop_remaining=$2 WHERE id=$3`, payload, uop, b.ID)
	testutil.MustNoErr(t, err, "set bin payload/uop")
	return b.ID
}

// THE F1 RULE, and it is the whole reason this panel is worth building:
// payload_rebound_with_inventory IS NOT A DROP.
//
// The applier rebinds the bin's payload and then APPLIES the delta — its own
// comment says "counting CONTINUES (the tote's unit total stays correct), and
// the bin is anomaly-flagged for a later cycle count of the mixed contents".
// It sits in the discrepancy ledger because it is worth seeing, not because
// units went missing.
//
// Summing it into UOP lost would inflate the figure and corrupt the exact
// comparison the panel exists to make: a payload reading -443 beside ~443 UOP
// of genuinely dropped count. So it contributes to mixed_contents and ZERO to
// uop_lost, and it carries no UOP total of its own anywhere.
func TestDeltaIntegrity_ReboundIsNotALoss(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-2 * time.Hour)

	binID := seedBinWithUOP(t, db, "DI-REBOUND", "PART-REB", 120)
	// One genuine drop: a credit of 50 that never landed.
	seedDrop(t, db, int(binID), 1, "stale_epoch_dropped", "PART-REB", 50, 120, base)
	// Three rebinds, carrying deltas that WERE applied. A naive sum would add
	// 900 to the loss figure.
	seedDrop(t, db, int(binID), 2, "payload_rebound_with_inventory", "PART-REB", 300, 120, base)
	seedDrop(t, db, int(binID), 3, "payload_rebound_with_inventory", "PART-REB", 300, 120, base)
	seedDrop(t, db, int(binID), 4, "payload_rebound_with_inventory", "PART-REB", 300, 120, base)

	got, err := db.DeltaIntegrityByPayload(base.Add(-time.Hour))
	testutil.MustNoErr(t, err, "DeltaIntegrityByPayload")

	var row *domain.DeltaIntegrity
	for i := range got {
		if got[i].PayloadCode == "PART-REB" {
			row = &got[i]
		}
	}
	if row == nil {
		t.Fatalf("no row for PART-REB in %+v", got)
	}
	if row.UOPLost != 50 {
		t.Errorf("uop_lost = %d, want 50 — the three rebinds must contribute ZERO; "+
			"they rebind and APPLY the delta, and summing them corrupts the "+
			"drop-total-vs-ledger-total comparison this panel is for", row.UOPLost)
	}
	if row.CreditsDropped != 50 {
		t.Errorf("credits_dropped = %d, want 50", row.CreditsDropped)
	}
	if row.DropRows != 1 {
		t.Errorf("drop_rows = %d, want 1 — a rebind is not a drop", row.DropRows)
	}
	if row.MixedContents != 3 {
		t.Errorf("mixed_contents = %d, want 3", row.MixedContents)
	}
}

// The comparison IS the panel. A payload whose ledger reads -443 and whose
// dropped CREDITS over the same window total 443 has its cause on the screen,
// and the sign convention is what makes the two numbers comparable at a
// glance: uop_lost is a NET signed toward the ledger, so positive means "the
// count reads this much BELOW reality".
func TestDeltaIntegrity_NetIsSignedTowardTheLedger(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-2 * time.Hour)

	seedBinWithUOP(t, db, "DI-SPR", "74577-6SA0A.06", -443)
	binID := seedBinWithUOP(t, db, "DI-SPR-2", "74577-6SA0A.06", 0)

	// Dropped credits: count never went up by these.
	seedDrop(t, db, int(binID), 1, "stale_epoch_dropped", "74577-6SA0A.06", 300, 0, base)
	seedDrop(t, db, int(binID), 2, "payload_mismatch_dropped", "74577-6SA0A.06", 200, 0, base)
	// A dropped consume pushes the other way: the count reads too HIGH by 57.
	seedDrop(t, db, int(binID), 3, "stale_epoch_dropped", "74577-6SA0A.06", -57, 0, base)

	got, err := db.DeltaIntegrityByPayload(base.Add(-time.Hour))
	testutil.MustNoErr(t, err, "DeltaIntegrityByPayload")

	var row *domain.DeltaIntegrity
	for i := range got {
		if got[i].PayloadCode == "74577-6SA0A.06" {
			row = &got[i]
		}
	}
	if row == nil {
		t.Fatalf("no row for the Springfield payload in %+v", got)
	}
	if row.CreditsDropped != 500 || row.ConsumesDropped != 57 {
		t.Errorf("directions wrong: credits=%d consumes=%d, want 500/57",
			row.CreditsDropped, row.ConsumesDropped)
	}
	if row.UOPLost != 443 {
		t.Errorf("uop_lost = %d, want 443 (500 credits - 57 consumes)", row.UOPLost)
	}
	// And the number it must be read against travels with it.
	if row.LedgerTotal != -443 {
		t.Errorf("ledger_total = %d, want -443 — without it the drop total is unanchored", row.LedgerTotal)
	}
	if row.StaleEpochRows != 2 || row.PayloadMismatchRows != 1 {
		t.Errorf("cause split wrong: stale=%d mismatch=%d, want 2/1 — two different bugs",
			row.StaleEpochRows, row.PayloadMismatchRows)
	}
}

// Blank on a good day, like its neighbour. A payload with no drops in the
// window produces no row at all.
func TestDeltaIntegrity_SilentWhenNothingDropped(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-2 * time.Hour)

	binID := seedBinWithUOP(t, db, "DI-QUIET", "PART-QUIET", 400)
	// Ordinary applied deltas, not drops.
	seedAudit(t, db, binID, 400, 380, "bin_uop_delta", "PART-QUIET", base)
	seedAudit(t, db, binID, 380, 360, "bin_uop_delta", "PART-QUIET", base.Add(time.Minute))

	got, err := db.DeltaIntegrityByPayload(base.Add(-time.Hour))
	testutil.MustNoErr(t, err, "DeltaIntegrityByPayload")
	for _, r := range got {
		if r.PayloadCode == "PART-QUIET" {
			t.Fatalf("a payload with no drops must produce no row, got %+v", r)
		}
	}
}

// A drop outside the window is not this window's problem.
func TestDeltaIntegrity_RespectsTheWindow(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-48 * time.Hour)

	binID := seedBinWithUOP(t, db, "DI-OLD", "PART-OLD", 100)
	seedDrop(t, db, int(binID), 0, "stale_epoch_dropped", "PART-OLD", 900, 100, base)

	got, err := db.DeltaIntegrityByPayload(time.Now().UTC().Add(-time.Hour))
	testutil.MustNoErr(t, err, "DeltaIntegrityByPayload")
	for _, r := range got {
		if r.PayloadCode == "PART-OLD" {
			t.Fatalf("a drop 48h ago must not appear in a 1h window, got %+v", r)
		}
	}
}

// seedAudit writes one ordinary bin_uop_audit row — a delta that APPLIED, so
// before != after. The quiet case needs these to prove the panel stays silent
// on activity that is not a drop.
func seedAudit(t *testing.T, db *store.DB, binID int64, before, after int, op, payload string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test',$5,'test','{"reason":"consume_tick"}',$6)`,
		binID, before, after, op, payload, at)
	testutil.MustNoErr(t, err, "seed audit row")
}
