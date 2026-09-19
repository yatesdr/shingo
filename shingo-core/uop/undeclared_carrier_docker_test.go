//go:build docker

package uop_test

import (
	"database/sql"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/payloads"
	"shingocore/uop"
)

// undeclared_carrier_docker_test.go — the watcher, and the produce-tick rebind.
//
// THE FLAG EXISTS BECAUSE THE PRODUCE DOOR STOPPED REFUSING. A carrier holding
// a payload its bin type is not declared to carry used to be refused at
// finalize; the parts were already in it, so the refusal recorded a lie. The
// write lands now and the bin carries bins.undeclared_carrier_at — which means
// the ONLY thing standing between "that bin can never be fetched" and nobody
// knowing is this count and the /material-flags list beside it. Law 8: a flag
// nobody sees is error 5.
//
// THE REBIND IS A PAYLOAD WRITE. uop's produce-tick identity binding moves a
// carrier onto the payload a press is physically filling it with, which can be
// a payload the carrier is not declared for. Without the recompute here, the
// bin would sit unsourceable and unflagged until its next finalize — possibly
// weeks. It rides the UPDATE that was already there: zero added statements on
// the delta path.

type undeclaredFixture struct {
	db        *store.DB
	fitsID    int64
	foreignID int64
	nodeID    int64
	declared  string // payload WITH payload_bin_types rows
	sparse    string // payload with NONE — the Springfield shape
}

func setupUndeclaredFixture(t *testing.T) undeclaredFixture {
	t.Helper()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)

	fits := &bins.BinType{Code: "UC-FITS"}
	testutil.MustNoErr(t, db.CreateBinType(fits), "create UC-FITS")
	foreign := &bins.BinType{Code: "UC-FOREIGN"}
	testutil.MustNoErr(t, db.CreateBinType(foreign), "create UC-FOREIGN")

	declared := &payloads.Payload{Code: "UC-DECLARED", UOPCapacity: 100}
	testutil.MustNoErr(t, payloads.Create(db.DB, declared), "create declared payload")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(declared.ID, []int64{fits.ID}), "declare bin types")

	// THE SPARSE PAYLOAD HAS NO ROWS AT ALL, which is 127 of Springfield's 128.
	sparse := &payloads.Payload{Code: "UC-SPARSE", UOPCapacity: 100}
	testutil.MustNoErr(t, payloads.Create(db.DB, sparse), "create sparse payload")

	return undeclaredFixture{
		db: db, fitsID: fits.ID, foreignID: foreign.ID, nodeID: sd.StorageNode.ID,
		declared: declared.Code, sparse: sparse.Code,
	}
}

// binOfType makes an EMPTY carrier of the given type at the storage node.
func (f undeclaredFixture) binOfType(t *testing.T, label string, binTypeID int64) int64 {
	t.Helper()
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, NodeID: &f.nodeID, Status: "available"}
	testutil.MustNoErr(t, f.db.CreateBin(b), "create bin "+label)
	return b.ID
}

func (f undeclaredFixture) flaggedAt(t *testing.T, binID int64) *time.Time {
	t.Helper()
	var at sql.NullTime
	testutil.MustNoErr(t, f.db.DB.QueryRow(
		`SELECT undeclared_carrier_at FROM bins WHERE id=$1`, binID).Scan(&at),
		"read undeclared_carrier_at")
	if !at.Valid {
		return nil
	}
	return &at.Time
}

// TestAnomalySummary_CountsCarriersTheRuleRefuses is the watcher on the
// inventory page.
//
// IT ALSO PINS THE SPARSE ARM QUIET. A carrier holding a payload with no
// declared bin types is not counted — at Springfield that is almost every
// carrier on the plant, and counting them would make this number useless on
// the exact plant it was built for.
func TestAnomalySummary_CountsCarriersTheRuleRefuses(t *testing.T) {
	t.Parallel()
	f := setupUndeclaredFixture(t)
	manifest := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(f.db, manifest, service.EpochAnnounce{})

	base, err := svc.AnomalySummary()
	testutil.MustNoErr(t, err, "baseline summary")
	if base.UndeclaredCarrierBins != 0 {
		t.Fatalf("baseline UndeclaredCarrierBins = %d, want 0", base.UndeclaredCarrierBins)
	}

	// One carrier the rule refuses, one it admits, one carrying a payload with
	// no rule at all. Only the first is a finding.
	wrong := f.binOfType(t, "UC-WRONG", f.foreignID)
	right := f.binOfType(t, "UC-RIGHT", f.fitsID)
	sparse := f.binOfType(t, "UC-SPARSE-BIN", f.foreignID)
	testutil.MustNoErr(t, manifest.RecordProducedBinFromTemplate(wrong, f.declared, nil, ""), "finalize wrong")
	testutil.MustNoErr(t, manifest.RecordProducedBinFromTemplate(right, f.declared, nil, ""), "finalize right")
	testutil.MustNoErr(t, manifest.RecordProducedBinFromTemplate(sparse, f.sparse, nil, ""), "finalize sparse")

	got, err := svc.AnomalySummary()
	testutil.MustNoErr(t, err, "summary")
	if got.UndeclaredCarrierBins != 1 {
		t.Errorf("UndeclaredCarrierBins = %d, want 1.\n"+
			"Exactly one carrier here holds a payload its type is not declared for. "+
			"A 3 means the sparse arm is flagging payloads nobody has described, which "+
			"is 127 of Springfield's 128 payloads and makes the number unreadable; a 0 "+
			"means the produce door landed a bin nothing will ever fetch and said nothing.",
			got.UndeclaredCarrierBins)
	}

	// Clearing the carrier clears the finding, and the count follows.
	_, cErr := manifest.ClearForReuse(wrong, nil, protocol.DeclaredByLifecycle)
	testutil.MustNoErr(t, cErr, "clear for reuse")
	after, err := svc.AnomalySummary()
	testutil.MustNoErr(t, err, "summary after clear")
	if after.UndeclaredCarrierBins != 0 {
		t.Errorf("UndeclaredCarrierBins = %d after the carrier was emptied, want 0 — "+
			"the count would only ever rise", after.UndeclaredCarrierBins)
	}
}

// TestProduceTickRebind_FlagsAnUndeclaredCarrier. The identity binding writes
// the payload a press is physically filling the carrier with, which is stronger
// evidence than any label typed at a load screen — and can be a payload the
// carrier is not declared for. The rebind is a payload write, so the finding is
// recomputed with it.
//
// COUNTING IS NOT AFFECTED, and must not be: the rebind exists because a
// produce count must never freeze on a label disagreement (HK 2026-07-16). The
// flag is a finding, not a gate.
func TestProduceTickRebind_FlagsAnUndeclaredCarrier(t *testing.T) {
	t.Parallel()
	f := setupUndeclaredFixture(t)
	manifest := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(f.db, manifest, service.EpochAnnounce{})

	// A carrier of the WRONG type, labelled with the sparse payload (so it
	// starts unflagged — no rule to break), then rebound by a produce tick onto
	// the declared payload, which its type is not among.
	binID := f.binOfType(t, "UC-REBIND", f.foreignID)
	testutil.MustNoErr(t, manifest.RecordProducedBinFromTemplate(binID, f.sparse, nil, ""), "seed load")
	if at := f.flaggedAt(t, binID); at != nil {
		t.Fatalf("seed carrier is already flagged at %v — the sparse payload has no rule", *at)
	}

	d := makeBinDelta(binID, f.declared, 5, 1, protocol.ReasonProduceTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply produce tick")

	got, err := f.db.GetBin(binID)
	testutil.MustNoErr(t, err, "re-read bin")
	if got.PayloadCode != f.declared {
		t.Fatalf("rebind did not take: payload=%q want %q", got.PayloadCode, f.declared)
	}
	if f.flaggedAt(t, binID) == nil {
		t.Error("a produce tick rebound this carrier onto a payload its bin type is not " +
			"declared to carry, and left it unflagged. Every sourcing reader refuses it " +
			"from this moment; without the flag nothing says so until the next finalize.")
	}
}

// TestProduceTickRebind_ClearsAStaleFinding is the other direction, and the
// half that is easy to leave out. A rebind onto a payload the carrier IS
// declared for makes the bin correct, so the finding has to go in the same
// statement — a flag that only ever sets is a list that only ever grows.
func TestProduceTickRebind_ClearsAStaleFinding(t *testing.T) {
	t.Parallel()
	f := setupUndeclaredFixture(t)
	manifest := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	svc := uop.NewInventoryDeltaService(f.db, manifest, service.EpochAnnounce{})

	// Start flagged: a wrong-type carrier loaded with the declared payload.
	// Then a produce tick rebinds it onto the sparse payload, which constrains
	// nothing — so the carrier is correct and the finding must go.
	binID := f.binOfType(t, "UC-REBIND-HEAL", f.foreignID)
	testutil.MustNoErr(t, manifest.RecordProducedBinFromTemplate(binID, f.declared, nil, ""), "seed load")
	if f.flaggedAt(t, binID) == nil {
		t.Fatal("seed load did not flag")
	}

	d := makeBinDelta(binID, f.sparse, 5, 1, protocol.ReasonProduceTick)
	testutil.MustNoErr(t, svc.ApplyBinUOPDelta(testStation, d), "apply produce tick")

	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("the finding survived a rebind onto a payload with no declared "+
			"carriers (flagged at %v). No declaration means no rule, so there is "+
			"nothing left to find.", *at)
	}
}
