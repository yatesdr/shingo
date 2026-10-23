package engine

import (
	"slices"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// chain_readiness_test.go — what a press's dependent positions need before its
// swap can index from them, judged from what stands on them.

func readinessClaim(role protocol.ClaimRole, partialDest string) *processes.NodeClaim {
	return &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: protocol.SwapModeTwoRobotPressIndex, Role: role,
		PairedCoreNode: "B", PayloadCode: "PART", InboundSource: "IN", OutboundDestination: "OUT",
		PartialDestination: partialDest}
}

func intp(n int) *int { return &n }

// The carriers a position can hold, as the bins feed row says.
var (
	rowEmpty         = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 1}
	rowTopsAndBottom = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 2,
		TotesOnCart: intp(0), TotesPerLayer: intp(8)}
	rowMarker      = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 3, Bare: true}
	rowFullOfPart  = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 4, PayloadCode: "PART", Stocked: true}
	rowPartialPart = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 5, PayloadCode: "PART", Stocked: true, Partial: true}
	rowOtherPart   = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 6, PayloadCode: "OTHER", Stocked: true}
	rowPartAtZero  = protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 7, PayloadCode: "PART"}
)

// TestChainReadiness_JudgesTheCarrierOnTheOnDeck is every carrier on a press's
// on-deck, for both roles: what is cleared (and primed behind the clear), and
// what is left for the index to carry.
func TestChainReadiness_JudgesTheCarrierOnTheOnDeck(t *testing.T) {
	t.Parallel()
	occupied := map[string]bool{"LINE": true, "B": true}
	for _, c := range []struct {
		name        string
		role        protocol.ClaimRole
		partialDest string
		row         protocol.NodeBinsRow
		clear       bool
	}{
		{"a marker on a produce on-deck", protocol.ClaimRoleProduce, "", rowMarker, true},
		{"a marker on a consume on-deck", protocol.ClaimRoleConsume, "", rowMarker, true},
		{"an empty on a produce on-deck is what it indexes", protocol.ClaimRoleProduce, "", rowEmpty, false},
		{"tops & bottoms on a produce on-deck is what SEND BACK lands", protocol.ClaimRoleProduce, "", rowTopsAndBottom, false},
		{"a full of the part on a produce on-deck", protocol.ClaimRoleProduce, "", rowFullOfPart, true},
		{"another part on a produce on-deck", protocol.ClaimRoleProduce, "", rowOtherPart, true},
		{"the part's carrier at nothing on a produce on-deck", protocol.ClaimRoleProduce, "", rowPartAtZero, true},
		{"a partial of the part on a produce on-deck, no partial home", protocol.ClaimRoleProduce, "", rowPartialPart, true},
		{"a partial of the part on a produce on-deck with a partial home", protocol.ClaimRoleProduce, "PARTIALS", rowPartialPart, false},
		{"another part's partial on a produce on-deck with a partial home", protocol.ClaimRoleProduce, "PARTIALS",
			protocol.NodeBinsRow{Node: "B", State: protocol.BinsStateOccupied, Occupied: true, BinID: 8, PayloadCode: "OTHER", Stocked: true, Partial: true}, true},
		{"an empty on a consume on-deck", protocol.ClaimRoleConsume, "", rowEmpty, true},
		{"tops & bottoms on a consume on-deck", protocol.ClaimRoleConsume, "", rowTopsAndBottom, true},
		{"another part on a consume on-deck", protocol.ClaimRoleConsume, "", rowOtherPart, true},
		{"the part's carrier at nothing on a consume on-deck", protocol.ClaimRoleConsume, "", rowPartAtZero, true},
		{"a full of the part on a consume on-deck", protocol.ClaimRoleConsume, "", rowFullOfPart, false},
		{"a partial of the part on a consume on-deck runs", protocol.ClaimRoleConsume, "", rowPartialPart, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			r := ChainReadiness(readinessClaim(c.role, c.partialDest), occupied, Deck{Rows: map[string]protocol.NodeBinsRow{"B": c.row}})
			if c.clear {
				if len(r.Clear) != 1 || r.Clear[0].BinID != c.row.BinID || !slices.Equal(r.Prime, []string{"B"}) {
					t.Errorf("readiness %+v: want the carrier cleared and B primed behind it", r)
				}
				return
			}
			if len(r.Clear) != 0 || len(r.Prime) != 0 || len(r.Vacant) != 0 {
				t.Errorf("readiness %+v: want nothing, the index carries it", r)
			}
		})
	}
}

// TestChainReadiness_VacantUnknownAndComing: a vacant on-deck is primed unless
// a carrier is coming; an on-deck the table could not identify builds the swap
// (no clear, no prime); a wrong carrier with a leg placing there is not cleared
// out from under that leg.
func TestChainReadiness_VacantUnknownAndComing(t *testing.T) {
	t.Parallel()
	claim := readinessClaim(protocol.ClaimRoleProduce, "")
	vacant := map[string]bool{"LINE": true, "B": false}
	occupied := map[string]bool{"LINE": true, "B": true}

	if r := ChainReadiness(claim, vacant, Deck{}); !slices.Equal(r.Prime, []string{"B"}) || !slices.Equal(r.Vacant, []string{"B"}) {
		t.Errorf("vacant: %+v, want B primed", r)
	}
	if r := ChainReadiness(claim, vacant, Deck{Coming: map[string]bool{"B": true}}); len(r.Prime) != 0 || !slices.Equal(r.Vacant, []string{"B"}) {
		t.Errorf("vacant with a carrier coming: %+v, want no prime, still vacant", r)
	}
	for name, deck := range map[string]Deck{
		"no row":         {},
		"an unknown row": {Rows: map[string]protocol.NodeBinsRow{"B": {Node: "B", State: protocol.BinsStateUnknown}}},
		"missing from occupancy (reads occupied), row says empty": {Rows: map[string]protocol.NodeBinsRow{"B": {Node: "B", State: protocol.BinsStateEmpty}}},
	} {
		occ := occupied
		if name == "missing from occupancy (reads occupied), row says empty" {
			occ = map[string]bool{"LINE": true}
		}
		if r := ChainReadiness(claim, occ, deck); len(r.Clear)+len(r.Prime)+len(r.Vacant) != 0 {
			t.Errorf("%s: %+v, want nothing: unknown builds the swap", name, r)
		}
	}
	coming := Deck{Rows: map[string]protocol.NodeBinsRow{"B": rowMarker}, Coming: map[string]bool{"B": true}}
	if r := ChainReadiness(claim, occupied, coming); len(r.Clear) != 0 || len(r.Prime) != 0 {
		t.Errorf("a marker with a leg placing there: %+v, want no clear", r)
	}
}

// TestChainReadiness_OnlyAPressHasDependentPositions: a claim of any other mode
// names no dependent position, whatever its paired fields hold.
func TestChainReadiness_OnlyAPressHasDependentPositions(t *testing.T) {
	t.Parallel()
	claim := readinessClaim(protocol.ClaimRoleConsume, "")
	claim.SwapMode = protocol.SwapModeTwoRobot
	r := ChainReadiness(claim, map[string]bool{"LINE": true, "B": false}, Deck{Rows: map[string]protocol.NodeBinsRow{"B": rowMarker}})
	if len(r.Clear)+len(r.Prime)+len(r.Vacant) != 0 {
		t.Errorf("two_robot with a stale paired node: %+v, want nothing", r)
	}
}

// TestBuildPlans_ReadyOnDeckBuildsTheSwap: through both plans, an on-deck whose
// carrier is the index's (tops & bottoms on a produce press, a full of the part
// on a consume press) or unknown builds the swap and no chain; a wrong one
// builds the swap too, queued behind its clear and its prime (W6).
func TestBuildPlans_ReadyOnDeckBuildsTheSwap(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	runtime := &processes.RuntimeState{}
	occupied := map[string]bool{"LINE": true, "B": true}
	rows := func(r protocol.NodeBinsRow) Deck { return Deck{Rows: map[string]protocol.NodeBinsRow{"B": r}} }

	for name, deck := range map[string]Deck{"tops & bottoms": rows(rowTopsAndBottom), "unknown": {}} {
		plan, err := BuildProducePlan(node, runtime, readinessClaim(protocol.ClaimRoleProduce, ""), occupied, deck)
		if err != nil || plan.Dispatch == nil || plan.Chain.Count() != 0 {
			t.Errorf("produce, %s on the on-deck: plan %+v (%v), want the swap", name, plan, err)
		}
	}
	for name, deck := range map[string]Deck{"a full of the part": rows(rowFullOfPart), "unknown": {}} {
		plan, err := BuildConsumePlan(node, runtime, readinessClaim(protocol.ClaimRoleConsume, ""), 1, occupied, deck, false)
		if err != nil || plan.Dispatch == nil || plan.Chain.Count() != 0 {
			t.Errorf("consume, %s on the on-deck: plan %+v (%v), want the swap", name, plan, err)
		}
	}
	plan, err := BuildConsumePlan(node, runtime, readinessClaim(protocol.ClaimRoleConsume, ""), 1, occupied, rows(rowEmpty), false)
	if err != nil || plan.Dispatch == nil || len(plan.Chain.Clear) != 1 || len(plan.Chain.Prime) != 1 || plan.OrderCount() != 4 {
		t.Errorf("consume, an empty on the on-deck: plan %+v (%v), want B cleared and primed ahead of the swap (4 orders)", plan, err)
	}
}
