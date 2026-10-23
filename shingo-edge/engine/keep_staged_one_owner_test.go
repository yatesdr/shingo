package engine

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/engine/changeover"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// ONE REFILL OWNER, THROUGH A CHANGEOVER. The keeper is the only thing that
// refills a keep-staged spot (keptFor): a changeover's start and cancel send
// back a spare the spot is no longer kept for and order nothing else, and the
// incoming leg takes the spare only where a REQUEST would (legDiffs). These
// drive the start, the keeper's three decision points (the sweep, a lift at the
// spot, an order's end) and the cancel end to end against a Core stub.

// restubSpot makes Core answer the line and the spot as given.
func restubSpot(t *testing.T, fx *coFixture, line, spot protocol.NodeBinInfo) {
	t.Helper()
	rows := map[string]protocol.NodeBinInfo{"L1": line, pinSpot: spot}
	bridgeCore(t, fx.eng, pinNodeBinsStub(t, rows, nil).URL)
	fx.holdRows(t, rows)
}

// changeoverLegs is the live complex legs of the line.
func changeoverLegs(t *testing.T, fx *coFixture) []domain.Order {
	t.Helper()
	var out []domain.Order
	for _, o := range mustActive(t, fx) {
		if o.OrderType == orders.TypeComplex {
			out = append(out, o)
		}
	}
	return out
}

// legsTakeTheSpare reports whether the live legs pick up at the spot without
// one of them parking a carrier there first.
func legsTakeTheSpare(t *testing.T, fx *coFixture) bool {
	t.Helper()
	var lift, drop bool
	for _, o := range changeoverLegs(t, fx) {
		steps, err := fx.eng.storedStepsOf(o.ID)
		testutil.MustNoErr(t, err, "steps")
		d, l := spotSteps(steps, pinSpot)
		drop, lift = drop || d, lift || l
	}
	return lift && !drop
}

// landLegs confirms every live leg as Core's confirm does, running the
// completion cascade and with it the keeper's order-end kick.
func landLegs(t *testing.T, fx *coFixture) {
	t.Helper()
	for _, o := range changeoverLegs(t, fx) {
		land(t, fx, o.ID)
	}
}

// A BARE SPOT IS REFILLED BEHIND THE LEG, NOT BY THE START. Two robots, the
// spot as inbound staging, nothing on it: the start builds the market swap,
// which parks its carrier on the spot, and orders no refill. The keeper waits
// while that leg still brings a carrier there, and refills when it ends.
func TestKeepStagedOneOwner_ABareSpotIsRefilledBehindTheLeg(t *testing.T) {
	t.Parallel()
	c := pinCase{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeTwoRobot, read: readBare, staging: true}
	fx := c.seed(t)
	withCascade(fx)
	startKSChangeover(t, fx)
	if legsTakeTheSpare(t, fx) {
		t.Fatal("a leg picks up at a bare spot")
	}
	if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 0 {
		t.Fatalf("the start ordered %d refill(s), want none", len(refills))
	}

	fx.eng.sweepCellLevels()
	if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 0 {
		t.Fatalf("the keeper refilled while the leg still brings a carrier to the spot")
	}
	if note := fx.eng.KeepStagedNote("L1"); !strings.Contains(note, "still bringing") {
		t.Errorf("board note = %q, want it to say the leg is still bringing a bin", note)
	}

	restubSpot(t, fx, protocol.NodeBinInfo{Occupied: true}, protocol.NodeBinInfo{}) // the leg collected what it parked
	landLegs(t, fx)
	refills, _ := liveSpot(t, fx, pinSpot)
	if len(refills) != 1 || refills[0].PayloadCode != "PART-NEW" || !refills[0].RetrieveEmpty {
		t.Fatalf("refills after the leg ended = %+v, want one empty for PART-NEW", refills)
	}
}

// A RIGHT SPARE: THE LEG TAKES IT, AND THE KEEPER REFILLS AT THE LIFT. And the
// wedge the old lockout left: a spare taken off the spot by hand before the
// robot arrives is refilled by the next sweep, for the incoming claim, so the
// leg still finds one.
func TestKeepStagedOneOwner_ARightSpareIsRefilledAtTheLift(t *testing.T) {
	t.Parallel()
	c := pinCase{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, read: readRight, staging: true}

	t.Run("lifted by the leg", func(t *testing.T) {
		t.Parallel()
		fx := c.seed(t)
		withCascade(fx)
		startKSChangeover(t, fx)
		if !legsTakeTheSpare(t, fx) {
			t.Fatal("no leg takes the right spare standing on the spot")
		}
		if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 0 {
			t.Fatalf("the start ordered %d refill(s), want none", len(refills))
		}
		var lifter *domain.Order
		for _, o := range changeoverLegs(t, fx) {
			steps, err := fx.eng.storedStepsOf(o.ID)
			testutil.MustNoErr(t, err, "steps")
			if _, l := spotSteps(steps, pinSpot); l {
				lifter = &o
			}
		}
		testutil.MustNoErr(t, fx.db.UpdateOrderStatus(lifter.ID, string(protocol.StatusInTransit)), "with the fleet")
		restubSpot(t, fx, protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}, protocol.NodeBinInfo{})
		fx.eng.HandleBinPickedUp(lifter.UUID, 7, pinSpot)
		refills, _ := liveSpot(t, fx, pinSpot)
		if len(refills) != 1 || refills[0].PayloadCode != "PART-NEW" || refills[0].RetrieveEmpty {
			t.Fatalf("refills after the lift = %+v, want one full of PART-NEW", refills)
		}
	})

	t.Run("taken by hand before the robot came", func(t *testing.T) {
		t.Parallel()
		fx := c.seed(t)
		startKSChangeover(t, fx)
		restubSpot(t, fx, protocol.NodeBinInfo{Occupied: true, PayloadCode: "PART-OLD"}, protocol.NodeBinInfo{})
		fx.eng.sweepCellLevels()
		refills, _ := liveSpot(t, fx, pinSpot)
		if len(refills) != 1 || refills[0].PayloadCode != "PART-NEW" {
			t.Fatalf("refills after the sweep = %+v, want one of PART-NEW for the leg to take", refills)
		}
	})
}

// THE 08:31 SHAPE (SPR ALN_011, 2026-10-15). A consume incoming claim, an
// unstamped empty on the spot, the spot as the inbound staging. The empty is
// wrong for the claim: the start sends it back naming its bin, and the leg is
// the market swap parking its carrier on the spot behind that return. No leg
// picks the empty up, the start orders no refill, and the keeper refills only
// once the leg is done with the spot.
func TestKeepStagedOneOwner_TheWrongSpareGoesBackAndTheLegNeverTakesIt(t *testing.T) {
	t.Parallel()
	empty := protocol.NodeBinInfo{Occupied: true, BinID: 137}
	c := pinCase{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, read: "empty-0831", staging: true, spotRow: &empty}
	fx := c.seed(t)
	withCascade(fx)
	startKSChangeover(t, fx)

	if legsTakeTheSpare(t, fx) {
		t.Fatal("a leg picks up the wrong spare: the 08:31 shape")
	}
	refills, returns := liveSpot(t, fx, pinSpot)
	if len(returns) != 1 || returns[0].DeliveryNode != "SRC-OLD" {
		t.Fatalf("returns = %+v, want the empty back to SRC-OLD", returns)
	}
	// The return names the bin it is for on the wire (keep_staged_return_bin_test.go).
	if len(refills) != 0 {
		t.Fatalf("the start ordered %d refill(s), want none", len(refills))
	}

	fx.eng.sweepCellLevels()
	if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 0 {
		t.Fatalf("the keeper refilled while the leg still parks a carrier on the spot")
	}

	restubSpot(t, fx, protocol.NodeBinInfo{Occupied: true}, protocol.NodeBinInfo{})
	land(t, fx, returns[0].ID)
	landLegs(t, fx)
	refills, _ = liveSpot(t, fx, pinSpot)
	if len(refills) != 1 || refills[0].PayloadCode != "PART-NEW" {
		t.Fatalf("refills after the leg ended = %+v, want one of PART-NEW", refills)
	}
}

// A START THAT ABORTS THE KEEPER'S QUEUED REFILL LEAVES IT ARMED. The start's
// legs are written in the abort's own second, so they cannot re-arm the
// keeper; the start writes the resume stamp, as REQUEST does
// (rearmKeptLines). Here the stamps are the abort's second, as at the plant.
func TestKeepStagedOneOwner_AStartThatAbortsARefillLeavesTheKeeperArmed(t *testing.T) {
	t.Parallel()
	c := pinCase{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeTwoRobot, read: readBare, queuedRef: true}
	fx := c.seed(t)
	queueKeeperRefill(t, fx)
	startKSChangeover(t, fx)
	sameSecondAsTheAbort(t, fx)

	if pinPaused(t, fx, fx.toStyleID) {
		t.Fatal("the keeper reads paused after a START that aborted its queued refill")
	}
	fx.eng.sweepCellLevels()
	if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 1 {
		t.Fatalf("refills after the sweep = %d, want 1: the keeper refills the bare spot", len(refills))
	}
}

// AN UNKNOWN SPOT REFUSES START, NAMED, AND CANCELS NOTHING. Without the read
// the start cannot say whether its leg takes the spare, as a REQUEST cannot.
func TestKeepStagedOneOwner_AnUnknownSpotRefusesStart(t *testing.T) {
	t.Parallel()
	c := pinCase{role: protocol.ClaimRoleProduce, mode: protocol.SwapModeTwoRobot, read: readUnknown, staging: true}
	fx := c.seed(t)
	queueKeeperRefill(t, fx)

	co, err := fx.eng.StartProcessChangeover(fx.processID, fx.toStyleID, "test", "keep-staged")
	if err == nil || co != nil {
		t.Fatalf("START with the spot unread = %+v, %v; want a refusal", co, err)
	}
	if !strings.Contains(err.Error(), pinSpot) {
		t.Errorf("the refusal %q does not name the spot", err)
	}
	if refills, _ := liveSpot(t, fx, pinSpot); len(refills) != 1 {
		t.Errorf("live refills after the refusal = %d, want the queued one untouched", len(refills))
	}
	if _, err := fx.eng.PreviewChangeoverPlan(fx.processID, fx.toStyleID); err == nil || !strings.Contains(err.Error(), pinSpot) {
		t.Errorf("Preview with the spot unread = %v, want the same refusal", err)
	}
}

// PREVIEW SHOWS THE LEGS START BUILDS. For the same read, every mode, both roles
// and the spot as staging or not, the complex legs Preview plans and the ones
// Start creates have the same steps.
func TestKeepStagedOneOwner_PreviewIsStart(t *testing.T) {
	t.Parallel()
	for _, c := range pinCases() {
		if c.add || c.read == readUnknown {
			continue
		}
		t.Run(c.name(), func(t *testing.T) {
			t.Parallel()
			pv := c.seed(t)
			plan, err := pv.eng.PreviewChangeoverPlan(pv.processID, pv.toStyleID)
			testutil.MustNoErr(t, err, "preview")
			var want []string
			for _, a := range plan.Actions {
				for _, spec := range []*changeover.OrderSpec{a.SupplyOrder, a.EvacOrder} {
					if steps := specSteps(spec); steps != nil {
						want = append(want, fmt.Sprintf("%+v", steps))
					}
				}
			}
			fx := c.seed(t)
			startKSChangeover(t, fx)
			var got []string
			for _, o := range changeoverLegs(t, fx) {
				steps, err := fx.eng.storedStepsOf(o.ID)
				testutil.MustNoErr(t, err, "steps")
				got = append(got, fmt.Sprintf("%+v", steps))
			}
			sort.Strings(want)
			sort.Strings(got)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Errorf("Start built\n  %s\nPreview showed\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

// refillSpot HAS ONE CALLER, the keeper's applySpotPlan. A second caller is a
// second refill owner, which is what ordered two refills onto a one-bin spot at
// SPR ALN_011 on 2026-10-15.
func TestArch_RefillSpotHasOneCaller(t *testing.T) {
	t.Parallel()
	root := edgeRoot(t)
	call := regexp.MustCompile(`\.refillSpot\(`)
	var sites []string
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "//") {
				continue
			}
			if call.MatchString(line) {
				sites = append(sites, filepath.ToSlash(rel)+":"+itoaLine(i+1)+": "+strings.TrimSpace(line))
			}
		}
		return nil
	})
	testutil.MustNoErr(t, err, "walk")
	if len(sites) != 1 || !strings.Contains(sites[0], "engine/keep_staged_spot.go") ||
		!strings.Contains(sites[0], "e.refillSpot(node, c, plan.refills, origin)") {
		t.Errorf("refillSpot is called from:\n  %s\nwant one call, the keeper's applySpotPlan", strings.Join(sites, "\n  "))
	}
}

// WHICH SHAPES ADVANCE THE CLAIM AT THE SWAP ORDER'S END. applyOrderBComplex
// re-points the line's claim when the task has no next-material order and the
// order sets a carrier down on the line. That is the single-robot changeover,
// Swap and Evacuate, both roles, with or without a right spare on the spot:
// one order (since 2026-10-16) that brings the incoming carrier itself. Every
// other mode links a next-material order (whose completion advances the claim),
// and a Drop sets nothing down on the line. A builder that adds a shape to this
// set has to say what the pointer does for it.
func TestClaimAdvance_OnlyTheSingleRobotOrderReachesTheSwapArm(t *testing.T) {
	t.Parallel()
	node := processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			noHop := mode == protocol.SwapModeSequential || mode == protocol.SwapModeTwoRobotPressIndex
			for _, v := range goldenVariants(mode) {
				for _, spare := range []bool{false, true} {
					for _, sit := range []ChangeoverSituation{SituationSwap, SituationEvacuate, SituationDrop, SituationAdd} {
						from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
						to := goldenClaim(role, mode, "P-TO", v.secondPaired, v.flip)
						if noHop {
							from.InboundStaging, from.OutboundStaging, to.InboundStaging, to.OutboundStaging = "", "", "", ""
						}
						if spare {
							to.KeepStagedNode = "KS-SPOT"
							if !noHop {
								to.KeepStagedNode = to.InboundStaging
							}
						}
						d := ChangeoverNodeDiff{CoreNodeName: "LINE", Situation: sit, FromClaim: from, ToClaim: to}
						switch sit {
						case SituationDrop:
							d.ToClaim = nil
						case SituationAdd:
							d.FromClaim = nil
						}
						plan := BuildChangeoverPlan([]ChangeoverNodeDiff{d}, []processes.Node{node}, false, nil, toolingChangeover{})
						for _, a := range plan.Actions {
							reaches := a.SupplyOrder == nil && a.EvacOrder != nil && protocol.PlacesBinAt(specSteps(a.EvacOrder), "LINE")
							want := mode == protocol.SwapModeSingleRobot && (sit == SituationSwap || sit == SituationEvacuate)
							if reaches != want {
								t.Errorf("%s/%s/%s spare=%v %s: reaches the swap order's claim advance = %v, want %v",
									role, mode, v.name, spare, sit, reaches, want)
							}
						}
					}
				}
			}
		}
	}
}

// The single-robot changeover with a right spare standing: the one order lifts
// the spare off the spot and sets it on the line, and its end points the
// line's claim at the incoming claim. Before 2026-10-16 nothing did: no
// next-material order was linked, so the node stayed on the outgoing claim
// (the ALN_007 stale pointer).
func TestClaimAdvance_SingleRobotRightSpare(t *testing.T) {
	t.Parallel()
	c := pinCase{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeSingleRobot, read: readRight, staging: true}
	fx := c.seed(t)
	withCascade(fx)
	startKSChangeover(t, fx)
	legs := changeoverLegs(t, fx)
	if len(legs) != 1 {
		t.Fatalf("legs = %d, want the one order", len(legs))
	}
	land(t, fx, legs[0].ID)
	rt, err := fx.db.GetProcessNodeRuntime(fx.nodeIDs["L1"])
	testutil.MustNoErr(t, err, "runtime")
	to, err := fx.db.GetStyleNodeClaimByNode(fx.toStyleID, "L1")
	testutil.MustNoErr(t, err, "incoming claim")
	if rt.ActiveClaimID == nil || *rt.ActiveClaimID != to.ID {
		t.Fatalf("the line's claim is %v after the order ended, want the incoming claim %d", rt.ActiveClaimID, to.ID)
	}
}
