//go:build docker

package bins_test

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/audit"
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

// ── CarrierBindings (5.11) ───────────────────────────────────────────────────
//
// The binding boundary is the one thing here that cannot be checked without a
// database: it is a correlated MAX over an op set, and getting the set wrong is
// silent and points the reassuring way. A boundary op MISSING joins two bindings
// and reports an age too long; a NON-boundary op included cuts one binding in
// half and hides a real stale one.

func TestCarrierBindings_BoundAtIsTheNewestEpochBumpOnly(t *testing.T) {
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	// TRUNCATED TO THE MICROSECOND, which is Postgres timestamptz's resolution.
	// Without it the round-trip drops the sub-microsecond nanoseconds Go's clock
	// supplies and time.Equal fails by ~700ns — a comparison artefact that reads
	// as a wrong boundary op and sends the next reader after the wrong bug.
	base := time.Now().UTC().Add(-30 * 24 * time.Hour).Truncate(time.Microsecond)

	b := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "BIND-1", PayloadCode: "PART-A"}
	testutil.MustNoErr(t, bins.Create(db.DB, b), "create bin")
	_, err := db.DB.Exec(`UPDATE bins SET uop_remaining=-40, payload_code='PART-A' WHERE id=$1`, b.ID)
	testutil.MustNoErr(t, err, "set ledger")

	// THE SPRINGFIELD BIN-27 SHAPE, which is why this test exists. A binding
	// opened by set_for_production, a CYCLE COUNT in the middle of it, and no
	// boundary since. The binding is 30 days old and must report as 30 days —
	// RecordCount does not bump delta_epoch, so a recount corrects the count
	// INSIDE the binding. Treating it as a boundary would report 20 days here
	// and would have hidden the longest binding in the dump.
	seedAudit(t, db, b.ID, 0, 2000, "set_for_production", "PART-A", base)
	seedAudit(t, db, b.ID, 2000, 2000, "manifest_confirmed", "PART-A", base.Add(time.Minute))
	seedAudit(t, db, b.ID, -700, 4500, "cycle_count", "PART-A", base.Add(10*24*time.Hour))
	seedAudit(t, db, b.ID, 40, -40, "bin_uop_delta", "PART-A", base.Add(20*24*time.Hour))
	// Two applier observation rows. Neither bumps the epoch (uop/applier.go says
	// so in as many words), so neither may move bound_at.
	seedAudit(t, db, b.ID, -40, -40, "stale_epoch_dropped", "PART-A", base.Add(25*24*time.Hour))
	seedAudit(t, db, b.ID, -40, -40, "payload_mismatch_dropped", "PART-A", base.Add(26*24*time.Hour))

	got, err := bins.CarrierBindings(db.DB, audit.EpochBumpOps)
	testutil.MustNoErr(t, err, "CarrierBindings")

	var row *bins.CarrierBinding
	for i := range got {
		if got[i].BinID == b.ID {
			row = &got[i]
		}
	}
	if row == nil {
		t.Fatalf("bin %d missing from CarrierBindings: %+v", b.ID, got)
	}
	if row.BoundAt == nil {
		t.Fatal("bound_at is nil, but a set_for_production row exists")
	}
	if !row.BoundAt.Equal(base) {
		t.Errorf("bound_at = %s, want %s (the set_for_production). A cycle count, a "+
			"manifest confirm, a delta or an applier drop row must NOT move it — none of "+
			"them bumps delta_epoch, and treating one as a boundary cuts the binding short "+
			"and hides a genuinely stale one.", row.BoundAt, base)
	}
	if row.UOPRemaining != -40 {
		t.Errorf("uop_remaining = %d, want -40 (never clamped)", row.UOPRemaining)
	}
	if row.UOPCapacity == nil || *row.UOPCapacity != 1000 {
		t.Errorf("uop_capacity = %v, want 1000 — without it a negative cannot be sized", row.UOPCapacity)
	}

	// And a release AFTER all of it does start a new binding.
	later := base.Add(28 * 24 * time.Hour)
	seedAudit(t, db, b.ID, -40, 0, "released_underpack", "PART-A", later)
	got2, err := bins.CarrierBindings(db.DB, audit.EpochBumpOps)
	testutil.MustNoErr(t, err, "CarrierBindings after release")
	for _, r := range got2 {
		if r.BinID == b.ID && (r.BoundAt == nil || !r.BoundAt.Equal(later)) {
			t.Errorf("after a release, bound_at = %v, want %s", r.BoundAt, later)
		}
	}
}

// A carrier with no boundary row at all reports NIL, not the zero time and not
// "just bound". The absence has to survive the query, or the page can never say
// it had it.
func TestCarrierBindings_PreservesEveryAbsence(t *testing.T) {
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)

	noHistory := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "NO-HISTORY", PayloadCode: "PART-A"}
	testutil.MustNoErr(t, bins.Create(db.DB, noHistory), "create bin")
	_, err := db.DB.Exec(`UPDATE bins SET payload_code='PART-A', uop_remaining=5 WHERE id=$1`, noHistory.ID)
	testutil.MustNoErr(t, err, "bind it")

	unbound := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "UNBOUND"}
	testutil.MustNoErr(t, bins.Create(db.DB, unbound), "create unbound bin")

	// A bound payload that is NOT in the payloads table at all: capacity has to
	// come back nil rather than zero, so a negative on it renders as "cannot
	// size" instead of as zero binloads.
	unknownPayload := &bins.Bin{BinTypeID: sd.BinType.ID, Label: "UNKNOWN-PAYLOAD"}
	testutil.MustNoErr(t, bins.Create(db.DB, unknownPayload), "create bin")
	_, err = db.DB.Exec(`UPDATE bins SET payload_code='PART-GHOST', uop_remaining=-9 WHERE id=$1`, unknownPayload.ID)
	testutil.MustNoErr(t, err, "bind a payload with no catalog row")

	got, err := bins.CarrierBindings(db.DB, audit.EpochBumpOps)
	testutil.MustNoErr(t, err, "CarrierBindings")

	by := map[int64]bins.CarrierBinding{}
	for _, r := range got {
		by[r.BinID] = r
	}

	if r := by[noHistory.ID]; r.BoundAt != nil {
		t.Errorf("a carrier with no boundary row reported bound_at = %s. NIL is the only "+
			"honest answer — a zero time would render as an age of decades and the epoch "+
			"as an age of zero, and both are inventions", r.BoundAt)
	}
	if r := by[unbound.ID]; r.PayloadCode != "" {
		t.Errorf("unbound carrier reported payload %q", r.PayloadCode)
	}
	if r := by[unbound.ID]; r.UOPCapacity != nil {
		t.Errorf("unbound carrier reported a capacity (%v); there is no payload to have one", r.UOPCapacity)
	}
	if r := by[unknownPayload.ID]; r.UOPCapacity != nil {
		t.Errorf("a payload with no catalog row reported capacity %v, want nil. Zero would "+
			"be divided by, and the quotient would render as a measurement", r.UOPCapacity)
	}
	if r := by[unknownPayload.ID]; r.UOPRemaining != -9 {
		t.Errorf("uop_remaining = %d, want -9", r.UOPRemaining)
	}
}
