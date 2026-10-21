package engine

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/engine/changeover"
	"shingoedge/store/processes"
)

// A KEEP-STAGED NODE IN THE TWO MODES WITH NO STAGING HOP.
//
// A sequential line's backfill and a press's refill leg fetch the line's fresh
// carrier from the market. With a keep-staged node named, they fetch it from
// the spot instead, and that is the only step that changes: the same step
// list, the inbound-source pickup replaced by the spot pickup (named for the
// claim's part, never Empty), the press's index moves untouched. Steady state
// and changeover, both roles, both flip states and both press geometries.

const everyModeSpot = "KS-SPOT"

func withSpot(c *processes.NodeClaim) *processes.NodeClaim {
	k := *c
	k.KeepStagedNode = everyModeSpot
	return &k
}

// assertOnlyTheFetchMoved compares a step list built for a claim with no spot
// against the same list built with one: exactly the inbound-source pickup is
// replaced, by the spot pickup.
func assertOnlyTheFetchMoved(t *testing.T, what string, plain, kept []protocol.ComplexOrderStep, claim *processes.NodeClaim) {
	t.Helper()
	if len(plain) != len(kept) {
		t.Fatalf("%s: %d steps with a spot, %d without:\n kept  %+v\n plain %+v", what, len(kept), len(plain), kept, plain)
	}
	moved := 0
	for i := range plain {
		if reflect.DeepEqual(plain[i], kept[i]) {
			continue
		}
		moved++
		if plain[i].Action != protocol.ActionPickup || plain[i].Node != claim.InboundSource {
			t.Errorf("%s: step %d moved and it is not the fetch: %+v -> %+v", what, i, plain[i], kept[i])
		}
		want := protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: everyModeSpot, PayloadCode: claim.PayloadCode}
		if kept[i] != want {
			t.Errorf("%s: step %d = %+v, want the spot pickup %+v", what, i, kept[i], want)
		}
	}
	if moved != 1 {
		t.Errorf("%s: %d steps moved, want the one fetch:\n kept  %+v\n plain %+v", what, moved, kept, plain)
	}
}

func TestKeepStagedNode_SequentialAndPressFetchFromTheSpot(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		// Sequential: the removal fetches nothing, so it is unchanged; the
		// backfill fetches from the spot.
		seq := goldenClaim(role, protocol.SwapModeSequential, "P-RUN", "", false)
		seq.InboundStaging, seq.OutboundStaging = "", ""
		what := string(role) + "/sequential"
		plainDisp, err := BuildSwapDispatch(node, seq)
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
		keptDisp, err := BuildSwapDispatch(node, withSpot(seq))
		if err != nil {
			t.Fatalf("%s with a spot: %v", what, err)
		}
		if !reflect.DeepEqual(plainDisp, keptDisp) {
			t.Errorf("%s: the removal changed with a spot named:\n %+v\n %+v", what, keptDisp, plainDisp)
		}
		assertOnlyTheFetchMoved(t, what+" backfill", BuildSequentialBackfillSteps(seq),
			BuildSequentialBackfillSteps(withSpot(seq)), seq)
		for _, parked := range []bool{false, true} {
			inactive := "PAIR-1"
			if parked {
				inactive = "LINE"
			}
			from := goldenClaim(role, protocol.SwapModeSequential, "P-FROM", "", false)
			from.InboundStaging, from.OutboundStaging = "", ""
			to := goldenClaim(role, protocol.SwapModeSequential, "P-TO", "", false)
			to.InboundStaging, to.OutboundStaging = "", ""
			assertOnlyTheFetchMoved(t, what+" changeover swap",
				BuildSwapChangeoverSteps(from, to, inactive, "").StepsA,
				BuildSwapChangeoverSteps(from, withSpot(to), inactive, "").StepsA, to)
			assertOnlyTheFetchMoved(t, what+" changeover evacuate",
				BuildEvacuateChangeoverSteps(from, to, inactive, "").StepsA,
				BuildEvacuateChangeoverSteps(from, withSpot(to), inactive, "").StepsA, to)
		}

		// Press-index: the leg that owns the fetch takes it from the spot; the
		// index moves and the leg that does not fetch are unchanged.
		for _, v := range goldenVariants(protocol.SwapModeTwoRobotPressIndex) {
			press := goldenClaim(role, protocol.SwapModeTwoRobotPressIndex, "P-RUN", v.secondPaired, v.flip)
			press.InboundStaging, press.OutboundStaging = "", ""
			pw := string(role) + "/press/" + v.name
			plain, err := BuildSwapDispatch(node, press)
			if err != nil {
				t.Fatalf("%s: %v", pw, err)
			}
			kept, err := BuildSwapDispatch(node, withSpot(press))
			if err != nil {
				t.Fatalf("%s with a spot: %v", pw, err)
			}
			fetching, still := "A", "B"
			plainFetch, keptFetch, plainStill, keptStill := plain.StepsA, kept.StepsA, plain.StepsB, kept.StepsB
			if v.flip {
				fetching, still = "B", "A"
				plainFetch, keptFetch, plainStill, keptStill = plain.StepsB, kept.StepsB, plain.StepsA, kept.StepsA
			}
			assertOnlyTheFetchMoved(t, pw+" leg "+fetching, plainFetch, keptFetch, press)
			if !reflect.DeepEqual(plainStill, keptStill) {
				t.Errorf("%s: leg %s changed with a spot named:\n %+v\n %+v", pw, still, keptStill, plainStill)
			}
			if plain.AutoConfirmA != kept.AutoConfirmA || plain.AutoConfirmB != kept.AutoConfirmB ||
				plain.DeliveryNodeA != kept.DeliveryNodeA {
				t.Errorf("%s: the receipts or the delivery node moved with a spot named", pw)
			}

			from := goldenClaim(role, protocol.SwapModeTwoRobotPressIndex, "P-FROM", v.secondPaired, v.flip)
			to := goldenClaim(role, protocol.SwapModeTwoRobotPressIndex, "P-TO", v.secondPaired, v.flip)
			for _, tooling := range []bool{false, true} {
				cw := pw + " changeover"
				if tooling {
					cw += " evacuate"
				}
				p := buildPressIndexChangeoverSwap(from, to, tooling)
				k := buildPressIndexChangeoverSwap(from, withSpot(to), tooling)
				if v.flip {
					assertOnlyTheFetchMoved(t, cw+" supply", p.Roles.supply.steps, k.Roles.supply.steps, to)
					if !reflect.DeepEqual(p.Roles.evac.steps, k.Roles.evac.steps) {
						t.Errorf("%s: the evac changed with a spot named", cw)
					}
				} else {
					assertOnlyTheFetchMoved(t, cw+" evac", p.Roles.evac.steps, k.Roles.evac.steps, to)
					if !reflect.DeepEqual(p.Roles.supply.steps, k.Roles.supply.steps) {
						t.Errorf("%s: the index changed with a spot named", cw)
					}
				}
			}
		}
	}
}

// THE SPOT COSTS NO ROUND TRIP, IN ANY MODE. A request reads its spot in the
// one bins-table read it already makes for the line (claimOccupancy), so a line
// that names a keep-staged node takes exactly the Core round trips the same
// line takes without one: on a line holding a bin and on a bare line, every
// swap mode, both roles. What the spot adds is its own orders.
func TestKeepStagedNode_TheSpotCostsNoRoundTrip(t *testing.T) {
	t.Parallel()
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			for _, bareLine := range []bool{false, true} {
				name := string(role) + "/" + string(mode)
				if bareLine {
					name += "/bare line"
				}
				t.Run(name, func(t *testing.T) {
					t.Parallel()
					trips := map[bool]int{}
					for _, kept := range []bool{false, true} {
						w := newButtonWorld(t, role, mode)
						if bareLine {
							w.bare(ksLine)
						}
						if kept {
							_, err := w.db.DB.Exec(`UPDATE style_node_claims SET keep_staged_node=?`, ksSpot)
							testutil.MustNoErr(t, err, "name the spot")
							// The spot is read whatever stands there; bare, so every
							// mode builds its swap rather than refusing one that would
							// stage on a bin the stub cannot name as a spare.
						}
						door := doorMaterial
						if role == protocol.ClaimRoleProduce {
							door = doorProduce
						}
						got := w.press(door)
						if got.refused != "" {
							t.Fatalf("kept=%v: refused: %s", kept, got.refused)
						}
						trips[kept] = got.trips
						t.Logf("kept=%v: %d Core round trip(s), %d leg(s), %d to the line", kept, got.trips, got.legs, got.toLine)
					}
					if trips[true] != trips[false] {
						t.Errorf("Core round trips: %d with a spot, %d without", trips[true], trips[false])
					}
				})
			}
		}
	}
}

// takesFromSpot reports whether a set of legs takes the spare standing on the
// spot: a pickup there with no leg of the set setting a carrier down there
// first. A pickup of what a leg parked there (a relay, or a single-robot swap
// collecting its stage order's carrier) does not.
func takesFromSpot(legs ...[]protocol.ComplexOrderStep) bool {
	var lift, drop bool
	for _, steps := range legs {
		d, l := spotSteps(steps, everyModeSpot)
		drop, lift = drop || d, lift || l
	}
	return lift && !drop
}

// ONE LEG DOOR. For the same read of the spot, a changeover's incoming leg is
// built from the claim requestClaim gives a REQUEST (legDiffs), and takes the
// spare standing there exactly when the REQUEST's swap does: a right spare
// that is not leaving, and nothing otherwise. Every configurable mode, every
// press geometry, both roles, the spot as inbound staging and not. The
// changeover leg never picks up at the spot unless requestClaim took the spare.
func TestKeepStagedNode_TheChangeoverLegTakesTheSpareAsRequestDoes(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	nodes := []processes.Node{*node}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		reads := map[string]spotRead{
			"bare":  {known: true},
			"wrong": {known: true, occupied: true, payload: "P-OTHER"},
		}
		if role == protocol.ClaimRoleConsume {
			reads["right"] = spotRead{known: true, occupied: true, payload: "P-TO"}
		} else {
			reads["right"] = spotRead{known: true, occupied: true}
		}
		for _, mode := range protocol.ConfigurableSwapModes() {
			noHop := mode == protocol.SwapModeSequential || mode == protocol.SwapModeTwoRobotPressIndex
			for _, v := range goldenVariants(mode) {
				for _, staging := range []bool{false, true} {
					if staging && noHop {
						continue // no staging hop in these modes
					}
					for name, read := range reads {
						what := string(role) + "/" + string(mode) + "/" + v.name + "/" + name
						if staging {
							what += "/spot==staging"
						}
						from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
						to := withSpot(goldenClaim(role, mode, "P-TO", v.secondPaired, v.flip))
						if noHop {
							from.InboundStaging, from.OutboundStaging = "", ""
							to.InboundStaging, to.OutboundStaging = "", ""
						}
						if staging {
							to.InboundStaging = everyModeSpot
						}

						planClaim, takes := requestClaim(to, to, spotAtRequest{read: read})
						d, err := BuildSwapDispatch(node, planClaim)
						if err != nil {
							t.Fatalf("%s: request: %v", what, err)
						}
						// A sequential line fetches in its backfill, not in the removal.
						reqLegs := [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB}
						if mode == protocol.SwapModeSequential {
							reqLegs = append(reqLegs, BuildSequentialBackfillSteps(planClaim))
						}
						if got := takesFromSpot(reqLegs...); got != takes {
							t.Errorf("%s: the request's swap takes the spare = %v, its decision says %v", what, got, takes)
						}

						diffs := legDiffs([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap,
							FromClaim: from, ToClaim: to}}, map[string]spotRead{everyModeSpot: read})
						if !reflect.DeepEqual(diffs[0].ToClaim, planClaim) {
							t.Errorf("%s: the changeover leg's claim %+v, the request's %+v", what, diffs[0].ToClaim, planClaim)
						}
						plan := BuildChangeoverPlan(diffs, nodes, false, nil, toolingChangeover{})
						var legs [][]protocol.ComplexOrderStep
						for _, a := range plan.Actions {
							if a.Err != nil {
								t.Fatalf("%s: changeover: %v", what, a.Err)
							}
							for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
								legs = append(legs, specSteps(spec))
							}
						}
						if got := takesFromSpot(legs...); got != takes {
							t.Errorf("%s: the changeover's legs take the spare = %v, the request's door says %v", what, got, takes)
						}
					}
				}
			}
		}
	}
}

// A RIGHT SPARE STARTS THE SWAP MID-ORDER, IN EVERY MODE (owner, 2026-10-16:
// "keep staged basically just makes robot swap modes start mid-order"). With a
// right spare standing, the fresh carrier is already where the swap would have
// staged it, so no leg sets a carrier down on the spot: there is one pickup
// there, naming the part, and the next step sets that carrier on the line side.
// The single robot lifts the line's bin first — old out, then spot to line.
// Steady swap and changeover, both roles, every press geometry; the spot is
// the inbound staging in the two modes that stage, a node of its own in the
// two that do not.
func TestKeepStagedNode_ARightSpareStartsMidOrder(t *testing.T) {
	t.Parallel()
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	nodes := []processes.Node{*node}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			noHop := mode == protocol.SwapModeSequential || mode == protocol.SwapModeTwoRobotPressIndex
			for _, v := range goldenVariants(mode) {
				what := string(role) + "/" + string(mode) + "/" + v.name
				kept := func(payload string) *processes.NodeClaim {
					c := withSpot(goldenClaim(role, mode, payload, v.secondPaired, v.flip))
					if noHop {
						c.InboundStaging, c.OutboundStaging = "", ""
					} else {
						c.InboundStaging = everyModeSpot
					}
					return c
				}
				run := kept("P-RUN")
				d, err := BuildSwapDispatch(node, run)
				if err != nil {
					t.Fatalf("%s: %v", what, err)
				}
				steady := [][]protocol.ComplexOrderStep{d.StepsA, d.StepsB}
				if mode == protocol.SwapModeSequential {
					steady = append(steady, BuildSequentialBackfillSteps(run))
				}
				assertMidOrder(t, what+" steady", steady, run.PayloadCode, mode)

				from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
				if noHop {
					from.InboundStaging, from.OutboundStaging = "", ""
				}
				to := kept("P-TO")
				plan := BuildChangeoverPlan([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap,
					FromClaim: from, ToClaim: to}}, nodes, false, nil, toolingChangeover{})
				var legs [][]protocol.ComplexOrderStep
				for _, a := range plan.Actions {
					legs = append(legs, specSteps(a.SupplyOrder), specSteps(a.EvacOrder))
				}
				assertMidOrder(t, what+" changeover", legs, to.PayloadCode, mode)
			}
		}
	}
}

func assertMidOrder(t *testing.T, what string, legs [][]protocol.ComplexOrderStep, part string, mode protocol.SwapMode) {
	t.Helper()
	pickups := 0
	for _, steps := range legs {
		liftedLine := false
		for i, s := range steps {
			if s.Action == protocol.ActionPickup && s.Node == "LINE" {
				liftedLine = true
			}
			if s.Node != everyModeSpot {
				continue
			}
			switch s.Action {
			case protocol.ActionDropoff:
				t.Errorf("%s: a leg sets a carrier down on the spot (step %d): the spare is already there", what, i)
			case protocol.ActionPickup:
				pickups++
				if s.PayloadCode != part || s.Empty {
					t.Errorf("%s: the spot pickup is %+v, want it naming %s", what, s, part)
				}
				if i+1 >= len(steps) || steps[i+1].Action != protocol.ActionDropoff ||
					steps[i+1].Node == "STG-IN" || steps[i+1].Node == "STG-OUT" {
					t.Errorf("%s: after the spot pickup comes %+v, want the set-down on the line side", what, steps[i+1:])
				}
				if mode == protocol.SwapModeSingleRobot && !liftedLine {
					t.Errorf("%s: the single robot lifts the spare before the line's bin: old out first", what)
				}
			}
		}
	}
	if pickups != 1 {
		t.Errorf("%s: %d pickups at the spot, want 1", what, pickups)
	}
}
