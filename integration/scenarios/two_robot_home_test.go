// A two-robot line swapping through its part's home on a dedicated-positions
// loader, carried end to end across the two modules (brief G, Part A).
//
// The line is the one a plant asks for with no staging: a one-node consume line
// whose inbound source, inbound staging and outbound destination are all the
// part's pinned home on an operator-driven loader. The supply robot waits at
// the home's full; the removal robot waits at the line's cart; one RELEASE
// moves both, and the line's emptied cart goes back onto the home the full has
// just left, to be loaded.
//
// These sit beside the sequential line's pins through the same home
// (sequential_line_test.go, TestScenario_SequentialLine_SwapsThroughItsOwnHome)
// and use the same cell (keep_staged_recovery_test.go, ksrOpts.home), in a file
// of their own because they drive the two-robot pair, its one release and its
// two robots, which the sequential helpers do not.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"shingo/protocol"

	"shingocore/plantspec"
	corebins "shingocore/store/bins"
	coreorders "shingocore/store/orders"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
	edgestore "shingoedge/store"
)

// homeLine is the cell options for the two-robot line through its home: the
// loader with the given buffers, the first held of them holding an empty.
func homeLine(buffers, held int) ksrOpts {
	return ksrOpts{role: protocol.ClaimRoleConsume, mode: protocol.SwapModeTwoRobot, plain: true, bareSpot: true,
		home: true, stagedAtHome: true, nodeList: true, buffers: buffers, heldBuffers: held}
}

// stops is a plan as action(node) words.
func stops(steps []protocol.ComplexOrderStep) []string {
	var out []string
	for _, s := range steps {
		out = append(out, s.Action+"("+s.Node+")")
	}
	return out
}

// coreStops is the plan Core holds for the order, as action(node) words.
func (c *ksrCell) coreStops(o *coreorders.Order) []string {
	c.t.Helper()
	var steps []protocol.ComplexOrderStep
	mustNil(c.t, json.Unmarshal([]byte(o.StepsJSON), &steps), "decode Core's plan")
	return stops(steps)
}

// pairLegs is the two legs a REQUEST made, the supply (bound for the line)
// first.
func (c *ksrCell) pairLegs(label string, before int64) (supply, evac domain.Order) {
	c.t.Helper()
	var legs []domain.Order
	for _, o := range c.newRows(before) {
		if o.OrderType == protocol.OrderTypeComplex {
			legs = append(legs, o)
		}
	}
	if len(legs) != 2 {
		c.dump(label)
		c.t.Fatalf("%s: %d swap legs, want 2", label, len(legs))
	}
	if legs[1].DeliveryNode == ksrLine {
		legs[0], legs[1] = legs[1], legs[0]
	}
	return legs[0], legs[1]
}

// The brief's A0: does the claim save through every door a plant's claim meets,
// and are the legs the Edge builds and Core stores
// supply wait(H) pickup(H) dropoff(L) and evac wait(L) pickup(L) dropoff(H)?
//
// The claim is saved through the Edge's own save (Engine.SaveFlow: the
// validator and the store's write) with Core's node list as Core sends it, so
// the leg rule reads real kinds; a market slot named as the source is refused
// by the same kinds, so they are live. plantspec validates the sim plant's
// two-robot home line reshaped to this claim.
func TestScenario_TwoRobotLine_ThroughItsHome_SavesAndBuildsItsLegs(t *testing.T) {
	t.Parallel()
	c := newKsrCell(t, homeLine(0, 0))
	c.settle()

	// ── the save ──
	kinds := c.edge.Engine.CoreNodeKinds()
	if kinds.Empty() || !kinds.Known[ksrHomeA] {
		t.Fatalf("the Edge holds no node list from Core naming %s (kinds empty=%v): the save would refuse nothing",
			ksrHomeA, kinds.Empty())
	}
	if g := kinds.GroupOf(ksrHomeA); g != "" {
		t.Fatalf("Core's node list names %s a member of %s", ksrHomeA, g)
	}
	if g := kinds.GroupOf(ksrMktA + "-1"); g != ksrMktA {
		t.Fatalf("Core's node list does not name the market slot a member of its group (got %q): the control "+
			"below would prove nothing", g)
	}
	t.Logf("Core's node list at the Edge: %d names, plant-wide=%v, %d group members", len(kinds.Known),
		kinds.PlantWide, len(kinds.MemberOf))
	save := func(edit func(*domain.FlowCell)) error {
		claims, err := c.edge.DB.ListStyleNodeClaims(c.styleA)
		mustNil(t, err, "style A's claims")
		var cells []domain.FlowCell
		for _, cl := range claims {
			cell := domain.Collapse(cl)
			if edit != nil {
				edit(&cell)
			}
			cells = append(cells, cell)
		}
		fp, err := c.edge.Engine.FlowFingerprint(c.processID, c.styleA)
		mustNil(t, err, "fingerprint")
		_, err = c.edge.Engine.SaveFlow(c.processID, edgeengine.FlowSaveRequest{Source: domain.ClaimSourceAdmin,
			CalledBy: "g-line", ToStyleID: c.styleA, Cells: cells, Fingerprint: fp})
		return err
	}
	if err := save(nil); err != nil {
		t.Fatalf("the claim did not save through the Edge's save with Core's node list: %v", err)
	}
	err := save(func(cell *domain.FlowCell) { cell.InboundSource = ksrMktA + "-1" })
	var fve *edgeengine.FlowValidationError
	if !errors.As(err, &fve) || !strings.Contains(err.Error(), protocol.MsgMemberIsCoresToPick) {
		t.Fatalf("control: a market slot as the inbound source saved (%v); the kinds refuse nothing", err)
	}
	claim, err := c.edge.DB.GetStyleNodeClaimByNode(c.styleA, ksrLine)
	mustNil(t, err, "the saved claim")
	if claim.SwapMode != protocol.SwapModeTwoRobot || claim.InboundSource != ksrHomeA ||
		claim.InboundStaging != ksrHomeA || claim.OutboundDestination != ksrHomeA || claim.OutboundStaging != "" ||
		claim.KeepStagedNode != "" {
		t.Fatalf("the saved claim reads %s source=%q staging=%q dest=%q outbound staging=%q spot=%q", claim.SwapMode,
			claim.InboundSource, claim.InboundStaging, claim.OutboundDestination, claim.OutboundStaging,
			claim.KeepStagedNode)
	}
	in := domain.InputFromClaim(*claim)
	for _, f := range domain.ValidateNodeClaim(in, domain.ClaimNodeContext{Checked: true, Kinds: kinds,
		KnownCoreNodes: kinds.Known}) {
		t.Logf("ValidateNodeClaim on the saved claim: %s %s: %s", f.Severity, f.Field, f.Message)
		if f.Severity == domain.SeverityError {
			t.Errorf("ValidateNodeClaim refuses the saved claim: %s: %s", f.Field, f.Message)
		}
	}

	// ── plantspec: the sim plant's two-robot home line, reshaped to this claim ──
	p, err := plantspec.Load("../../plants/master.yaml")
	mustNil(t, err, "load plants/master.yaml")
	at := slices.IndexFunc(p.Claims, func(cl plantspec.Claim) bool {
		return cl.CoreNode == "ALN_008" && cl.SwapMode == string(protocol.SwapModeTwoRobot) && cl.InboundSource == "PLK_H1"
	})
	if at < 0 {
		t.Fatal("plants/master.yaml has no two-robot ALN_008 claim sourcing PLK_H1")
	}
	p.Claims[at].InboundStaging, p.Claims[at].OutboundStaging = "PLK_H1", ""
	if err := p.Validate(); err != nil {
		t.Errorf("plantspec refuses the two-robot line with no staging through its home: %v", err)
	}

	// ── the legs the Edge builds ──
	wantSupply := []string{"wait(" + ksrHomeA + ")", "pickup(" + ksrHomeA + ")", "dropoff(" + ksrLine + ")"}
	wantEvac := []string{"wait(" + ksrLine + ")", "pickup(" + ksrLine + ")", "dropoff(" + ksrHomeA + ")"}
	a, b := edgeengine.BuildTwoRobotSwapSteps(claim)
	t.Logf("BuildTwoRobotSwapSteps: supply %v, evac %v", stops(a), stops(b))
	if !slices.Equal(stops(a), wantSupply) || !slices.Equal(stops(b), wantEvac) {
		t.Errorf("BuildTwoRobotSwapSteps: supply %v want %v; evac %v want %v", stops(a), wantSupply, stops(b), wantEvac)
	}

	// ── the legs Core stores, after dispatch ──
	mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
	c.settle()
	before := c.lastID()
	c.operatorRequest()
	c.tick()
	supply, evac := c.pairLegs("the request", before)
	t.Logf("the Edge's rows: supply %v, evac %v", stops(c.legSteps(supply)), stops(c.legSteps(evac)))
	c.eventually("both legs with the fleet", func() bool {
		return c.coreOf(supply).VendorOrderID != "" && c.coreOf(evac).VendorOrderID != ""
	})
	for _, l := range []struct {
		name string
		o    domain.Order
		want []string
	}{{"supply", supply, wantSupply}, {"evac", evac, wantEvac}} {
		co := c.coreOf(l.o)
		t.Logf("Core's %s, order %d: %s -> %s status=%s cause=%q sibling=%q steps_json=%s", l.name, co.ID,
			co.SourceNode, co.DeliveryNode, co.Status, co.QueueCause, co.SiblingOrderUUID, co.StepsJSON)
		if got := c.coreStops(co); !slices.Equal(got, l.want) {
			t.Errorf("Core stores the %s as %v, want %v", l.name, got, l.want)
		}
	}
}

// homeLineStaged presses REQUEST on the line (its cart part-used, 30 of 40)
// and brings both robots to their waits: the supply at the home's full, the
// evac at the line's cart. It checks the plans Core stores are A0's.
func (c *ksrCell) homeLineStaged(label string) (supply, evac domain.Order) {
	c.t.Helper()
	mustNil(c.t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
	c.settle()
	before := c.lastID()
	c.operatorRequest()
	c.tick()
	supply, evac = c.pairLegs(label, before)
	c.eventually(label+": both legs with the fleet", func() bool {
		return c.coreOf(supply).VendorOrderID != "" && c.coreOf(evac).VendorOrderID != ""
	})
	wantSupply := []string{"wait(" + ksrHomeA + ")", "pickup(" + ksrHomeA + ")", "dropoff(" + ksrLine + ")"}
	wantEvac := []string{"wait(" + ksrLine + ")", "pickup(" + ksrLine + ")", "dropoff(" + ksrHomeA + ")"}
	for _, l := range []struct {
		name string
		o    domain.Order
		want []string
	}{{"supply", supply, wantSupply}, {"evac", evac, wantEvac}} {
		co := c.coreOf(l.o)
		c.t.Logf("%s: Core's %s, order %d: %s -> %s status=%s cause=%q: %v", label, l.name, co.ID, co.SourceNode,
			co.DeliveryNode, co.Status, co.QueueCause, c.coreStops(co))
		if got := c.coreStops(co); !slices.Equal(got, l.want) {
			c.t.Errorf("%s: Core stores the %s as %v, want %v", label, l.name, got, l.want)
		}
	}
	c.drive(c.coreOf(supply), "RUNNING", "WAITING")
	c.drive(c.coreOf(evac), "RUNNING", "WAITING")
	c.eventually(label+": both legs staged at the Edge", func() bool {
		return c.edgeRow(supply.ID).Status == protocol.StatusStaged && c.edgeRow(evac.ID).Status == protocol.StatusStaged
	})
	return supply, evac
}

// fleetBlocks is what Core has sent the fleet for the order: its blocks as
// action(location), and whether the order is complete.
func (c *ksrCell) fleetBlocks(o domain.Order) (blocks []string, complete bool) {
	c.t.Helper()
	v := c.core.sim.GetOrder(c.coreOf(o).VendorOrderID)
	if v == nil {
		return nil, false
	}
	for _, b := range v.Blocks {
		blocks = append(blocks, b.Action+"("+b.Location+")")
	}
	return blocks, v.Complete
}

// labels is the bins' labels, sorted.
func labels(bs []*corebins.Bin) []string {
	var out []string
	for _, b := range bs {
		out = append(out, b.Label)
	}
	slices.Sort(out)
	return out
}

// The brief's A1: the line swaps through its home on one RELEASE, with the
// loader holding no buffer, a free buffer, and a buffer holding an empty. The
// home holds a full, the line a part-used cart, two robots. Pinned as it runs
// at 9c282051: in every cell the park keeps the old cart's drop on the home
// (the full standing there is the one its sibling lifts), the one RELEASE sends
// both legs on at once, and the line ends with the full and the home with the
// old cart; the buffers are not touched.
func TestScenario_TwoRobotLine_ThroughItsHome_SwapsOnOneRelease(t *testing.T) {
	t.Parallel()
	for _, lc := range []struct {
		name           string
		buffers, held  int
		bufferHoldsBin string
	}{
		{"no buffer", 0, 0, ""},
		{"a free buffer", 1, 0, ""},
		{"its buffer holding an empty", 1, 1, "KSR-BUF-EMPTY-1"},
	} {
		t.Run(lc.name, func(t *testing.T) {
			t.Parallel()
			c := newKsrCell(t, homeLine(lc.buffers, lc.held))
			supply, evac := c.homeLineStaged("the request")
			defer func() { c.coreRoute("at the end", c.rows()...) }()

			start := time.Now()
			ev := func(what string) {
				t.Logf("+%s %s | %s holds %v, %s holds %v", time.Since(start).Round(time.Millisecond), what, ksrHomeA,
					labels(c.binsAt(ksrHomeA)), ksrLine, labels(c.binsAt(ksrLine)))
			}
			ev("both robots at their waits")
			mustNil(t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}),
				"RELEASE")
			c.settle()
			c.eventually("the one RELEASE sends both legs on", func() bool {
				_, s := c.fleetBlocks(supply)
				_, e := c.fleetBlocks(evac)
				return s && e
			})
			for _, l := range []struct {
				name string
				o    domain.Order
			}{{"supply", supply}, {"evac", evac}} {
				blocks, complete := c.fleetBlocks(l.o)
				t.Logf("after RELEASE the %s's vendor order holds %v (complete=%v); Edge row %s, Core %s", l.name, blocks,
					complete, c.edgeRow(l.o.ID).Status, c.coreOf(l.o).Status)
			}
			ev("RELEASE: both legs sent on")

			if !c.pickupAt(c.coreOf(supply), ksrHomeA) {
				t.Fatalf("the supply holds no pickup at %s", ksrHomeA)
			}
			ev("the supply lifts the full at the home")
			if !c.pickupAt(c.coreOf(evac), ksrLine) {
				t.Fatalf("the evac holds no pickup at %s", ksrLine)
			}
			bare := time.Now()
			ev("the evac lifts the old cart off the line: the line bare")
			c.drive(c.coreOf(evac), "RUNNING", "FINISHED")
			ev("the evac sets the old cart down on the home")
			c.drive(c.coreOf(supply), "RUNNING", "FINISHED")
			ev("the supply sets the full down on the line")
			t.Logf("the line stood bare for %s of harness time (no travel time here): from the evac's lift to the "+
				"supply's set-down", time.Since(bare).Round(time.Millisecond))
			c.driveUntil("both legs confirmed", func() bool {
				done := true
				for _, l := range []domain.Order{supply, evac} {
					if c.edgeRow(l.ID).Status == protocol.StatusDelivered {
						mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(l.ID, 1), "confirm")
					}
					if !protocol.IsTerminal(c.edgeRow(l.ID).Status) || !protocol.IsTerminal(c.coreOf(l).Status) {
						done = false
					}
				}
				return done
			})
			for _, l := range []domain.Order{supply, evac} {
				if s := c.coreOf(l).Status; s != protocol.StatusConfirmed {
					t.Errorf("leg %d ended %s at Core, want confirmed", l.ID, s)
				}
			}
			c.lineHolds("after the swap", "KSR-HOME-A-FULL")
			if got := labels(c.binsAt(ksrHomeA)); !slices.Equal(got, []string{"KSR-LINE-BIN"}) {
				t.Errorf("after the swap the home holds %v, want the old cart alone", got)
			}
			for _, b := range c.binsAt(ksrHomeA) {
				t.Logf("the loader operator's home %s holds %s: part %q, %d of %d left, status %s", ksrHomeA, b.Label,
					b.PayloadCode, b.UOPRemaining, b.UOPCapacity, b.Status)
			}
			if lc.buffers > 0 {
				want := []string{}
				if lc.bufferHoldsBin != "" {
					want = []string{lc.bufferHoldsBin}
				}
				if got := labels(c.binsAt("KSR-BUF-1")); !slices.Equal(got, want) {
					t.Errorf("after the swap the buffer holds %v, want %v", got, want)
				}
			}
		})
	}
}

// loseRelease takes the order's release off the Edge's outbox before it is
// sent: the release never reaches Core, as a message lost on the way.
func (c *ksrCell) loseRelease(o domain.Order) {
	c.t.Helper()
	msgs, err := c.edge.DB.ListPendingOutbox(1000)
	mustNil(c.t, err, "the Edge's outbox")
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderRelease {
			continue
		}
		var env protocol.Envelope
		mustNil(c.t, json.Unmarshal(m.Payload, &env), "a release envelope")
		var rel protocol.OrderRelease
		mustNil(c.t, env.DecodePayload(&rel), "a release")
		if rel.OrderUUID == o.UUID {
			mustNil(c.t, c.edge.DB.AckOutbox(m.ID), "lose the release")
			return
		}
	}
	c.t.Fatalf("no release of order %d on the Edge's outbox to lose", o.ID)
}

// sentDropAt reports whether Core has sent the fleet a set-down at node for
// the order.
func (c *ksrCell) sentDropAt(o domain.Order, node string) bool {
	c.t.Helper()
	blocks, _ := c.fleetBlocks(o)
	return slices.Contains(blocks, protocol.ActionDropoff+"("+node+")")
}

// The brief's A2, the missing hold: a cart is not sent to set down on a spot
// until the cart standing there has been lifted. On the line through its home,
// the old cart's drop is the home, where the full stands until the supply lifts
// it. Two ways the lift does not happen: the supply's release never reaches
// Core, and the supply's robot stands faulted at the home (released, never
// lifting). In both the evac should not be sent to set the old cart down on
// the home while the full stands on it.
//
// RED at 9c282051 in both, and skipped unless SHINGO_KNOWN_RED=1 (lane g-line's
// notes have the red output): the park reads the home clear because the
// sibling's plan picks up there (loader_place.go homeClearForReturn), the
// release hold keys on a stamp or a staged sibling's grant (vacate_hold.go
// vacateHoldsAtRelease), and this line has neither, so the evac's release
// appends pickup(L) dropoff(H) at once, whatever the supply has done.
func TestScenario_TwoRobotLine_ThroughItsHome_HoldsTheOldCartOffTheFull(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		lost bool
	}{{"the supply's release does not reach Core", true}, {"the supply's robot stands faulted at the home", false}} {
		t.Run(tc.name, func(t *testing.T) {
			if os.Getenv("SHINGO_KNOWN_RED") == "" {
				t.Skip("brief G A2 red pin, red until Core holds a set-down on a spot until the cart standing " +
					"there is lifted; run with SHINGO_KNOWN_RED=1")
			}
			t.Parallel()
			c := newKsrCell(t, homeLine(0, 0))
			supply, evac := c.homeLineStaged("the request")
			defer func() { c.coreRoute("at the end", c.rows()...) }()
			mustNil(t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}),
				"RELEASE")
			if tc.lost {
				c.loseRelease(supply)
			}
			c.settle()
			for i := 0; i < 3; i++ {
				c.tick()
			}
			for _, l := range []struct {
				name string
				o    domain.Order
			}{{"supply", supply}, {"evac", evac}} {
				blocks, complete := c.fleetBlocks(l.o)
				co := c.coreOf(l.o)
				t.Logf("after RELEASE the %s's vendor order holds %v (complete=%v); Core %s cause=%q; Edge row %s",
					l.name, blocks, complete, co.Status, co.QueueCause, c.edgeRow(l.o.ID).Status)
			}
			check := func(when string) {
				t.Helper()
				if c.binAt("KSR-HOME-A-FULL") == ksrHomeA && c.sentDropAt(evac, ksrHomeA) {
					blocks, _ := c.fleetBlocks(evac)
					t.Errorf("%s: the evac is sent to set the old cart down on %s while the full stands on it "+
						"(its vendor order holds %v)", when, ksrHomeA, blocks)
				}
			}
			check("after RELEASE")
			// The evac's robot lifts the line's cart, if it was let go; the
			// supply's does not lift.
			if c.pickupAt(c.coreOf(evac), ksrLine) {
				t.Logf("the evac lifted the old cart off the line; %s holds %v", ksrHomeA, labels(c.binsAt(ksrHomeA)))
				check("after the evac's lift")
			}
			if c.sentDropAt(evac, ksrHomeA) {
				// What follows on the floor: the robot carrying the old cart
				// arrives at the home and sets it down.
				c.drive(c.coreOf(evac), "RUNNING", "FINISHED")
				c.settle()
				t.Logf("the evac set the old cart down: %s holds %v; the full is at %q; the old cart at %q",
					ksrHomeA, labels(c.binsAt(ksrHomeA)), c.binAt("KSR-HOME-A-FULL"), c.binAt("KSR-LINE-BIN"))
			}
		})
	}
}

// legRow is what Core and the Edge say of one leg, in one line.
func (c *ksrCell) legRow(name string, o domain.Order) string {
	c.t.Helper()
	co := c.coreOf(o)
	e := c.edgeRow(o.ID)
	return fmt.Sprintf("%s: Core %s (vendor %s) cause=%q reason=%q | Edge %s code=%q reason=%q", name, co.Status,
		c.vendorState(co.VendorOrderID), co.QueueCause, co.QueueReason, e.Status, e.QueueCode, e.QueueReason)
}

// swapReady is the station's RELEASE button for the line: the answer its
// view gives (store.ComputeSwapReady).
func (c *ksrCell) swapReady() bool {
	c.t.Helper()
	claim, err := c.edge.DB.GetStyleNodeClaim(c.claimA)
	mustNil(c.t, err, "the line's claim")
	rt, err := c.edge.DB.GetProcessNodeRuntime(c.nodeID)
	mustNil(c.t, err, "the line's runtime")
	return edgestore.ComputeSwapReady(c.edge.DB, claim, rt, nil)
}

// The brief's A3: one robot free at REQUEST. Core gives every leg to the fleet
// (it reads no robot's availability), and the fleet gives its one robot to one
// leg; the other stands unassigned (CREATED). The docker harness's simulator
// has no robot pool (the pool is the sim build's Driver), so that is modelled
// by driving one leg's robot and leaving the other's order alone.
//
// Pinned as it runs at 9c282051. The leg with no robot reads dispatched at Core
// and the Edge, with no cause and no sentence. Nothing is lifted while one
// robot serves the pair, whichever leg it takes. With the evac parked the
// station shows RELEASE, and a press is held at the Edge on the home's lift and
// remembered; with the supply parked it shows none. A second robot reaching its
// wait frees the swap: the remembered press goes out by itself, or the RELEASE
// the station now shows runs it.
func TestScenario_TwoRobotLine_ThroughItsHome_OneRobotFree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		evacFirst bool
	}{{"the robot takes the evac", true}, {"the robot takes the supply", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			c := newKsrCell(t, homeLine(1, 0))
			mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
			c.settle()
			before := c.lastID()
			c.operatorRequest()
			c.tick()
			supply, evac := c.pairLegs("the request", before)
			defer func() { c.coreRoute("at the end", c.rows()...) }()
			c.eventually("both legs with the fleet", func() bool {
				return c.coreOf(supply).VendorOrderID != "" && c.coreOf(evac).VendorOrderID != ""
			})
			served, idle := evac, supply
			if !tc.evacFirst {
				served, idle = supply, evac
			}
			c.drive(c.coreOf(served), "RUNNING", "WAITING")
			c.eventually("the served leg parks at its wait", func() bool {
				return c.edgeRow(served.ID).Status == protocol.StatusStaged
			})
			for i := 0; i < 3; i++ {
				c.tick()
			}
			t.Logf("one robot: %s", c.legRow("supply", supply))
			t.Logf("one robot: %s", c.legRow("evac", evac))
			ready := c.swapReady()
			t.Logf("one robot: the station's RELEASE button shows: %v", ready)
			// The leg with no robot: dispatched at Core and the Edge, unassigned
			// at the fleet, with no cause and no sentence on either row.
			co, e := c.coreOf(idle), c.edgeRow(idle.ID)
			if v := c.vendorState(co.VendorOrderID); v != "CREATED" || co.Status != protocol.StatusDispatched ||
				e.Status != protocol.StatusDispatched || co.QueueCause != "" || e.QueueReason != "" {
				t.Errorf("the leg with no robot reads %s", c.legRow("it", idle))
			}
			// RELEASE shows with the evac parked, not with the supply alone
			// (store.ComputeSwapReady gates on the evac).
			if ready != tc.evacFirst {
				t.Errorf("RELEASE shows %v with one robot on the %s", ready, map[bool]string{true: "evac", false: "supply"}[tc.evacFirst])
			}
			if ready {
				// Pressed, it sends nothing: the evac's drop needs the supply's
				// lift (G7), and the supply has no robot. The press is remembered.
				err := c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"})
				t.Logf("one robot: RELEASE answers %v", err)
				if err == nil || !strings.Contains(err.Error(), "waiting for the bin on "+ksrHomeA+" to be lifted") {
					t.Errorf("RELEASE with one robot on the evac answers %v, want it held on the home's lift", err)
				}
				c.settle()
				for i := 0; i < 3; i++ {
					c.tick()
				}
				t.Logf("one robot, after RELEASE: %s", c.legRow("supply", supply))
				t.Logf("one robot, after RELEASE: %s", c.legRow("evac", evac))
				for _, l := range []struct {
					name string
					o    domain.Order
				}{{"supply", supply}, {"evac", evac}} {
					blocks, complete := c.fleetBlocks(l.o)
					t.Logf("one robot, after RELEASE: the %s's vendor order holds %v (complete=%v)", l.name, blocks, complete)
					if complete {
						t.Errorf("one robot: RELEASE sent the %s on", l.name)
					}
				}
			}
			if got := labels(c.binsAt(ksrLine)); !slices.Equal(got, []string{"KSR-LINE-BIN"}) {
				t.Errorf("with one robot the line holds %v, want its cart untouched", got)
			}
			if got := labels(c.binsAt(ksrHomeA)); !slices.Equal(got, []string{"KSR-HOME-A-FULL"}) {
				t.Errorf("with one robot the home holds %v, want its full untouched", got)
			}

			// A second robot comes free and takes the other leg to its wait.
			c.drive(c.coreOf(idle), "RUNNING", "WAITING")
			c.eventually("the second robot parks at its wait", func() bool {
				return c.edgeRow(idle.ID).Status == protocol.StatusStaged || c.edgeRow(idle.ID).Status == protocol.StatusInTransit
			})
			for i := 0; i < 3; i++ {
				c.tick()
			}
			t.Logf("two robots: %s", c.legRow("supply", supply))
			t.Logf("two robots: %s", c.legRow("evac", evac))
			_, sentS := c.fleetBlocks(supply)
			_, sentE := c.fleetBlocks(evac)
			t.Logf("two robots: the remembered release went out by itself: supply %v, evac %v; RELEASE shows %v",
				sentS, sentE, c.swapReady())
			// The press made with one robot goes out by itself when the second
			// parks; with no press made, the swap waits for one.
			if sentS != tc.evacFirst || sentE != tc.evacFirst {
				t.Errorf("when the second robot parks the legs went on supply=%v evac=%v, want %v (a press was made: %v)",
					sentS, sentE, tc.evacFirst, tc.evacFirst)
			}
			if !sentS || !sentE {
				mustNil(t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}),
					"RELEASE with two robots")
				c.settle()
			}
			c.eventually("both legs sent on", func() bool {
				_, s := c.fleetBlocks(supply)
				_, e := c.fleetBlocks(evac)
				return s && e
			})
			c.drive(c.coreOf(supply), "RUNNING", "FINISHED")
			c.drive(c.coreOf(evac), "RUNNING", "FINISHED")
			c.driveUntil("both legs confirmed", func() bool {
				done := true
				for _, l := range []domain.Order{supply, evac} {
					if c.edgeRow(l.ID).Status == protocol.StatusDelivered {
						mustNil(t, c.edge.Engine.OrderManager().ConfirmDelivery(l.ID, 1), "confirm")
					}
					if !protocol.IsTerminal(c.edgeRow(l.ID).Status) || !protocol.IsTerminal(c.coreOf(l).Status) {
						done = false
					}
				}
				return done
			})
			c.lineHolds("after the swap", "KSR-HOME-A-FULL")
		})
	}
}
