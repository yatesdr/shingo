//go:build docker

package bins_test

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// The ledger is allowed to go negative and the design says keep it that way:
// negative is LOUDLY wrong, a clamp would be SILENTLY wrong. These pin the
// reads that find the wrongness, not a guard that hides it.

// seedAudit writes one bin_uop_audit row directly — the applier's shape.
func seedAudit(t *testing.T, db *store.DB, binID int64, before, after int, op, payload string, at time.Time) {
	t.Helper()
	_, err := db.Exec(`INSERT INTO bin_uop_audit
		(bin_id, before_uop, after_uop, op, source, payload_code, actor, metadata, applied_at)
		VALUES ($1,$2,$3,$4,'test',$5,'test','{"reason":"consume_tick"}',$6)`,
		binID, before, after, op, payload, at)
	testutil.MustNoErr(t, err, "seed audit row")
}

func TestNegativeExcursions_FindsCrossingNotContinuation(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-time.Hour)

	// 40 → 10 (fine), 10 → -5 (CROSSING), -5 → -20 (continuation, deeper),
	// -20 → 30 (recovery).
	seedAudit(t, db, 1, 40, 10, "bin_uop_delta", "PART-A", base)
	seedAudit(t, db, 1, 10, -5, "bin_uop_delta", "PART-A", base.Add(time.Minute))
	seedAudit(t, db, 1, -5, -20, "bin_uop_delta", "PART-A", base.Add(2*time.Minute))
	seedAudit(t, db, 1, -20, 30, "manifest_set", "PART-A", base.Add(3*time.Minute))

	got, err := bins.NegativeExcursions(db.DB, base.Add(-time.Hour), 5*time.Minute, 50)
	testutil.MustNoErr(t, err, "NegativeExcursions")

	if len(got) != 1 {
		t.Fatalf("one crossing expected (the continuation is the same excursion), got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.BeforeUOP != 10 || e.AfterUOP != -5 {
		t.Errorf("crossing brackets wrong: before=%d after=%d", e.BeforeUOP, e.AfterUOP)
	}
	// Deepest folds in the continuation — the excursion reached -20, not -5.
	if e.Deepest != -20 {
		t.Errorf("Deepest = %d, want -20 (the floor before recovery)", e.Deepest)
	}
	if e.RecoveredAt == nil {
		t.Fatal("this excursion recovered; RecoveredAt must be set")
	}
	if d := e.Duration(time.Now()); d != 2*time.Minute {
		t.Errorf("Duration = %s, want 2m (crossing to recovery)", d)
	}
}

// An excursion that never came back has no RecoveredAt — that is what puts it
// on the exception list rather than in the history.
func TestNegativeExcursions_OpenExcursionHasNoRecovery(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-time.Hour)

	seedAudit(t, db, 2, 5, -3, "bin_uop_delta", "PART-B", base)
	seedAudit(t, db, 2, -3, -9, "bin_uop_delta", "PART-B", base.Add(time.Minute))

	got, err := bins.NegativeExcursions(db.DB, base.Add(-time.Hour), 5*time.Minute, 50)
	testutil.MustNoErr(t, err, "NegativeExcursions")
	if len(got) != 1 {
		t.Fatalf("want 1 excursion, got %d", len(got))
	}
	if got[0].RecoveredAt != nil {
		t.Errorf("still negative — RecoveredAt must be nil, got %v", got[0].RecoveredAt)
	}
	if got[0].Deepest != -9 {
		t.Errorf("Deepest = %d, want -9", got[0].Deepest)
	}
}

// The hypothesis flag: a release (positive → exactly 0) shortly before the
// crossing is what would make the drops and the negatives two faces of one
// race. The flag must be honest in BOTH directions — a false negative here
// would send someone hunting for a race that is not there.
func TestNegativeExcursions_PrecededByRelease(t *testing.T) {
	db := testdb.Open(t)
	base := time.Now().UTC().Add(-time.Hour)

	// Bin 3: released (60 → 0), then a late consume delta debits from zero.
	seedAudit(t, db, 3, 60, 0, "manifest_clear", "PART-C", base)
	seedAudit(t, db, 3, 0, -12, "bin_uop_delta", "PART-C", base.Add(30*time.Second))

	// Bin 4: no release anywhere near it — just drifted under.
	seedAudit(t, db, 4, 3, -2, "bin_uop_delta", "PART-D", base.Add(time.Minute))

	got, err := bins.NegativeExcursions(db.DB, base.Add(-time.Hour), 5*time.Minute, 50)
	testutil.MustNoErr(t, err, "NegativeExcursions")

	byBin := map[int64]bins.NegativeExcursion{}
	for _, e := range got {
		byBin[e.BinID] = e
	}
	if !byBin[3].PrecededByRelease {
		t.Error("bin 3 crossed 30s after a release — the flag must be set")
	}
	if byBin[4].PrecededByRelease {
		t.Error("bin 4 had no release; a false positive here invents a race")
	}
}

func TestOpenNegativeBins_AndNegativePayloads(t *testing.T) {
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	// Two bins on one payload: one negative, one positive but not enough to
	// cover it — so the PAYLOAD total is negative too.
	neg := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "NEG-1", PayloadCode: "PART-X"}
	testutil.MustNoErr(t, bins.Create(db.DB, neg), "create neg bin")
	pos := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "POS-1", PayloadCode: "PART-X"}
	testutil.MustNoErr(t, bins.Create(db.DB, pos), "create pos bin")

	// payload_code is written by the manifest path, not by Create — set both
	// columns directly here, since the point of the fixture is the ledger
	// value, not how it got there.
	_, err := db.DB.Exec(`UPDATE bins SET uop_remaining = -40, payload_code='PART-X' WHERE id=$1`, neg.ID)
	testutil.MustNoErr(t, err, "drive negative")
	_, err = db.DB.Exec(`UPDATE bins SET uop_remaining = 10, payload_code='PART-X' WHERE id=$1`, pos.ID)
	testutil.MustNoErr(t, err, "set positive")

	open, err := bins.OpenNegativeBins(db.DB)
	testutil.MustNoErr(t, err, "OpenNegativeBins")
	if len(open) != 1 || open[0].BinID != neg.ID || open[0].UOPRemaining != -40 {
		t.Fatalf("want exactly the negative bin, got %+v", open)
	}

	payloads, err := bins.NegativePayloads(db.DB)
	testutil.MustNoErr(t, err, "NegativePayloads")
	if got, ok := payloads["PART-X"]; !ok || got != -30 {
		t.Fatalf("PART-X total = %d (present=%v), want -30 — the SUM, not the worst bin", got, ok)
	}
}

// Blank on a good day. If this ever returns rows for a healthy plant the
// exception list stops being an exception list.
func TestOpenNegativeBins_EmptyWhenHealthy(t *testing.T) {
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	b := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "HEALTHY", PayloadCode: "PART-OK"}
	testutil.MustNoErr(t, bins.Create(db.DB, b), "create bin")
	_, err := db.DB.Exec(`UPDATE bins SET uop_remaining = 25, payload_code='PART-OK' WHERE id=$1`, b.ID)
	testutil.MustNoErr(t, err, "stock it")

	open, err := bins.OpenNegativeBins(db.DB)
	testutil.MustNoErr(t, err, "OpenNegativeBins")
	if len(open) != 0 {
		t.Fatalf("healthy plant must produce an empty list, got %+v", open)
	}
	payloads, err := bins.NegativePayloads(db.DB)
	testutil.MustNoErr(t, err, "NegativePayloads")
	if len(payloads) != 0 {
		t.Fatalf("healthy plant must produce no negative payloads, got %+v", payloads)
	}
}
