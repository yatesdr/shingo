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

// bin_type_rule_pin_test.go — the bin-type axis across the two Go predicates
// the three dispatch doors run on.
//
// WHY A SEPARATE PHOTOGRAPH. eligibility_pure.json spans the bins-row axes
// (content, attestation, status, holds) and its fixture list is shared with the
// docker half, so a row added there lands in eligibility_sql.json and
// dig_readers.json too. The bin-type rule is a different axis — it is a fact
// about the PAYLOAD, held in payload_bin_types, not a column on the bin — and
// the SQL half already photographs it (the bin_type_rule_excludes node axis).
// What was never photographed is the GO half, which is what this file is.
//
// THE THREE DOORS REDUCE TO TWO PREDICATES. Tier-4 concrete-node pickup
// (source_finder.go), the complex allocator (allocator.go) and the reserve
// reconcile all run BinUnavailableReason; the tier-2 dedicated-loader pool runs
// binsource.RejectReason through binsource.Source. So the doors are pinned by
// pinning the two, and a third door added tomorrow inherits the rule instead of
// needing a fourth patch.
//
// ── WHAT THIS FILE RECORDS AT THE PRE-CHANGE TREE ─────────────────────────
//
// THE HOLE ITSELF. Every row below is ACCEPTED ("") today, including the bin
// whose type the payload's allow-list excludes, because neither predicate reads
// payload_bin_types at all — the three doors list bins with ListBinsByNode
// (`status != 'retired'` only) and filter in Go, and the Go filter has no
// bin-type term. The SQL sourcing readers have enforced the rule since
// 493a8062; these three doors never ran SQL, so the rule stops at them.
//
// The fixture bin's BinTypeID varies across the cases and no verdict moves with
// it. That invariance IS the photograph: it is what makes the diff after the
// rule lands attributable, cell by cell, to the rule and to nothing else.
//
// Run with -update to regenerate:
//
//	go test -run TestGolden_BinTypeRule -update ./dispatch/binresolver/

// binTypeCase is one (bin type, payload allow-list) pairing. The bin is the
// baseline sourceable bin in every case — full of wantPayload, confirmed,
// unheld, ordinary status — so a verdict that moves is attributable to the
// bin-type rule and to nothing else.
type binTypeCase struct {
	Name string
	// BinTypeID is the type the fixture bin IS.
	BinTypeID int64
	// Allowed is what payload_bin_types says may carry wantPayload. nil is the
	// "no rows" arm — the payload is undescribed and constrains nothing. An
	// empty non-nil slice is a payload described as carryable by nothing.
	//
	// Nothing reads it at the pre-change tree. It is declared now so the case
	// names mean the same thing on both sides of the change.
	Allowed []int64
	// Shape is what the bin HOLDS, and it is here because the Fill intent
	// rejects a full bin ("full-not-a-container") before it ever reaches a type
	// check. A photograph taken only over full bins would show the Fill door's
	// type rule masked by the intent rule and would stay green through a change
	// that never reached it.
	Shape binTypeShape
}

// binTypeShape selects the fixture bin's content. Drain needs stock; Fill needs
// a container, so each door is exercised by a different shape and both must be
// present for the matrix to mean anything.
type binTypeShape int

const (
	shapeFull    binTypeShape = iota // 1000/1000 of wantPayload — a Drain source
	shapePartial                     // 400/1000 of wantPayload — eligible to both
	shapeEmpty                       // an empty carrier — a Fill container only
)

const (
	binTypeFits    = int64(1) // the type the allow-list names
	binTypeForeign = int64(2) // a type it does not
)

var binTypeCases = []binTypeCase{
	// --- full: the Drain door's axis ---
	{Name: "full_no_rules_for_payload", BinTypeID: binTypeForeign, Allowed: nil, Shape: shapeFull},
	{Name: "full_type_is_allowed", BinTypeID: binTypeFits, Allowed: []int64{binTypeFits}, Shape: shapeFull},
	{Name: "full_type_is_not_allowed", BinTypeID: binTypeForeign, Allowed: []int64{binTypeFits}, Shape: shapeFull},
	{Name: "full_type_allowed_among_several", BinTypeID: binTypeFits, Allowed: []int64{binTypeFits, 7, 9}, Shape: shapeFull},
	{Name: "full_payload_allows_nothing", BinTypeID: binTypeFits, Allowed: []int64{}, Shape: shapeFull},

	// --- partial: eligible to BOTH intents, so one row moves two doors ---
	{Name: "partial_no_rules_for_payload", BinTypeID: binTypeForeign, Allowed: nil, Shape: shapePartial},
	{Name: "partial_type_is_allowed", BinTypeID: binTypeFits, Allowed: []int64{binTypeFits}, Shape: shapePartial},
	{Name: "partial_type_is_not_allowed", BinTypeID: binTypeForeign, Allowed: []int64{binTypeFits}, Shape: shapePartial},

	// --- empty: the Fill door's own axis. An empty is fungible to the ranker,
	// but a carrier about to be filled with X must still be a type that may
	// carry X — which is the produce-side half of the same rule.
	{Name: "empty_no_rules_for_payload", BinTypeID: binTypeForeign, Allowed: nil, Shape: shapeEmpty},
	{Name: "empty_type_is_allowed", BinTypeID: binTypeFits, Allowed: []int64{binTypeFits}, Shape: shapeEmpty},
	{Name: "empty_type_is_not_allowed", BinTypeID: binTypeForeign, Allowed: []int64{binTypeFits}, Shape: shapeEmpty},
}

// binTypeFixtureBin is the baseline sourceable bin — confirmed, unheld,
// ordinary status — carrying the case's type and content shape. Every rejection
// in this photograph is the bin-type rule or the intent rule, and the case name
// says which is which.
func binTypeFixtureBin(c binTypeCase) *bins.Bin {
	loaded := time.Unix(1_700_000_500, 0).UTC()
	code := "FITS"
	if c.BinTypeID != binTypeFits {
		code = "FOREIGN"
	}
	b := &bins.Bin{
		ID:                1,
		BinTypeID:         c.BinTypeID,
		BinTypeCode:       code,
		PayloadCode:       wantPayload,
		UOPRemaining:      1000,
		UOPCapacity:       1000,
		Status:            domain.BinStatusAvailable,
		ManifestConfirmed: true,
		CreatedAt:         time.Unix(1_700_000_000, 0).UTC(),
		LoadedAt:          &loaded,
	}
	switch c.Shape {
	case shapePartial:
		b.UOPRemaining = 400
	case shapeEmpty:
		b.PayloadCode = ""
		b.UOPRemaining = 0
		b.UOPCapacity = 0
		b.LoadedAt = nil
	}
	return b
}

// binTypeRow is one case's verdict from each door's predicate.
type binTypeRow struct {
	Case string `json:"case"`

	// BinUnavailableReason — tier-4 concrete-node pickup, the complex allocator,
	// and the reserve reconcile.
	ConcreteNode string `json:"concrete_node"`

	// binsource.RejectReason — the tier-2 dedicated-loader pool, both intents.
	LoaderDrain string `json:"loader_drain"`
	LoaderFill  string `json:"loader_fill"`
}

func TestGolden_BinTypeRule(t *testing.T) {
	t.Parallel()

	rows := make([]binTypeRow, 0, len(binTypeCases))
	for _, c := range binTypeCases {
		b := binTypeFixtureBin(c)

		rule := domain.NewBinTypeRule(c.Allowed)
		cand := binsource.Cand{
			BinID:             b.ID,
			BinTypeID:         b.BinTypeID,
			Payload:           b.PayloadCode,
			UOP:               b.UOPRemaining,
			Cap:               b.UOPCapacity,
			LoadedAt:          b.LoadedAt,
			CreatedAt:         b.CreatedAt,
			ManifestConfirmed: b.ManifestConfirmed,
			Status:            b.Status,
		}

		rows = append(rows, binTypeRow{
			Case:         c.Name,
			ConcreteNode: BinUnavailableReason(b, wantPayload, rule),
			LoaderDrain:  binsource.RejectReason(cand, binsource.Want{Payload: wantPayload, Intent: binsource.Drain, BinTypes: rule}),
			LoaderFill:   binsource.RejectReason(cand, binsource.Want{Payload: wantPayload, Intent: binsource.Fill, BinTypes: rule}),
		})
	}

	got, err := json.MarshalIndent(rows, "", "  ")
	if err != nil {
		t.Fatalf("marshal rows: %v", err)
	}
	got = append(got, '\n')

	const goldenPath = "testdata/golden/bin_type_rule.json"
	if *updateFlag {
		if err := os.MkdirAll("testdata/golden", 0o755); err != nil {
			t.Fatalf("mkdir golden: %v", err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated %s (%d cases)", goldenPath, len(rows))
		return
	}

	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Skipf("golden file %s not found (run with -update to create): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("bin-type matrix changed.\n--- want (golden) ---\n%s\n--- got ---\n%s\n"+
			"If this change is intended, re-run with -update and justify each differing row.",
			want, got)
	}
}

// TestBinTypeRuleBindsBothPredicates is the assertion half of the photograph.
//
// It replaces TestBinTypeRuleIsInvisibleToBothPredicates, which stated the hole
// at the pre-change tree — that neither predicate could see a bin's type — and
// was written to go red when the rule landed. It did. This is the same claim
// with the sign flipped, and the pair is the record that the cell moved on
// purpose rather than drifting.
func TestBinTypeRuleBindsBothPredicates(t *testing.T) {
	t.Parallel()

	rule := domain.NewBinTypeRule([]int64{binTypeFits})
	excluded := binTypeFixtureBin(binTypeCase{BinTypeID: binTypeForeign, Shape: shapeFull})

	if got := BinUnavailableReason(excluded, wantPayload, rule); got == "" {
		t.Error("BinUnavailableReason accepted a bin whose type the payload's " +
			"allow-list excludes; the rule is hard at the shingo level")
	}

	cand := binsource.Cand{
		BinID: 1, BinTypeID: binTypeForeign, Payload: wantPayload, UOP: 1000, Cap: 1000,
		ManifestConfirmed: true, Status: domain.BinStatusAvailable,
		CreatedAt: time.Unix(1_700_000_000, 0).UTC(),
	}
	if got := binsource.RejectReason(cand, binsource.Want{
		Payload: wantPayload, Intent: binsource.Drain, BinTypes: rule,
	}); got != "bin-type" {
		t.Errorf("binsource.RejectReason = %q, want \"bin-type\" for a carrier the part may not travel in", got)
	}
}

// TestBinTypeRule_NoRulesPermitsEverything is the coverage half, and it is the
// arm that must never tighten: payload_bin_types is sparsely populated, and a
// payload nobody has described yet constrains nothing. A pre-2026-04-27 hard
// INNER JOIN starved orders for exactly this and the fallback exists to stop it
// happening again.
func TestBinTypeRule_NoRulesPermitsEverything(t *testing.T) {
	t.Parallel()
	var unrestricted domain.BinTypeRule // no rows for the payload
	b := binTypeFixtureBin(binTypeCase{BinTypeID: binTypeForeign, Shape: shapeFull})
	if got := BinUnavailableReason(b, wantPayload, unrestricted); got != "" {
		t.Errorf("an undescribed payload refused bin type %d: %q — coverage is a gap "+
			"in the data, not a licence to refuse", b.BinTypeID, got)
	}
}

// TestBinTypeRule_ZeroValueIsUnrestricted pins the arm every door with no
// payload in hand relies on: a removal leg clearing whatever is resident, an
// empty pickup that dropped its payload context. Those doors ask for a bin
// without naming a part, and a part is what the rule is about — so the zero
// value must permit everything, or they start refusing every bin.
func TestBinTypeRule_ZeroValueIsUnrestricted(t *testing.T) {
	t.Parallel()
	var zero domain.BinTypeRule
	for _, id := range []int64{0, binTypeFits, binTypeForeign, 99} {
		if !zero.Permits(id) {
			t.Errorf("zero BinTypeRule refused bin type %d; it must permit every type", id)
		}
	}
}

// TestBinTypeRule_EmptyRuleRefusesEverything separates the two empties, which
// is the distinction the nil map carries and a len()==0 check would lose: a
// payload with NO ROWS constrains nothing, and a payload described as carryable
// by an empty set of types constrains everything. Collapsing them would read a
// configuration gap as permission.
func TestBinTypeRule_EmptyRuleRefusesEverything(t *testing.T) {
	t.Parallel()
	if !domain.NewBinTypeRule(nil).Permits(binTypeFits) {
		t.Error("NewBinTypeRule(nil) must be unrestricted — no rows means no rule")
	}
	if domain.NewBinTypeRule([]int64{}).Permits(binTypeFits) {
		t.Error("NewBinTypeRule([]) must refuse every type — a payload described " +
			"as carryable by nothing must not read as a licence")
	}
}
