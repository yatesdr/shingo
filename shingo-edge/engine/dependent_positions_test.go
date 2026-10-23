package engine

import (
	"shingoedge/store/orders"
	"slices"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// dependent_positions_test.go — "which positions does this swap depend on".
//
// Nine sites wrote the same pair, "if the claim is press-index, its
// ExtensionPositions()", or the front plus that pair. Each site's answer is
// pinned here as it stood before NodeClaim.DependentPositions replaced the pair,
// over the claims that tell a gated answer from an ungated one: a 3- and a
// 2-position press, a two_robot claim carrying a stale paired node, and a
// sequential claim (which must name its partner in PairedCoreNode).

func staleTwoRobotClaim() *processes.NodeClaim {
	return &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: protocol.SwapModeTwoRobot,
		PairedCoreNode: "B", SecondPairedCoreNode: "C"}
}

func sequentialPartnerClaim() *processes.NodeClaim {
	return &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: protocol.SwapModeSequential, PairedCoreNode: "B"}
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// TestDependentPositionSites_AnswerAsAtTheBase pins every replaced site's answer.
func TestDependentPositionSites_AnswerAsAtTheBase(t *testing.T) {
	t.Parallel()
	press3, press2 := pressIndexClaim("LINE", "B", "C"), pressIndexClaim("LINE", "B", "")
	stale, seq := staleTwoRobotClaim(), sequentialPartnerClaim()

	t.Run("barePositions (vacant_line.go): a press's paired positions only", func(t *testing.T) {
		t.Parallel()
		vacant := map[string]bool{"LINE": false, "B": false, "C": false}
		for _, c := range []struct {
			name  string
			claim *processes.NodeClaim
			want  []string
		}{{"press3", press3, []string{"B", "C"}}, {"press2", press2, []string{"B"}}, {"stale two_robot", stale, nil}, {"sequential", seq, nil}} {
			lineBare, paired := barePositions(c.claim, vacant)
			if !lineBare || !slices.Equal(paired, c.want) {
				t.Errorf("%s: lineBare=%v paired=%v, want true %v", c.name, lineBare, paired, c.want)
			}
		}
	})

	t.Run("claimOccupancy (operator_stations.go): the names asked", func(t *testing.T) {
		t.Parallel()
		eng := testEngine(t, testEngineDB(t))
		for _, c := range []struct {
			name  string
			claim *processes.NodeClaim
			want  []string
		}{{"press3", press3, []string{"B", "C", "LINE"}}, {"stale two_robot", stale, []string{"LINE"}}, {"sequential", seq, []string{"LINE"}}} {
			occ, _, _ := eng.claimOccupancy(c.claim)
			if got := sortedKeys(occ); !slices.Equal(got, c.want) {
				t.Errorf("%s: asked %v, want %v", c.name, got, c.want)
			}
		}
	})

	t.Run("guardLineRequest (operator_guards.go, operator_produce.go): the inbound positions", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		id, err := db.CreateOrder("to-b", protocol.OrderTypeRetrieve, nil, true, 1, "B", "", "MARKET", "", false, "", "", "")
		testutil.MustNoErr(t, err, "create")
		testutil.MustNoErr(t, db.UpdateOrderStatus(id, string(protocol.StatusDispatched)), "status")
		node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
		for _, c := range []struct {
			name  string
			claim *processes.NodeClaim
			want  []string
		}{{"press3", press3, []string{"B"}}, {"stale two_robot", stale, nil}, {"sequential", seq, nil}} {
			inbound, err := eng.guardLineRequest(node, &processes.RuntimeState{}, c.claim)
			testutil.MustNoErr(t, err, c.name)
			var got []string
			for k, v := range inbound {
				if v {
					got = append(got, k)
				}
			}
			slices.Sort(got)
			if !slices.Equal(got, c.want) {
				t.Errorf("%s: inbound %v, want %v", c.name, got, c.want)
			}
		}
	})

	t.Run("buildParticipants (operator_changeover_plan.go): a press's positions are indexed over", func(t *testing.T) {
		t.Parallel()
		indexedOver := func(diffs []ChangeoverNodeDiff) []string {
			var out []string
			for _, p := range buildParticipants(diffs) {
				if p.Role == domain.ParticipantRoleIndexedOver {
					out = append(out, p.CoreNodeName)
				}
			}
			return out
		}
		if got := indexedOver([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: press3, ToClaim: stale}}); !slices.Equal(got, []string{"B", "C"}) {
			t.Errorf("press → stale two_robot: indexed over %v, want [B C]", got)
		}
		if got := indexedOver([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: stale, ToClaim: seq}}); len(got) != 0 {
			t.Errorf("stale two_robot → sequential: indexed over %v, want none", got)
		}
	})

	t.Run("pressHasBarePosition (changeover.go): the front and every extension are read", func(t *testing.T) {
		t.Parallel()
		d := ChangeoverNodeDiff{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: press3, ToClaim: press3}
		vacantAt := func(names ...string) func(string) NodeState {
			return func(n string) NodeState {
				if slices.Contains(names, n) {
					return NodeState{Seat: SeatEmpty}
				}
				return NodeState{Seat: SeatBin}
			}
		}
		for _, c := range []struct {
			vacant []string
			want   bool
		}{{nil, false}, {[]string{"LINE"}, true}, {[]string{"B"}, true}, {[]string{"C"}, true}} {
			if got := pressHasBarePosition(d, vacantAt(c.vacant...)); got != c.want {
				t.Errorf("vacant %v: %v, want %v", c.vacant, got, c.want)
			}
		}
	})

	t.Run("FanOutPressIndexCrossMode (changeover.go): a press's positions on either side", func(t *testing.T) {
		t.Parallel()
		situations := func(diffs []ChangeoverNodeDiff) map[string]ChangeoverSituation {
			out := map[string]ChangeoverSituation{}
			for _, d := range FanOutPressIndexCrossMode(diffs, nil)[len(diffs):] {
				out[d.CoreNodeName] = d.Situation
			}
			return out
		}
		got := situations([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: press3, ToClaim: stale}})
		if len(got) != 2 || got["B"] != SituationDrop || got["C"] != SituationDrop {
			t.Errorf("press → stale two_robot: %v, want B and C dropped", got)
		}
		got = situations([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: stale, ToClaim: press2}})
		if len(got) != 1 || got["B"] != SituationAdd {
			t.Errorf("stale two_robot → press2: %v, want B added", got)
		}
		if got = situations([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: stale, ToClaim: seq}}); len(got) != 0 {
			t.Errorf("stale two_robot → sequential: %v, want nothing", got)
		}
	})

	t.Run("pressIndexPositions (changeover_tooling.go): a press's whole geometry, or none", func(t *testing.T) {
		t.Parallel()
		if got := pressIndexPositions(press3); !slices.Equal(got, []string{"LINE", "B", "C"}) {
			t.Errorf("press3: %v", got)
		}
		for _, c := range []*processes.NodeClaim{stale, seq, nil} {
			if got := pressIndexPositions(c); got != nil {
				t.Errorf("%+v: %v, want nil", c, got)
			}
		}
	})
}

// TestRequestProduceSwap_DoubleTapWhileR1TravelsIsRefused pins why hasActiveSwap
// stays beside PlacingAt: they answer different questions. With R2 confirmed
// and R1 still travelling (it carries the old bin out and the backfill to
// INDEX-B), nothing is placing at the press, so "is a carrier coming to the
// line" says no; "is this cell mid-cycle" says yes, and a second pair would be
// a second refill to INDEX-B.
func TestRequestProduceSwap_DoubleTapWhileR1TravelsIsRefused(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _, _ := seedSwapClaim(t, db, protocol.SwapModeTwoRobotPressIndex, "")
	_, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	eng := testEngine(t, db)
	holdPlaces(t, eng, occupiedAt("PRESS", "INDEX-B"))
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "PRESS"}, {Name: "INDEX-B"}, {Name: "MARKET-EMPTIES"}})
	seedRuntimeUOP(t, db, nodeID)

	res, err := eng.RequestProduceSwap(nodeID, 0)
	testutil.MustNoErr(t, err, "first request")
	if res.OrderA == nil || res.OrderB == nil {
		t.Fatalf("first request: want the pair, got %+v", res)
	}
	testutil.MustNoErr(t, db.UpdateOrderStatus(res.OrderB.ID, string(protocol.StatusConfirmed)), "R2 confirmed")
	testutil.MustNoErr(t, db.UpdateOrderStatus(res.OrderA.ID, string(protocol.StatusInTransit)), "R1 travelling")

	if p, err := db.LiveOrderPlacing("PRESS"); err != nil || p != orders.PlacingNone {
		t.Fatalf("placing at PRESS = %v (%v), want none: the premise of the pin", p, err)
	}
	_, err = eng.RequestProduceSwap(nodeID, 0)
	if err == nil || !strings.Contains(err.Error(), "a swap is already in progress") {
		t.Errorf("second request: err = %v, want the in-progress refusal", err)
	}
}

// TestDependentPositions_AreTheBuildersNonLinePositions is DependentPositions'
// definition, pinned to the steady-state step builders (buildSwapDispatch) and
// not a call into them: for every mode they build, both roles, flipped and
// unflipped, the claim's own positions other than its line that the swap's
// steps lift from or set down on are exactly DependentPositions. Every claim
// names a paired and a second paired node, so a mode whose builders never
// touch them (a sequential partner, a stale pairing) must answer none.
func TestDependentPositions_AreTheBuildersNonLinePositions(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, mode := range []protocol.SwapMode{protocol.SwapModeSimple, protocol.SwapModeSingleRobot,
		protocol.SwapModeTwoRobot, protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential} {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
			for _, flipped := range []bool{false, true} {
				for _, second := range []string{"", "C"} {
					claim := &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: mode, Role: role,
						PairedCoreNode: "B", SecondPairedCoreNode: second, IndexRobotSupplies: flipped,
						InboundSource: "IN", InboundStaging: "IN-STG", OutboundStaging: "OUT-STG",
						OutboundDestination: "OUT", PayloadCode: "PART"}
					d, err := buildSwapDispatch(node, claim)
					testutil.MustNoErr(t, err, string(mode))
					positions := map[string]bool{}
					for _, p := range claim.Positions() {
						if p != claim.CoreNodeName {
							positions[p] = true
						}
					}
					worked := map[string]bool{}
					if d != nil {
						for _, s := range append(append([]protocol.ComplexOrderStep(nil), d.StepsA...), d.StepsB...) {
							if (s.Action == protocol.ActionPickup || s.Action == protocol.ActionDropoff) && positions[s.Node] {
								worked[s.Node] = true
							}
						}
					}
					want := claim.DependentPositions()
					slices.Sort(want)
					if got := sortedKeys(worked); !slices.Equal(got, want) {
						t.Errorf("%s %s flipped=%v second=%q: the steps work %v, DependentPositions = %v",
							mode, role, flipped, second, got, want)
					}
				}
			}
		}
	}
}
