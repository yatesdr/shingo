package dispatch

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
)

// outflow_typing_test.go — MG3-4. A press empty pull has no payload to reason
// from; the only fact available is where the carrier is GOING.

// ── The wire: the next dropoff reaches the need ─────────────────────────────

// nextDropoffNode names where a pickup's carrier is set down next — the FIRST
// dropoff at or after the step, because a multi-leg plan drops at several
// places and only the next one is about this carrier.
func TestOutflowTyping_NextDropoffIsTheNextOne(t *testing.T) {
	t.Parallel()
	steps := []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "P1"},
		{Action: protocol.ActionDropoff, Node: "D1"},
		{Action: protocol.ActionPickup, Node: "P2"},
		{Action: protocol.ActionDropoff, Node: "D2"},
	}
	for _, tc := range []struct {
		from int
		want string
	}{
		{0, "D1"},
		{1, "D1"},
		{2, "D2"},
		{3, "D2"},
	} {
		if got := nextDropoffNode(steps, tc.from); got != tc.want {
			t.Errorf("nextDropoffNode(from %d) = %q, want %q", tc.from, got, tc.want)
		}
	}
	// A plan whose tail has no dropoff yields blank, which every consumer reads
	// as "no destination fact" rather than as an error.
	if got := nextDropoffNode(steps[:1], 0); got != "" {
		t.Errorf("a pickup with no dropoff after it = %q, want blank", got)
	}
	// A DEFERRED dropoff — blank node, resolved after intake — is skipped rather
	// than returned as an empty answer that stops the search.
	deferred := []protocol.ComplexOrderStep{
		{Action: protocol.ActionPickup, Node: "P1"},
		{Action: protocol.ActionDropoff, Node: ""},
		{Action: protocol.ActionDropoff, Node: "D-REAL"},
	}
	if got := nextDropoffNode(deferred, 0); got != "D-REAL" {
		t.Errorf("with a deferred dropoff in the way = %q, want D-REAL", got)
	}
}

// ── The rule: exactly one, or nothing ───────────────────────────────────────

func TestOutflowTyping_OneEffectiveTypeIsTheWant(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(500, "OT-PRESS-POS"))
	db.effectiveBinTypes[500] = []*bins.BinType{{ID: 1, Code: "OT-45x58"}}

	got := NewSourceFinder(db, nil, nil).wantedBinType(SourceNeed{
		Intent: IntentEmpty, ProcessNode: "OT-PRESS-POS",
	})
	if got != "OT-45x58" {
		t.Errorf("wantedBinType = %q, want OT-45x58. One effective type at the destination "+
			"is a statement: this slot takes that and nothing else, so any other carrier "+
			"arrives and cannot be set down", got)
	}
}

// ZERO OR MANY LEAVE IT ALONE, and they are different facts with the same
// correct handling.
func TestOutflowTyping_ZeroOrManyChangesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		types []*bins.BinType
	}{
		{
			// Nobody has said what fits — the state of every position in every
			// plant today. Narrowing on the operator's behalf is making a
			// decision for them.
			name:  "zero — nobody has said",
			types: nil,
		},
		{
			// Several fit, and picking one here would be arbitrary. The cascade's
			// ordering already decides which carrier costs least to fetch.
			name: "many — several fit",
			types: []*bins.BinType{
				{ID: 1, Code: "OT-BIG"}, {ID: 2, Code: "OT-SMALL"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := newFakeFinderDB()
			db.addNode(pinNode(510, "OT-POS"))
			db.effectiveBinTypes[510] = tc.types

			got := NewSourceFinder(db, nil, nil).wantedBinType(SourceNeed{
				Intent: IntentEmpty, ProcessNode: "OT-POS",
			})
			if got != "" {
				t.Errorf("wantedBinType = %q, want blank", got)
			}
		})
	}
}

// A MAINTAIN ORIGIN STILL WINS. The pinned type comes off the episode and is
// never re-derived — that was MG2-4's whole argument, and outflow typing is a
// derivation.
func TestOutflowTyping_MaintainOriginStillWins(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	db.addNode(pinNode(520, "OT-POS-2"))
	db.effectiveBinTypes[520] = []*bins.BinType{{ID: 9, Code: "OT-POSITION-SAYS"}}
	db.maintainedType["11111111-2222-3333-4444-555555555555"] = "OT-EPISODE-SAYS"

	got := NewSourceFinder(db, nil, nil).wantedBinType(SourceNeed{
		Intent: IntentEmpty, ProcessNode: "OT-POS-2",
		OriginID: "11111111-2222-3333-4444-555555555555",
	})
	if got != "OT-EPISODE-SAYS" {
		t.Errorf("wantedBinType = %q, want the EPISODE's type. A pinned type is pinned; "+
			"re-deriving it at selection time is how the ask and the count come to disagree "+
			"about what the group is short of", got)
	}
}

// INERT WHEN NO DESTINATION IS KNOWN, which is every need that carries no
// process node — the state of every complex leg before MG3-4 threaded one.
func TestOutflowTyping_NoDestinationIsNoOpinion(t *testing.T) {
	t.Parallel()
	db := newFakeFinderDB()
	if got := NewSourceFinder(db, nil, nil).wantedBinType(SourceNeed{Intent: IntentEmpty}); got != "" {
		t.Errorf("wantedBinType = %q, want blank", got)
	}
}
