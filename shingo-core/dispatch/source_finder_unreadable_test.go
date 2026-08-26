package dispatch

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
)

// source_finder_unreadable_test.go — MG3-1a. A SEARCH THAT DID NOT RUN IS NOT
// AN ANSWER, at every call site in the cascade.
//
// ── THE LESSON THIS ENCODES ─────────────────────────────────────────────────
//
// The MG2 campaign shipped an exclusion query naming a CTE that did not exist.
// It threw on every call. Every finder call site read `err != nil || bin == nil`
// as one condition, so the throw became "no empty found", the ask parked on a
// plausible cause, the group's carriers went untouched — and the observable
// behaviour was indistinguishable from the exclusion working. fmt, vet, lint,
// unit, race and all three docker suites came back clean on it
// (SIM-CAMPAIGN-mg2 §2).
//
// Phase 3 opens four query bodies and adds two new arms. This audit is what
// stops the same class of mistake from hiding in them.
//
// ── WHY IT IS A CAUSE AND NOT A LOG LINE ────────────────────────────────────
//
// "The plant is out of material" and "Core could not ask" have opposite
// remedies — one sends someone to the floor, the other resolves itself — and a
// queue-cause histogram is where that difference is read. Collapsing them makes
// an outage look like a shortage for as long as it lasts.
//
// WAIT, NEVER FAIL. A read that did not answer is not a fact about the plant,
// so the order holds and the ordinary scanner retry is the releaser. The
// alternative — treating an unreadable search as "none" and terminalising —
// kills orders on a Postgres blip.

// TestUnreadableSearch_ParksRatherThanAnswering drives a real error through
// each tier that runs a finder, and requires the SAME disposition every time.
func TestUnreadableSearch_ParksRatherThanAnswering(t *testing.T) {
	t.Parallel()
	boom := errors.New("relation \"descendants\" does not exist")

	cases := []struct {
		name  string
		build func() (*SourceFinder, SourceNeed)
		// the cause the SAME shape produces when the search runs and matches
		// nothing — the thing the unreadable cause must not be confused with.
		noneFoundCause QueueCause
	}{
		{
			name: "tier 3 — group-scoped empty",
			build: func() (*SourceFinder, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinGroup(110, "UNR-GRP"))
				db.addNode(pinNode(111, "UNR-DEST"))
				db.searchErr = boom
				return NewSourceFinder(db, nil, nil), SourceNeed{
					SourceNode: "UNR-GRP", DeliveryNode: "UNR-DEST",
					PayloadCode: "PANEL-A", Intent: IntentEmpty,
				}
			},
			noneFoundCause: CauseFinderGroupEmpty,
		},
		{
			// TIER 4 DISCARDED ITS ERROR ENTIRELY — `candidates, _ :=` — which is
			// the same collapse in a shape that cannot even be misread: it states
			// that the outcome does not depend on whether the read worked.
			name: "tier 4 — node-local candidates",
			build: func() (*SourceFinder, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(115, "UNR-SLOT"))
				db.addNode(pinNode(116, "UNR-ELSEWHERE"))
				db.nodeBinsErr = boom
				return NewSourceFinder(db, nil, nil), SourceNeed{
					SourceNode: "UNR-SLOT", DeliveryNode: "UNR-ELSEWHERE",
					PayloadCode: "PANEL-A", Intent: IntentFull, NodeLocal: true,
				}
			},
			noneFoundCause: CauseFinderNodeEmpty,
		},
		{
			name: "tier 5 — plant-wide full retrieve",
			build: func() (*SourceFinder, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(121, "UNR-LINE"))
				db.searchErr = boom
				return NewSourceFinder(db, nil, nil), SourceNeed{
					DeliveryNode: "UNR-LINE", PayloadCode: "PANEL-A", Intent: IntentFull,
				}
			},
			noneFoundCause: CauseFinderPlantEmpty,
		},
		{
			name: "tier 5 — plant-wide empty, untyped",
			build: func() (*SourceFinder, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(131, "UNR-LINE2"))
				db.searchErr = boom
				return NewSourceFinder(db, nil, nil), SourceNeed{
					DeliveryNode: "UNR-LINE2", PayloadCode: "PANEL-A", Intent: IntentEmpty,
				}
			},
			noneFoundCause: CauseFinderPlantEmpty,
		},
		{
			name: "tier 5 — plant-wide empty, typed by a maintain origin",
			build: func() (*SourceFinder, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(141, "UNR-LINE3"))
				db.addNode(pinGroup(142, "UNR-KEEPER-GRP"))
				db.maintainedType["11111111-2222-3333-4444-555555555555"] = "UNR-45x58"
				db.maintainedGroup["11111111-2222-3333-4444-555555555555"] = "UNR-KEEPER-GRP"
				db.searchErr = boom
				return NewSourceFinder(db, nil, nil), SourceNeed{
					DeliveryNode: "UNR-LINE3", Intent: IntentEmpty,
					OriginID: "11111111-2222-3333-4444-555555555555",
				}
			},
			noneFoundCause: CauseFinderNoEmptyOfType,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, need := tc.build()
			got := f.FindSourceForNeed(need)

			if got.Outcome != OutcomeWait {
				t.Fatalf("Outcome = %v, want Wait. A read that did not answer is not a fact "+
					"about the plant — terminalising here kills orders on a database blip",
					got.Outcome)
			}
			if got.QueueCause == tc.noneFoundCause {
				t.Errorf("QueueCause = %q — the SAME cause a successful search producing "+
					"nothing would give. That is the collapse: an outage reads as a shortage, "+
					"and the histogram cannot tell them apart", got.QueueCause)
			}
			if got.QueueCause != CauseFinderSourceUnreadable {
				t.Errorf("QueueCause = %q, want %q", got.QueueCause, CauseFinderSourceUnreadable)
			}
			if got.QueueCode != protocol.QueueWaitingForMaterial {
				t.Errorf("QueueCode = %q, want %q", got.QueueCode, protocol.QueueWaitingForMaterial)
			}
		})
	}
}

// AND sql.ErrNoRows STILL MEANS NONE-FOUND, which is the other half.
//
// An audit that made every empty result unreadable would be just as wrong in
// the opposite direction, and far more visible: every dry group in the plant
// would report as a Core outage. This is the arm that keeps the fix honest, and
// it is why the fakes in this package return sql.ErrNoRows rather than a
// hand-rolled error — they now say what the store says.
func TestNoneFoundIsStillNoneFound(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinGroup(150, "NF-GRP"))
	db.addNode(pinNode(151, "NF-DEST"))
	// No searchErr: the finders return sql.ErrNoRows, exactly as the store does.

	f := NewSourceFinder(db, nil, nil)
	got := f.FindSourceForNeed(SourceNeed{
		SourceNode: "NF-GRP", DeliveryNode: "NF-DEST",
		PayloadCode: "PANEL-A", Intent: IntentEmpty,
	})

	if got.QueueCause != CauseFinderGroupEmpty {
		t.Errorf("QueueCause = %q, want %q. sql.ErrNoRows is the store's spelling of "+
			"'the query ran and matched nothing' — reading it as a failure would report "+
			"every dry group in the plant as an outage", got.QueueCause, CauseFinderGroupEmpty)
	}
}

// sourceSearchFailed is the one place the distinction is decided, so it gets a
// direct test as well as the four call-site ones above.
//
// THE WRAPPED CASE IS THE ONE THAT MATTERS. The store wraps with %w in some
// paths and returns the sentinel bare in others; a classifier using == rather
// than errors.Is would read a wrapped none-found as a failure, which is the
// same collapse pointing the other way.
func TestSourceSearchFailed_ClassifiesTheThreeCases(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"nil — neither a failure nor a match", nil, false},
		{"bare sql.ErrNoRows — the query ran, nothing matched", sql.ErrNoRows, false},
		{"WRAPPED sql.ErrNoRows — still none-found", errNoRowsWrapped(), false},
		{"a real error — the query did not run", errors.New("connection refused"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sourceSearchFailed(tc.err); got != tc.want {
				t.Errorf("sourceSearchFailed(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func errNoRowsWrapped() error {
	return errWrap("count empty 45x58 in group 12", sql.ErrNoRows)
}

func errWrap(msg string, err error) error {
	return &wrapped{msg: msg, err: err}
}

type wrapped struct {
	msg string
	err error
}

func (w *wrapped) Error() string { return w.msg + ": " + w.err.Error() }
func (w *wrapped) Unwrap() error { return w.err }

// A found bin is still a found bin — the audit adds an arm, it does not gate
// the happy path behind a new condition.
func TestUnreadableAudit_LeavesTheFoundPathAlone(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	at := int64(161)
	db.addNode(pinNode(160, "OK-DEST"))
	db.addNode(pinNode(at, "OK-SLOT"))
	db.globalEmpty = &bins.Bin{ID: 1600, Label: "OK-BIN", NodeID: &at}

	f := NewSourceFinder(db, nil, nil)
	got := f.FindSourceForNeed(SourceNeed{
		DeliveryNode: "OK-DEST", PayloadCode: "PANEL-A", Intent: IntentEmpty,
	})
	if got.Outcome != OutcomeFound || got.Bin == nil || got.Bin.ID != 1600 {
		t.Fatalf("outcome=%v bin=%v, want Found with bin 1600", got.Outcome, got.Bin)
	}
}

// ── MG3-1: the fence reaches the query, with both rules on it ───────────────

// The finder BUILDS the fence from the need and HANDS IT DOWN. The rules
// themselves are tested at the query (store/bins/fence_test.go); what is
// asserted here is the wire — that the two inputs travel, unaltered, from the
// order to the search.
//
// It replaces the MG2-11-era assertion that recorded a subtree id. Same
// property, one layer up: the argument is the behaviour, so a finder that
// dropped it would let the fence be removed with every query test still green.
func TestEmptyFence_ReachesThePlantWideSearch(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		need SourceNeed
		want bins.EmptyFence
	}{
		{
			name: "an ordinary ask fences nothing",
			need: SourceNeed{DeliveryNode: "FN-DEST", PayloadCode: "PANEL-A", Intent: IntentEmpty},
			want: bins.EmptyFence{},
		},
		{
			name: "a press carries its process node — rule (i)'s input",
			need: SourceNeed{DeliveryNode: "FN-DEST", PayloadCode: "PANEL-A", Intent: IntentEmpty,
				ProcessNode: "FN-PRESS-1"},
			want: bins.EmptyFence{ProcessNode: "FN-PRESS-1"},
		},
		{
			name: "a keeper top-off carries its own group — rule (ii)'s input",
			need: SourceNeed{DeliveryNode: "FN-DEST", Intent: IntentEmpty,
				OriginID: "11111111-2222-3333-4444-555555555555"},
			want: bins.EmptyFence{OriginGroup: "FN-KEEPER-GRP"},
		},
		{
			name: "both, when both are known",
			need: SourceNeed{DeliveryNode: "FN-DEST", Intent: IntentEmpty,
				ProcessNode: "FN-PRESS-1", OriginID: "11111111-2222-3333-4444-555555555555"},
			want: bins.EmptyFence{ProcessNode: "FN-PRESS-1", OriginGroup: "FN-KEEPER-GRP"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := newFakeFinderDB()
			db.addNode(pinNode(200, "FN-DEST"))
			db.maintainedType["11111111-2222-3333-4444-555555555555"] = "FN-45x58"
			db.maintainedGroup["11111111-2222-3333-4444-555555555555"] = "FN-KEEPER-GRP"

			NewSourceFinder(db, nil, nil).FindSourceForNeed(tc.need)

			if db.lastFence != tc.want {
				t.Errorf("fence handed to the search = %+v, want %+v", db.lastFence, tc.want)
			}
		})
	}
}

// A READ FAILURE FENCES NOTHING rather than fencing everything.
//
// The direction is chosen, not incidental. What an unreadable episode costs
// here is one carrier taken from a fenced group, which the next tick corrects.
// The alternative — read failure means "fence everything" — turns one bad read
// into every empty pull in the plant parking at once.
func TestEmptyFence_ReadFailureDoesNotFenceEverything(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(210, "FN-ERR-DEST"))
	db.maintainedTypeErr = errors.New("episode read failed")

	NewSourceFinder(db, nil, nil).FindSourceForNeed(SourceNeed{
		DeliveryNode: "FN-ERR-DEST", PayloadCode: "PANEL-A", Intent: IntentEmpty,
		ProcessNode: "FN-PRESS-9", OriginID: "11111111-2222-3333-4444-555555555555",
	})

	// The process half still travels — it did not come from the failed read.
	if db.lastFence.OriginGroup != "" {
		t.Errorf("OriginGroup = %q after a failed episode read, want blank — a guess about "+
			"which group an ask is filling is worse than no fence", db.lastFence.OriginGroup)
	}
	if db.lastFence.ProcessNode != "FN-PRESS-9" {
		t.Errorf("ProcessNode = %q, want it carried through. It comes off the order, not "+
			"out of the read that failed", db.lastFence.ProcessNode)
	}
}
