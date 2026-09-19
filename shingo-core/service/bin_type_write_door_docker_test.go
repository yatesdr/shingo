//go:build docker

package service_test

import (
	"database/sql"
	"strings"
	"testing"
	"time"

	"shingo/protocol"

	"shingocore/internal/testdb"
	"shingocore/service"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/payloads"
)

// bin_type_write_door_docker_test.go — the carrier rule at the WRITE end.
//
// THE HOLE THIS PINS. payload_bin_types was enforced at exactly one of the two
// doors that write a payload onto a bin. The operator's Load Payload
// (BinService.LoadPayload) checked; produce finalize (RecordProducedBin, and
// RecordProducedBinFromTemplate through it) did not, so a cell finalizing a bin
// of the wrong carrier for its part wrote the row unopposed.
//
// WHAT THAT ROW IS. Since 493a8062 every sourcing reader composes
// BinSourceableSQL, whose PayloadBinTypeRuleArm refuses exactly this bin. So
// the unchecked write produced stock that COUNTS and can never be FETCHED —
// present in inventory, invisible to every finder, and indistinguishable at the
// HMI from a part nobody has produced yet.
//
// ── THE TWO DOORS ANSWER ONE RULE AND DO DIFFERENT THINGS WITH A "NO" ───────
//
// Both doors reach one implementation (service.judgeBinTypeCarriesPayload,
// under setForProductionTx), and this file asserts both, because a guard only
// one caller is tested against is how the first divergence started. What they
// do with a refusal differs on purpose (owner, 2026-09-20):
//
//   - THE OPERATOR DOOR REFUSES. A person is typing an assignment and nothing
//     physical has happened; refusing costs nothing and prevents the row.
//   - THE PRODUCE DOOR ACCEPTS AND FLAGS. The parts are in the carrier by the
//     time the cell reports it. A refusal there writes down that something
//     which happened did not, and the count is then wrong in the direction
//     nothing downstream can detect.
//
// So the wrong-carrier produce cell in this file MOVED: it landed before B5,
// was refused by B5, and lands FLAGGED now. The flag is the whole point — the
// bin is still unsourceable, so an unflagged landing would be a silent hole
// where the refusal was at least a loud one.
//
// ── THE SPARSE-TABLE ARM IS PINNED QUIET AT EVERY DOOR ──────────────────────
//
// payload_bin_types is sparsely populated (one payload of 128 has rows at
// Springfield). A payload with NO rows constrains nothing, and must therefore
// produce no refusal AND no flag. Every assertion below about the undescribed
// payload is asserting silence, not permission.

// bintypeFixture is the plant these cases run against: one payload declared
// carryable by exactly one of two bin types.
type bintypeFixture struct {
	db        *store.DB
	fitsID    int64
	foreignID int64
	payload   string
	payloadID int64
}

func setupBinTypeFixture(t *testing.T) bintypeFixture {
	t.Helper()
	db := testdb.Open(t)
	testdb.SetupStandardData(t, db)

	fits := &bins.BinType{Code: "BT-FITS", Description: "declared for the payload"}
	if err := db.CreateBinType(fits); err != nil {
		t.Fatalf("create BT-FITS: %v", err)
	}
	foreign := &bins.BinType{Code: "BT-FOREIGN", Description: "not declared for it"}
	if err := db.CreateBinType(foreign); err != nil {
		t.Fatalf("create BT-FOREIGN: %v", err)
	}

	p := &payloads.Payload{Code: "BTW-PART", UOPCapacity: 100}
	if err := payloads.Create(db.DB, p); err != nil {
		t.Fatalf("create payload: %v", err)
	}
	if err := db.SetPayloadBinTypes(p.ID, []int64{fits.ID}); err != nil {
		t.Fatalf("declare payload bin types: %v", err)
	}
	return bintypeFixture{db: db, fitsID: fits.ID, foreignID: foreign.ID,
		payload: p.Code, payloadID: p.ID}
}

func (f bintypeFixture) bin(t *testing.T, label string, binTypeID int64) int64 {
	t.Helper()
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, Status: "available"}
	if err := f.db.CreateBin(b); err != nil {
		t.Fatalf("create bin %s: %v", label, err)
	}
	return b.ID
}

// flaggedAt reads bins.undeclared_carrier_at — the finding the produce door
// records instead of refusing.
//
// nil means the carrier is clean, and that is a MEASURED result: the column is
// recomputed at every payload write on the bin and at every edit to the
// payload's rule, so nil never means "not checked".
func (f bintypeFixture) flaggedAt(t *testing.T, binID int64) *time.Time {
	t.Helper()
	var at sql.NullTime
	if err := f.db.DB.QueryRow(
		`SELECT undeclared_carrier_at FROM bins WHERE id=$1`, binID).Scan(&at); err != nil {
		t.Fatalf("read undeclared_carrier_at for bin %d: %v", binID, err)
	}
	if !at.Valid {
		return nil
	}
	return &at.Time
}

// TestProduceFinalize_LandsAndFlagsACarrierThePartMayNotTravelIn is the cell
// B5a moved.
//
// THE REFUSAL WAS THE LIE. The cell has already filled the carrier by the time
// this message arrives; refusing does not un-fill it, it only makes ShinGo's
// record disagree with the floor — and it disagrees by UNDER-counting, which is
// the direction nothing downstream can detect. The write lands, the bin carries
// the finding, and every sourcing reader still refuses the bin.
func TestProduceFinalize_LandsAndFlagsACarrierThePartMayNotTravelIn(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-WRONG", f.foreignID)

	if err := svc.RecordProducedBinFromTemplate(binID, f.payload, nil, ""); err != nil {
		t.Fatalf("produce finalize refused a carrier the payload does not declare: %v\n"+
			"The parts are already in the bin by the time a cell reports a finalize. "+
			"Refusing records that something which happened did not, and leaves the "+
			"count low with nothing saying so. Accept and flag.", err)
	}

	b, gErr := f.db.GetBin(binID)
	if gErr != nil {
		t.Fatalf("re-read bin: %v", gErr)
	}
	if b.PayloadCode != f.payload || !b.ManifestConfirmed {
		t.Errorf("finalize did not land: payload=%q confirmed=%v", b.PayloadCode, b.ManifestConfirmed)
	}
	if f.flaggedAt(t, binID) == nil {
		t.Error("the bin landed UNFLAGGED. It counts as stock and no sourcing reader " +
			"will ever fetch it, so an unflagged landing is strictly worse than the " +
			"refusal it replaced — a silent hole where there used to be a loud one.")
	}
}

// TestProduceFinalize_TheFlagKeepsItsFirstStamp. A carrier relabelled from one
// undeclared payload to another has been wrong CONTINUOUSLY, and the timestamp
// answers "how long", not "when did anyone last notice". Same shape as
// bins.MarkAnomalyWithNote's COALESCE, and it is the number /material-flags
// sorts its rows by.
func TestProduceFinalize_TheFlagKeepsItsFirstStamp(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-RESTAMP", f.foreignID)

	if err := svc.RecordProducedBinFromTemplate(binID, f.payload, nil, ""); err != nil {
		t.Fatalf("first finalize: %v", err)
	}
	first := f.flaggedAt(t, binID)
	if first == nil {
		t.Fatal("first finalize did not flag")
	}

	other := &payloads.Payload{Code: "BTW-PART-2", UOPCapacity: 100}
	if err := payloads.Create(f.db.DB, other); err != nil {
		t.Fatalf("create second payload: %v", err)
	}
	if err := f.db.SetPayloadBinTypes(other.ID, []int64{f.fitsID}); err != nil {
		t.Fatalf("declare second payload bin types: %v", err)
	}
	if err := svc.RecordProducedBinFromTemplate(binID, other.Code, nil, ""); err != nil {
		t.Fatalf("second finalize: %v", err)
	}
	second := f.flaggedAt(t, binID)
	if second == nil {
		t.Fatal("the second finalize cleared the flag — the carrier is still wrong")
	}
	if !second.Equal(*first) {
		t.Errorf("the flag was re-dated: %v -> %v. The carrier has been in the wrong "+
			"bin type continuously; re-stamping resets the age of a finding nobody "+
			"has acted on.", *first, *second)
	}
}

// TestProduceFinalize_AcceptsADeclaredCarrier is the other half: the rule must
// not refuse the carrier the payload actually declares — and must not flag it.
func TestProduceFinalize_AcceptsADeclaredCarrier(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-RIGHT", f.fitsID)

	if err := svc.RecordProducedBinFromTemplate(binID, f.payload, nil, ""); err != nil {
		t.Fatalf("produce finalize refused the carrier the payload declares: %v", err)
	}
	b, err := f.db.GetBin(binID)
	if err != nil {
		t.Fatalf("re-read bin: %v", err)
	}
	if b.PayloadCode != f.payload || !b.ManifestConfirmed {
		t.Errorf("finalize did not land: payload=%q confirmed=%v", b.PayloadCode, b.ManifestConfirmed)
	}
	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("a DECLARED carrier was flagged at %v. Every correct load would "+
			"appear on /material-flags and the list would mean nothing.", *at)
	}
}

// TestProduceFinalize_UndescribedPayloadConstrainsNothing is the arm that must
// never tighten. payload_bin_types is sparsely populated; a payload with no
// rows constrains nothing, and a pre-2026-04-27 hard INNER JOIN starved orders
// for exactly the payloads nobody had gotten around to describing.
func TestProduceFinalize_UndescribedPayloadConstrainsNothing(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)

	undescribed := &payloads.Payload{Code: "BTW-UNDESCRIBED", UOPCapacity: 100}
	if err := payloads.Create(f.db.DB, undescribed); err != nil {
		t.Fatalf("create undescribed payload: %v", err)
	}

	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-ANY", f.foreignID)
	if err := svc.RecordProducedBinFromTemplate(binID, undescribed.Code, nil, ""); err != nil {
		t.Fatalf("a payload with no payload_bin_types rows refused a carrier: %v\n"+
			"Coverage is a gap in the data, not a licence to refuse.", err)
	}
	// THE SPARSE ARM, PINNED QUIET AT THE PRODUCE DOOR. Springfield has rows for
	// one payload of 128. Flagging the other 127 would put most of the plant on
	// an operator's list and bury the one case worth walking to.
	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("a payload with NO payload_bin_types rows flagged its carrier at %v.\n"+
			"No declaration means no rule, not a rule nobody satisfies.", *at)
	}
}

// TestOperatorLoadPayload_StillRefuses proves the door whose own copy of the
// check was deleted in 523c6a3b is still held to the rule — and that B5a's
// accept-and-flag did NOT reach it. Nothing physical has happened at this door.
func TestOperatorLoadPayload_StillRefuses(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinService(f.db, service.NewBinManifestService(f.db, service.EpochAnnounce{}))
	binID := f.bin(t, "BTW-OP-WRONG", f.foreignID)

	err := svc.LoadPayload(binID, f.payload, nil, protocol.DeclaredByPerson)
	if err == nil {
		t.Fatal("operator Load Payload accepted a carrier the payload excludes; " +
			"the produce door's accept-and-flag is scoped to the produce door, because " +
			"at this one nothing has been put in the carrier yet")
	}
	if !strings.Contains(err.Error(), "BT-FOREIGN") || !strings.Contains(err.Error(), "BT-FITS") {
		t.Errorf("refusal lost its detail — it named neither the carrier nor what is "+
			"allowed: %v", err)
	}

	// A REFUSED DOOR LEARNED NOTHING ABOUT THE PLANT, so it records nothing. No
	// parts moved; a finding is a claim about the floor, not about a keystroke.
	b, gErr := f.db.GetBin(binID)
	if gErr != nil {
		t.Fatalf("re-read bin: %v", gErr)
	}
	if b.PayloadCode != "" {
		t.Errorf("bin carries payload %q after a refused operator load — the judgement "+
			"runs inside setForProductionTx, so the whole transaction must roll back",
			b.PayloadCode)
	}
	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("a REFUSED operator load flagged the carrier at %v", *at)
	}
}

// TestOperatorLoadPayload_ADeclaredCarrierClearsAStandingFlag. The finding is
// derived from the payload on the bin, so the write that makes a carrier
// correct must clear it in the same statement — otherwise a fixed carrier stays
// on the list, and a list with nothing to act on is a list people stop reading.
func TestOperatorLoadPayload_ADeclaredCarrierClearsAStandingFlag(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	manifest := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-HEALED", f.foreignID)

	if err := manifest.RecordProducedBinFromTemplate(binID, f.payload, nil, ""); err != nil {
		t.Fatalf("seed finalize: %v", err)
	}
	if f.flaggedAt(t, binID) == nil {
		t.Fatal("seed finalize did not flag")
	}

	// The carrier is re-dunnaged to a declared type, then loaded at the HMI.
	if _, err := f.db.DB.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, f.fitsID, binID); err != nil {
		t.Fatalf("re-dunnage bin: %v", err)
	}
	binSvc := service.NewBinService(f.db, manifest)
	if err := binSvc.LoadPayload(binID, f.payload, nil, protocol.DeclaredByPerson); err != nil {
		t.Fatalf("operator load of a declared carrier was refused: %v", err)
	}
	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("the finding survived the write that fixed it (flagged at %v). It is "+
			"derived from the payload on the bin, so it has to clear in the same "+
			"transaction that makes the bin pass.", *at)
	}
}

// TestClearForReuse_ClearsTheFinding. An empty carrier holds nothing, so the
// finding — "the payload this carrier holds may not travel in it" — has no
// subject left. Without this the flag outlives its own statement and the count
// on /inventory drifts upward for ever.
func TestClearForReuse_ClearsTheFinding(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})
	binID := f.bin(t, "BTW-CLEARED", f.foreignID)

	if err := svc.RecordProducedBinFromTemplate(binID, f.payload, nil, ""); err != nil {
		t.Fatalf("seed finalize: %v", err)
	}
	if f.flaggedAt(t, binID) == nil {
		t.Fatal("seed finalize did not flag")
	}
	if _, err := svc.ClearForReuse(binID, nil, protocol.DeclaredByLifecycle); err != nil {
		t.Fatalf("clear for reuse: %v", err)
	}
	if at := f.flaggedAt(t, binID); at != nil {
		t.Errorf("an EMPTY carrier is still flagged (at %v). There is no payload left "+
			"for the finding to be about.", *at)
	}
}

// TestPayloadBinTypes_EditingTheRuleRecomputesEveryCarrier is the other half of
// "clears itself": the finding is a statement about payload_bin_types, so
// editing that table is the moment it can stop being true — in both directions.
//
// BOTH DIRECTIONS IN ONE TEST ON PURPOSE. A recompute that only ever CLEARS is
// the easy half to write and the useless one: widening a rule with no
// narrowing means a carrier that quietly became unsourceable says nothing until
// its next finalize, which may be weeks away.
func TestPayloadBinTypes_EditingTheRuleRecomputesEveryCarrier(t *testing.T) {
	t.Parallel()
	f := setupBinTypeFixture(t)
	svc := service.NewBinManifestService(f.db, service.EpochAnnounce{})

	wrong := f.bin(t, "BTW-RULE-WRONG", f.foreignID)
	right := f.bin(t, "BTW-RULE-RIGHT", f.fitsID)
	for _, id := range []int64{wrong, right} {
		if err := svc.RecordProducedBinFromTemplate(id, f.payload, nil, ""); err != nil {
			t.Fatalf("seed finalize bin %d: %v", id, err)
		}
	}
	if f.flaggedAt(t, wrong) == nil || f.flaggedAt(t, right) != nil {
		t.Fatalf("seed state wrong: wrong=%v right=%v",
			f.flaggedAt(t, wrong), f.flaggedAt(t, right))
	}

	// WIDEN: declare BT-FOREIGN too. The flagged carrier passes now.
	if err := f.db.SetPayloadBinTypes(f.payloadID, []int64{f.fitsID, f.foreignID}); err != nil {
		t.Fatalf("widen rule: %v", err)
	}
	if at := f.flaggedAt(t, wrong); at != nil {
		t.Errorf("widening the rule left the carrier flagged (at %v). The bin passes "+
			"now; an operator sent to fix it would find nothing to fix, which is how "+
			"a list stops being read.", *at)
	}

	// NARROW: declare only BT-FOREIGN. The other carrier is wrong now.
	if err := f.db.SetPayloadBinTypes(f.payloadID, []int64{f.foreignID}); err != nil {
		t.Fatalf("narrow rule: %v", err)
	}
	if f.flaggedAt(t, right) == nil {
		t.Error("narrowing the rule did not flag the carrier it excluded. That bin is " +
			"unsourceable from this moment and nothing on any surface says so.")
	}

	// EMPTY: no rows at all. THE SPARSE ARM arriving from the clearing side —
	// removing the last declaration must un-flag, never flag.
	if err := f.db.SetPayloadBinTypes(f.payloadID, nil); err != nil {
		t.Fatalf("empty rule: %v", err)
	}
	for _, id := range []int64{wrong, right} {
		if at := f.flaggedAt(t, id); at != nil {
			t.Errorf("bin %d is flagged at %v against a payload with NO declared "+
				"carriers. No declaration means no rule.", id, *at)
		}
	}
}
