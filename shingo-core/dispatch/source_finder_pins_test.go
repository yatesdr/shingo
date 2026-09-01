package dispatch

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// source_finder_pins_test.go — MG3-0. The cascade's behaviour as it stands
// TODAY, pinned before any WHERE body is opened.
//
// WHY THESE EXIST, and why they are not "extra coverage". Phase 3 rewrites the
// predicate bodies of four empty finders and adds two new arms to them. The
// cascade's observable contract is (which finder was consulted, what outcome,
// which queue cause) — and almost none of that was asserted. Exactly one finder
// cause appeared in any test before this file; `CauseFinderNoEmptyOfType`, the
// cause the typed path exists to produce, appeared in none.
//
// So a strict arm that quietly widened a scope, or a fragment consolidation
// that swapped one tier's cause for its neighbour's, would have landed green.
// These pins are what make phase 3's diffs readable as intent rather than as
// hope — and with mid-lane docker gating dropped for this phase, they are also
// the lane's own safety net.
//
// THEY ARE CHARACTERIZATIONS, NOT ENDORSEMENTS. Some of what is pinned here is
// behaviour phase 3 intends to CHANGE — the dig-blind selection most of all.
// Pinning it first is what makes the reversal legible: MG3-1b edits a test that
// says "today it does this", rather than adding one that says "now it does
// that" against nothing.

// ── The tier → cause golden matrix ──────────────────────────────────────────

// pinNode is the least node that satisfies the cascade's lookups.
func pinNode(id int64, name string) *nodes.Node {
	return &nodes.Node{ID: id, Name: name, Enabled: true}
}

func pinGroup(id int64, name string) *nodes.Node {
	return &nodes.Node{ID: id, Name: name, Enabled: true, IsSynthetic: true,
		NodeTypeCode: protocol.NodeClassNGRP}
}

// TestPin_TierToCauseMatrix asserts, per tier, the exact triple a DRY need
// produces — and that no neighbouring finder was consulted on the way.
//
// THE NEGATIVE CALL COUNTS ARE HALF THE POINT. "No fall-through" is the
// property four separate incidents were about (the A4 replay drift, the loader
// pool family, the Hopkinsville wrong-supermarket, the node-local widening),
// and it is invisible in an outcome assertion: a tier that queued for the right
// reason after also running the plant-wide scan looks identical from the
// outside and is a different program.
func TestPin_TierToCauseMatrix(t *testing.T) {
	t.Parallel()

	type calls struct{ fifo, globalEmpty, groupEmpty, typedGroup, typedGlobal int }

	cases := []struct {
		name string
		// build returns a finder and the need to run through it.
		build func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed)
		// what the cascade must answer.
		outcome Outcome
		code    protocol.QueueCode
		cause   QueueCause
		// and which finders must have been consulted. Every field is asserted,
		// so a NEW call on any of them fails here.
		want calls
	}{
		{
			name: "tier 1 — NGRP source, resolver has no material",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinGroup(10, "PIN-GRP"))
				db.addNode(pinNode(11, "PIN-DEST"))
				r := &pinResolver{err: errors.New("no available slot in node group PIN-GRP")}
				return NewSourceFinder(db, r, nil), db, SourceNeed{
					SourceNode: "PIN-GRP", DeliveryNode: "PIN-DEST",
					PayloadCode: "PANEL-A", Intent: IntentFull,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderGroupEmpty,
			// TIER 1 IS TERMINAL ON A CAPACITY ERROR. Falling through to the
			// plant-wide FIFO here is precisely the A4 drift.
			want: calls{},
		},
		{
			name: "tier 3 — group-scoped empty, untyped, group dry",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinGroup(20, "PIN-MKT"))
				db.addNode(pinNode(21, "PIN-LINE"))
				return NewSourceFinder(db, nil, nil), db, SourceNeed{
					SourceNode: "PIN-MKT", DeliveryNode: "PIN-LINE",
					PayloadCode: "PANEL-A", Intent: IntentEmpty,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderGroupEmpty,
			// SCOPED, NOT WIDENED — Hopkinsville 2026-05-14 was a wrong-
			// supermarket pull, and the fix was this tier refusing to fall
			// through. groupEmpty is 1: it asked its own group and stopped.
			want: calls{groupEmpty: 1},
		},
		{
			name: "tier 4 — node-local move, nothing parked at the node",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(30, "PIN-SLOT"))
				db.addNode(pinNode(31, "PIN-ELSEWHERE"))
				return NewSourceFinder(db, nil, nil), db, SourceNeed{
					SourceNode: "PIN-SLOT", DeliveryNode: "PIN-ELSEWHERE",
					PayloadCode: "PANEL-A", Intent: IntentFull, NodeLocal: true,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderNodeEmpty,
			// A MOVE NEVER WIDENS. Tier 5 gates on !moveShaped, and this zero
			// is the whole node-local widening class.
			want: calls{},
		},
		{
			// THE CASE THE !moveShaped GATE ACTUALLY GUARDS, found by mutating
			// the gate away and watching every other pin stay green.
			//
			// A move-shaped need with an unresolvable source AND a blank payload
			// threads past every earlier tier: tier 1 needs a synthetic node,
			// tier 2 gates on `payloadCode != ""`, tier 4 gates on
			// `srcNode != nil`. It therefore arrives at tier 5 with nothing
			// found and only `!moveShaped` between it and a plant-wide scan.
			// Delete the gate and this move sources a carrier from anywhere in
			// the plant — the node-local widening class, with the order's own
			// named source silently ignored.
			//
			// The tier-4 case below cannot catch that: it returns Wait before
			// tier 5 is reachable at all, so its zero counts hold for a reason
			// that has nothing to do with the gate. Neither can a version of
			// this case carrying a payload — tier 2 answers first, with
			// `loader-source-unreadable`. Both were tried; both were mutation-
			// green. The blank payload is what makes the pin bite.
			name: "tier 5 gate — move-shaped, unresolvable source, blank payload never widens",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(35, "PIN-MOVE-DEST"))
				db.fifoBin = &bins.Bin{ID: 350, Label: "PIN-TEMPTING-BIN"}
				return NewSourceFinder(db, nil, nil), db, SourceNeed{
					SourceNode: "PIN-NO-SUCH-NODE", DeliveryNode: "PIN-MOVE-DEST",
					Intent: IntentFull, NodeLocal: true,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderPlantEmpty,
			want:  calls{},
		},
		{
			name: "tier 5 — plant-wide full retrieve, nothing anywhere",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(41, "PIN-LINE5"))
				return NewSourceFinder(db, nil, nil), db, SourceNeed{
					DeliveryNode: "PIN-LINE5", PayloadCode: "PANEL-A", Intent: IntentFull,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderPlantEmpty,
			want:  calls{fifo: 1},
		},
		{
			name: "tier 5 — plant-wide empty, untyped, nothing anywhere",
			build: func(t *testing.T) (*SourceFinder, *fakeFinderDB, SourceNeed) {
				db := newFakeFinderDB()
				db.addNode(pinNode(51, "PIN-LINE6"))
				return NewSourceFinder(db, nil, nil), db, SourceNeed{
					DeliveryNode: "PIN-LINE6", PayloadCode: "PANEL-A", Intent: IntentEmpty,
				}
			},
			outcome: OutcomeWait, code: protocol.QueueWaitingForMaterial,
			cause: CauseFinderPlantEmpty,
			want:  calls{globalEmpty: 1},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f, db, need := tc.build(t)
			got := f.FindSourceForNeed(need)

			if got.Outcome != tc.outcome {
				t.Errorf("Outcome = %v, want %v", got.Outcome, tc.outcome)
			}
			if got.QueueCode != tc.code {
				t.Errorf("QueueCode = %q, want %q", got.QueueCode, tc.code)
			}
			if got.QueueCause != tc.cause {
				t.Errorf("QueueCause = %q, want %q. The cause IS the operator deliverable — "+
					"it is what a histogram separates and what the releaser keys on — so a "+
					"tier that produces its neighbour's cause has changed behaviour even if "+
					"the outcome is identical", got.QueueCause, tc.cause)
			}
			gotCalls := calls{db.fifoCalls, db.globalEmptyCalls, db.groupEmptyCalls,
				db.typedGroupCalls, db.typedGlobalCalls}
			if gotCalls != tc.want {
				t.Errorf("finder calls = %+v, want %+v. A non-zero where zero was expected is "+
					"a FALL-THROUGH, which is the bug class this cascade's shape exists to "+
					"prevent; a zero where one was expected means the tier stopped being "+
					"reached at all", gotCalls, tc.want)
			}
		})
	}
}

// pinResolver is the least NodeResolver the tier-1 arm needs.
type pinResolver struct {
	err error
	res *binresolver.ResolveResult
}

func (r *pinResolver) Resolve(_ *nodes.Node, _ binresolver.ResolveMode, _ string,
	_ *int64, _ reservations.DigAsker, _ binresolver.BinFilter) (*binresolver.ResolveResult, error) {
	if r.err != nil {
		return nil, r.err
	}
	return r.res, nil
}

// ── Honored, not approximated, at the tier ──────────────────────────────────

// A declared type is searched FOR, and its absence WAITS scoped.
//
// THE FAILURE THIS FORBIDS is the plausible one: ask for any empty, then reject
// the wrong type after the fact. One wrong-typed carrier standing at the mouth
// would then mask every right-typed one behind it, and the order would wait
// while the material it needed sat two slots away. The type has to be in the
// WHERE, which is a claim about the query and is therefore asserted through the
// query the tier actually calls.
//
// `CauseFinderNoEmptyOfType` appeared in NO test before this one.
func TestPin_TypedGroupNeedWaitsScopedRatherThanWidening(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinGroup(60, "PIN-TYPED-GRP"))
	db.addNode(pinNode(61, "PIN-TYPED-DEST"))

	// The type the need wants is absent; ANOTHER type is present in the group,
	// and the wanted type is available plant-wide. Both are traps.
	db.typedEmpty["PIN-OTHER"] = &bins.Bin{ID: 900, Label: "WRONG-TYPE"}
	db.globalEmpty = &bins.Bin{ID: 901, Label: "RIGHT-TYPE-PLANT-WIDE"}

	// A maintain origin pins the type — the one wire that makes a need typed.
	db.maintainedType["11111111-2222-3333-4444-555555555555"] = "PIN-WANTED"
	db.maintainedGroup["11111111-2222-3333-4444-555555555555"] = "PIN-TYPED-GRP"

	f := NewSourceFinder(db, nil, nil)
	got := f.FindSourceForNeed(SourceNeed{
		SourceNode: "PIN-TYPED-GRP", DeliveryNode: "PIN-TYPED-DEST",
		Intent: IntentEmpty, OriginID: "11111111-2222-3333-4444-555555555555",
	})

	if got.Outcome != OutcomeWait {
		t.Fatalf("Outcome = %v, want Wait — a declared type that is absent WAITS", got.Outcome)
	}
	if got.QueueCause != CauseFinderNoEmptyOfType {
		t.Errorf("QueueCause = %q, want %q. 'The group has empties, none of the type' is a "+
			"different operator situation from 'the group is empty', and the two must not "+
			"collapse", got.QueueCause, CauseFinderNoEmptyOfType)
	}
	if db.typedGroupCalls != 1 {
		t.Errorf("typedGroupCalls = %d, want 1 — the TYPED group finder is the one a typed "+
			"need must reach", db.typedGroupCalls)
	}
	if db.groupEmptyCalls != 0 {
		t.Errorf("groupEmptyCalls = %d, want 0 — the untyped group finder would have taken "+
			"the wrong-typed carrier", db.groupEmptyCalls)
	}
	if db.globalEmptyCalls != 0 || db.typedGlobalCalls != 0 {
		t.Errorf("plant-wide calls = %d untyped / %d typed, want 0/0. A group-scoped need "+
			"that widens is the Hopkinsville wrong-supermarket pull",
			db.globalEmptyCalls, db.typedGlobalCalls)
	}
}

// ── Replay determinism ──────────────────────────────────────────────────────

// UNCHANGED STATE → THE SAME ANSWER, twice.
//
// It distinguishes "the scanner re-runs selection" from "the scanner re-picks
// around what it tried last time". The cascade holds no memory between runs by
// design, and the dig-blind loop pinned below depends on that being true: a
// finder that rotated candidates would turn the source-lock loop into an
// eventually-succeeding retry, and MG3-1b would be fixing nothing.
func TestPin_ReplayIsDeterministic(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(71, "PIN-DET-DEST"))
	// The carrier stands at a real node: tier 6 reads the slot back to decide
	// whether the pick is buried, so a node-less bin would fail there for a
	// reason that has nothing to do with determinism.
	at := int64(72)
	db.addNode(pinNode(at, "PIN-DET-SLOT"))
	db.globalEmpty = &bins.Bin{ID: 700, Label: "PIN-DET-BIN", NodeID: &at}

	f := NewSourceFinder(db, nil, nil)
	need := SourceNeed{DeliveryNode: "PIN-DET-DEST", PayloadCode: "PANEL-A", Intent: IntentEmpty}

	first := f.FindSourceForNeed(need)
	second := f.FindSourceForNeed(need)

	if first.Outcome != OutcomeFound || second.Outcome != OutcomeFound {
		t.Fatalf("outcomes = %v / %v, want Found both times", first.Outcome, second.Outcome)
	}
	if first.Bin == nil || second.Bin == nil || first.Bin.ID != second.Bin.ID {
		t.Fatalf("bins = %v / %v — unchanged state must select the same carrier. A cascade "+
			"that rotated would mask the source-lock loop as a retry that eventually wins",
			first.Bin, second.Bin)
	}
}

// ── The Asker, now that it is filled ────────────────────────────────────────

// SourceNeed.Asker IS FILLED ON THE SIMPLE PATH — the inverse of what MG3-0
// pinned, and the inversion is the point.
//
// The pin this replaces asserted the field was the ZERO value, because the
// field's own doc claimed "FindSource fills it" and FindSource did not. That
// was harmless while no empty tier read Asker; MG3-1b makes all four read it,
// and an unfilled asker then means a compound parent EXCLUDED FROM ITS OWN DIG
// — unable to see the carrier that dig uncovered for it, which is the exact
// arrest dig_exclusion.go was written about.
//
// A deliberate reversal reads as one only if what it reverses was written down.
func TestAskerIsFilledOnTheSimplePath(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(90, "ASK-DEST"))
	db.globalEmpty = nil

	order := &orders.Order{ID: 4242, DeliveryNode: "ASK-DEST", PayloadCode: "PANEL-A"}
	NewSourceFinder(db, nil, nil).FindSource(order, IntentEmpty)

	want := reservations.AskerFor(4242, 4242)
	if db.lastAsker != want {
		t.Errorf("asker handed to the search = %+v, want %+v. An order with no compound "+
			"parent owns lane locks on its own behalf", db.lastAsker, want)
	}
}

// AND A COMPOUND CHILD ASKS ON ITS PARENT'S BEHALF, which is the half that
// matters: in expose mode the lock is TRANSFERRED to the parent, so a child
// that asked only as itself would be shut out of the lane its own compound
// holds.
func TestAskerCarriesTheLaneOwnerForACompoundChild(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(91, "ASK-DEST-2"))

	parent := int64(7000)
	order := &orders.Order{ID: 7001, ParentOrderID: &parent,
		DeliveryNode: "ASK-DEST-2", PayloadCode: "PANEL-A"}
	NewSourceFinder(db, nil, nil).FindSource(order, IntentEmpty)

	want := reservations.AskerFor(7001, 7000)
	if db.lastAsker != want {
		t.Errorf("asker = %+v, want %+v — a child works inside a lane its parent locked",
			db.lastAsker, want)
	}
}
