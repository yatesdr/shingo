package binresolver

import (
	"encoding/json"
	"os"
	"testing"
	"time"

	"shingocore/dispatch/binsource"
	"shingocore/domain"
	"shingocore/store/bins"
)

// eligibility_golden_test.go — a photograph of every "may this bin be sourced?"
// predicate, one fixture set run through all of them, recorded as a golden file.
//
// WHY THIS EXISTS. The question is implemented independently in nine places and
// the implementations do not agree. Three of those are pure predicates and are
// covered here; the SQL-backed six are covered by the docker half (see
// eligibility_golden_docker_test.go). The count reached nine only after several
// sweeps missed readers, which is the argument for recording the answers rather
// than trying to hold them in your head.
//
// It deliberately does NOT assert the predicates agree — today they do not, and a
// test that pretended otherwise could not be written. It records what each one
// answers so that:
//   - intended divergence is documented in a form a reader can scan, and
//   - any change to any predicate shows up as a reviewable diff.
//
// That makes it the harness for collapsing these onto one shared predicate: a
// collapse commit that changes no behaviour produces NO diff here, and any diff
// it does produce is the thing to justify.
//
// Run with -update to regenerate:
//
//	go test -run TestGolden_Eligibility -update ./dispatch/binresolver/

// eligFixture is one bin's attributes. Fields map 1:1 onto the bins row columns
// the predicates read; nothing here is derived.
type eligFixture struct {
	Name      string
	Payload   string // the bin's own payload_code ("" = an empty carrier)
	UOP       int
	Cap       int // payloads.uop_capacity, JOINed in production (0 = unknown)
	Status    domain.BinStatus
	Confirmed bool
	Claimed   bool
	Locked    bool
	Reserved  bool // a pending reservation exists on this bin
	NoLoadedA bool // loaded_at IS NULL (an empty carrier never loaded)
}

// wantPayload is the part every predicate is asked for. Fixtures whose Payload
// differs are the "wrong part" arm.
const wantPayload = "PART-A"

// eligFixtures spans the axes the nine predicates disagree on. Each axis has at
// least one row that exposes a known divergence; see the brief section noted.
var eligFixtures = []eligFixture{
	// --- baseline ---
	{Name: "full_of_X_confirmed", Payload: wantPayload, UOP: 1000, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},
	{Name: "partial_of_X_confirmed", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},
	{Name: "empty_carrier", Payload: "", UOP: 0, Cap: 0, Status: domain.BinStatusAvailable, NoLoadedA: true},
	{Name: "wrong_part", Payload: "PART-B", UOP: 500, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},

	// --- content: the quantity axis. FindSourceFIFO has no uop floor. ---
	{Name: "zero_uop_still_labelled", Payload: wantPayload, UOP: 0, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},
	{Name: "negative_uop_labelled", Payload: wantPayload, UOP: -32, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},
	{Name: "over_capacity", Payload: wantPayload, UOP: 1200, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true},
	{Name: "cap_unknown_zero", Payload: wantPayload, UOP: 400, Cap: 0, Status: domain.BinStatusAvailable, Confirmed: true},

	// --- attestation: unconfirmed-with-payload is the admin Load-Payload state ---
	{Name: "unconfirmed_full", Payload: wantPayload, UOP: 1000, Cap: 1000, Status: domain.BinStatusAvailable},
	{Name: "unconfirmed_partial", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusAvailable},

	// --- status: three different rules across the nine ---
	{Name: "status_staged", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusStaged, Confirmed: true},
	{Name: "status_flagged", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusFlagged, Confirmed: true},
	{Name: "status_maintenance", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusMaintenance, Confirmed: true},
	{Name: "status_quality_hold", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusQualityHold, Confirmed: true},
	{Name: "status_retired", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusRetired, Confirmed: true},
	// Off-spec: the column carries no CHECK constraint. A reject-list predicate
	// admits this; an allow-list rejects it.
	{Name: "status_off_spec", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatus("quarantine"), Confirmed: true},

	// --- holds: isBinAvailableForRetrieve checks neither Locked nor Reserved ---
	{Name: "claimed", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true, Claimed: true},
	{Name: "locked", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true, Locked: true},
	{Name: "reserved_pending", Payload: wantPayload, UOP: 400, Cap: 1000, Status: domain.BinStatusAvailable, Confirmed: true, Reserved: true},
}

// bin builds the bins row this fixture describes.
func (f eligFixture) bin(id int64) *bins.Bin {
	b := &bins.Bin{
		ID:                    id,
		PayloadCode:           f.Payload,
		UOPRemaining:          f.UOP,
		UOPCapacity:           f.Cap,
		Status:                f.Status,
		ManifestConfirmed:     f.Confirmed,
		Locked:                f.Locked,
		HasPendingReservation: f.Reserved,
		CreatedAt:             time.Unix(1_700_000_000, 0).UTC(),
	}
	if f.Claimed {
		owner := int64(9999)
		b.ClaimedBy = &owner
	}
	if !f.NoLoadedA {
		loaded := time.Unix(1_700_000_500, 0).UTC()
		b.LoadedAt = &loaded
	}
	return b
}

// cand mirrors dispatch.candFromBin, which is unexported in that package. The
// mapping is field-for-field and carries no logic; if candFromBin changes shape,
// this must follow or the row stops describing production.
func (f eligFixture) cand(b *bins.Bin) binsource.Cand {
	return binsource.Cand{
		BinID:             b.ID,
		Payload:           b.PayloadCode,
		UOP:               b.UOPRemaining,
		Cap:               b.UOPCapacity,
		LoadedAt:          b.LoadedAt,
		CreatedAt:         b.CreatedAt,
		Claimed:           b.ClaimedBy != nil,
		Locked:            b.Locked,
		ManifestConfirmed: b.ManifestConfirmed,
		Status:            b.Status,
	}
}

// eligRow is one fixture's verdict from each pure predicate. "" means ACCEPTED;
// any other string is the predicate's own reason, kept verbatim so a reason
// change is as visible as a verdict change.
type eligRow struct {
	Fixture string `json:"fixture"`

	// #1 binsource.RejectReason — the dedicated-loader pool ranker, both intents.
	LoaderDrain string `json:"loader_drain"`
	LoaderFill  string `json:"loader_fill"`

	// #2 BinUnavailableReason — the concrete-node predicate (tier 4).
	ConcreteNode string `json:"concrete_node"`

	// #5 isBinAvailableForRetrieve — the NGRP resolver's non-lane predicate.
	NGRPResolver string `json:"ngrp_resolver"`
}

func TestGolden_Eligibility(t *testing.T) {
	t.Parallel()

	rows := make([]eligRow, 0, len(eligFixtures))
	for i, f := range eligFixtures {
		b := f.bin(int64(i + 1))
		c := f.cand(b)

		ngrp := ""
		if !isBinAvailableForRetrieve(b, wantPayload) {
			ngrp = "rejected"
		}

		rows = append(rows, eligRow{
			Fixture:      f.Name,
			LoaderDrain:  binsource.RejectReason(c, binsource.Want{Payload: wantPayload, Intent: binsource.Drain}),
			LoaderFill:   binsource.RejectReason(c, binsource.Want{Payload: wantPayload, Intent: binsource.Fill}),
			ConcreteNode: BinUnavailableReason(b, wantPayload),
			NGRPResolver: ngrp,
		})
	}

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	const goldenPath = "testdata/golden/eligibility_pure.json"
	if *updateFlag {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d fixtures)", goldenPath, len(rows))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden file %s not found (run with -update to create): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("eligibility matrix changed.\n--- want (golden) ---\n%s\n--- got ---\n%s\n"+
			"If this change is intended, re-run with -update and justify each differing row.",
			want, got)
	}
}
