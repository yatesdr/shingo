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
	"io"
	"net/http"
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
				t.Skip("brief G A2 red pin, kept skipped by the owner's ruling of 2026-11-25: no hold between a " +
					"lift and a set-down; with both robots holding orders the fleet manager sequences them, to be " +
					"watched on the floor (DOCKET R177); run with SHINGO_KNOWN_RED=1")
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

// The brief's A4: the part change on the two-robot line through its homes. A's
// home and B's home stand on one loader; the changeover's pair (the supply
// fetching B's full from B's home, the evac taking the line's old cart to A's
// home) runs through them with A's home bare, holding A's spare with a free
// buffer, and holding it with no buffer; the old cart empty (RELEASE EMPTY) and
// part-used.
//
// Pinned as it runs at 9c282051. The park places the evac's drop before the
// release, when the old cart still carries A whatever the release will say, so
// an empty and a part-used cart go alike: A's home when it is bare; the free
// buffer when A's spare stands on it; with no buffer both legs wait at Core,
// loader-park-no-slot, nothing lifted, until A's home empties by another door
// (here a person moves the spare off it), and the pair then runs to A's home.
// B's home, which the supply empties, is never offered: it is pinned to B and
// the cart reads A (loader_place.go homeAcceptsCarrier).
func TestScenario_TwoRobotLine_ThroughItsHome_PartChange(t *testing.T) {
	t.Parallel()
	for _, hc := range []struct {
		name     string
		bare     bool
		buffers  int
		waits    bool
		drop     string // the evac's drop as Core sends it
		spareEnd string // where A's spare ends, "" for none
	}{
		{"the old part's home bare", true, 0, false, ksrHomeA, ""},
		{"the old part's home holding its spare, a free buffer", false, 1, false, "KSR-BUF-1", ksrHomeA},
		{"the old part's home holding its spare, no buffer", false, 0, true, ksrHomeA, ksrOutStg},
	} {
		for _, cart := range []struct {
			name    string
			empty   bool
			payload string
		}{{"an empty old cart", true, ""}, {"a part-used old cart", false, ksrPartA}} {
			t.Run(hc.name+", "+cart.name, func(t *testing.T) {
				t.Parallel()
				o := homeLine(hc.buffers, 0)
				o.bareHomeA = hc.bare
				c := newKsrCell(t, o)
				mustNil(t, c.edge.DB.SetProcessNodeRuntime(c.nodeID, &c.claimA, 30), "count")
				c.settle()
				before := c.lastID()
				if _, err := c.edge.Engine.StartProcessChangeover(c.processID, c.styleB, "ksr", ""); err != nil {
					c.dump("START")
					t.Fatalf("START refused: %v", err)
				}
				c.tick()
				defer func() { c.coreRoute("at the end", c.rows()...) }()
				var legs []domain.Order
				for _, o := range c.newRows(before) {
					if o.OrderType == protocol.OrderTypeComplex && o.ProcessNodeID != nil && *o.ProcessNodeID == c.nodeID {
						legs = append(legs, o)
					}
				}
				if len(legs) != 2 {
					c.dump("START")
					t.Fatalf("the changeover made %d legs at the line, want the two-robot pair", len(legs))
				}
				supply, evac := legs[0], legs[1]
				if evac.DeliveryNode == ksrLine {
					supply, evac = evac, supply
				}
				for i := 0; i < 3; i++ {
					c.tick()
				}
				c.coreRoute("after START", supply, evac)
				wantSupply := []string{"wait(" + ksrHomeB + ")", "pickup(" + ksrHomeB + ")", "dropoff(" + ksrLine + ")"}
				if got := c.coreStops(c.coreOf(supply)); !slices.Equal(got, wantSupply) {
					t.Errorf("Core stores the changeover's supply as %v, want %v", got, wantSupply)
				}
				if hc.waits {
					c.partChangeWaits(supply, evac)
				}
				c.eventually("the pair with the fleet", func() bool {
					return c.coreOf(supply).VendorOrderID != "" && c.coreOf(evac).VendorOrderID != ""
				})
				wantEvac := []string{"wait(" + ksrLine + ")", "pickup(" + ksrLine + ")", "dropoff(" + hc.drop + ")"}
				if got := c.coreStops(c.coreOf(evac)); !slices.Equal(got, wantEvac) {
					t.Errorf("Core sends the changeover's evac as %v, want %v", got, wantEvac)
				}
				c.drive(c.coreOf(supply), "RUNNING", "WAITING")
				c.drive(c.coreOf(evac), "RUNNING", "WAITING")
				c.eventually("both legs staged at the Edge", func() bool {
					return c.edgeRow(supply.ID).Status == protocol.StatusStaged && c.edgeRow(evac.ID).Status == protocol.StatusStaged
				})
				disp := edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}
				if cart.empty {
					disp.Mode = edgeengine.DispositionCaptureLineside
				}
				if _, err := c.edge.Engine.ReleaseChangeoverWaitForNode(c.processID, c.nodeID, disp); err != nil {
					c.dump("release")
					t.Fatalf("the release at the line was refused: %v", err)
				}
				c.settle()
				c.eventually("the one release sends both legs on", func() bool {
					_, s := c.fleetBlocks(supply)
					_, e := c.fleetBlocks(evac)
					return s && e
				})
				if !c.pickupAt(c.coreOf(supply), ksrHomeB) || !c.pickupAt(c.coreOf(evac), ksrLine) {
					t.Fatal("the pair holds no lift at B's home and the line")
				}
				c.drive(c.coreOf(evac), "RUNNING", "FINISHED")
				c.drive(c.coreOf(supply), "RUNNING", "FINISHED")
				c.eventually("both legs done", func() bool {
					return protocol.IsTerminal(c.coreOf(supply).Status) && protocol.IsTerminal(c.coreOf(evac).Status)
				})
				t.Logf("end: line %v; A's home %v; B's home %v; buffer %v", labels(c.binsAt(ksrLine)),
					labels(c.binsAt(ksrHomeA)), labels(c.binsAt(ksrHomeB)), labels(c.binsAt("KSR-BUF-1")))
				c.lineHolds("after the part change", "KSR-HOME-B-FULL")
				if at := c.binAt("KSR-LINE-BIN"); at != hc.drop {
					t.Errorf("the old cart is at %q after the part change, want %s", at, hc.drop)
				}
				if at := c.binAt("KSR-HOME-A-FULL"); at != hc.spareEnd {
					t.Errorf("A's spare is at %q after the part change, want %q", at, hc.spareEnd)
				}
				if got := labels(c.binsAt(ksrHomeB)); len(got) != 0 {
					t.Errorf("B's home holds %v after the part change, want it bare", got)
				}
				for _, b := range c.binsAt(hc.drop) {
					if b.Label == "KSR-LINE-BIN" {
						t.Logf("the old cart on %s reads part %q, %d left", hc.drop, b.PayloadCode, b.UOPRemaining)
						if b.PayloadCode != cart.payload {
							t.Errorf("the old cart reads part %q, want %q", b.PayloadCode, cart.payload)
						}
					}
				}
				for _, l := range []domain.Order{supply, evac} {
					if s := c.coreOf(l).Status; s != protocol.StatusConfirmed {
						t.Errorf("leg %d ended %s at Core, want confirmed", l.ID, s)
					}
				}
			})
		}
	}
}

// partChangeWaits is the part change with no spot for the old cart: both legs
// wait at Core on the loader, saying which spot frees them, and nothing is
// lifted; then a person moves A's spare off its home on Core's bins page.
func (c *ksrCell) partChangeWaits(supply, evac domain.Order) {
	c.t.Helper()
	want := "Waiting for a slot at " + ksrHomeA + " — empty one of " + ksrHomeA + " and it goes"
	for _, l := range []struct {
		name string
		o    domain.Order
	}{{"supply", supply}, {"evac", evac}} {
		c.t.Logf("after START: %s", c.legRow(l.name, l.o))
		co, e := c.coreOf(l.o), c.edgeRow(l.o.ID)
		if co.Status != protocol.StatusQueued || co.QueueCause != "loader-park-no-slot" || co.QueueReason != want ||
			co.VendorOrderID != "" || e.Status != protocol.StatusQueued || e.QueueCode != "waiting_for_slot" ||
			e.QueueReason != want {
			c.t.Errorf("with no spot for the old cart the %s reads %s, want queued loader-park-no-slot %q at both",
				l.name, c.legRow(l.name, l.o), want)
		}
	}
	if c.binAt("KSR-LINE-BIN") != ksrLine || c.binAt("KSR-HOME-A-FULL") != ksrHomeA || c.binAt("KSR-HOME-B-FULL") != ksrHomeB {
		c.t.Errorf("while the pair waits a cart moved: old cart %q, A's spare %q, B's full %q",
			c.binAt("KSR-LINE-BIN"), c.binAt("KSR-HOME-A-FULL"), c.binAt("KSR-HOME-B-FULL"))
	}
	spare := c.binsAt(ksrHomeA)
	if len(spare) != 1 {
		c.t.Fatalf("A's home holds %v, want the spare alone", labels(spare))
	}
	out, err := c.core.eng.DB().GetNodeByDotName(ksrOutStg)
	mustNil(c.t, err, "where the spare goes")
	c.moveByHand(spare[0], out.ID, "move A's spare off its home")
}

// homeCard is the loader board's card for a home, from the home's tile as GET
// /api/operator-stations/{id}/view serves it (www apiGetOperatorStationView):
// the keys the card's status word, its order list and its CLEAR BIN are drawn
// from (www static/operator-station/operator-modal.js renderModal, :145-224
// and :579-584).
type homeCard struct {
	Node struct {
		ID int64 `json:"id"`
	} `json:"node"`
	Runtime *struct {
		RemainingUOPCached int `json:"remaining_uop_cached"`
	} `json:"runtime"`
	Orders []struct {
		ID     int64  `json:"id"`
		Status string `json:"status"`
	} `json:"orders"`
	BinState *struct {
		BinLabel    string `json:"bin_label"`
		PayloadCode string `json:"payload_code"`
		Occupied    bool   `json:"occupied"`
		Stocked     bool   `json:"stocked"`
	} `json:"bin_state"`
	Loader *struct {
		Layout string `json:"layout"`
	} `json:"loader"`
}

// count is the card's count: the tile's runtime count, the Edge's own for the
// node, 0 with no runtime (operator-modal.js:147-148).
func (h homeCard) count() int {
	if h.Runtime == nil {
		return 0
	}
	return h.Runtime.RemainingUOPCached
}

// word is the card's status word at a ratio of 1 (operator-modal.js:165-166).
func (h homeCard) word() string {
	if n := h.count(); n > 0 {
		return fmt.Sprintf("LOADED (%d UoP)", n)
	}
	return "EMPTY"
}

// offersClear is loaderOffersClear (operator-modal.js:732-736) on the card's
// bin and count: whether the card draws CLEAR BIN (:579-584).
func (h homeCard) offersClear() bool {
	b := h.BinState
	switch {
	case b == nil || !b.Occupied:
		return false
	case h.count() > 0:
		return true
	}
	return b.PayloadCode != "" && !b.Stocked
}

func (h homeCard) String() string {
	var orders []string
	for _, o := range h.Orders {
		orders = append(orders, fmt.Sprintf("%d %s", o.ID, o.Status))
	}
	bin := "none"
	if b := h.BinState; b != nil {
		bin = fmt.Sprintf("%s part=%q occupied=%v stocked=%v", b.BinLabel, b.PayloadCode, b.Occupied, b.Stocked)
	}
	return fmt.Sprintf("%s; orders %v; bin %s; a loader card %v; CLEAR BIN %v", h.word(), orders, bin, h.Loader != nil,
		h.offersClear())
}

// The brief's H3 (brief H; the owner, 2026-11-25: "a robot staging should not
// invite loader interaction"). The line through its home (A1's, no buffer),
// its home's cart loaded at the Edge's own LOAD as a plant's loader operator
// loads it, so the Edge's count for the home is the cart's. REQUEST; both
// robots to their waits, the supply's staged at the home under the cart it
// holds. What the loader board's card for the home offers, and what Core
// answers to its CLEAR BIN; then the same once the supply has lifted the cart
// and gone, and once the swap is done, with nothing staged at the home.
//
// The card is read from the board's own JSON (the loader's screen made by its
// own door, CreateLoaderBoard; the view as the board fetches it) and drawn by
// the board's rules, named on homeCard. CLEAR BIN is pressed through the door
// the card posts to (POST /api/process-nodes/{id}/clear-bin: Engine.ClearBin,
// Core's bin.clear), not the home clear (BRING PARTIAL HOME's, clear_home).
//
// The card, as it is at 92351072 and stays (no loader board change in this
// brief, the owner's word): with the supply's robot under the cart it reads
// LOADED (40 UoP), lists no order (the supply's row is the line's and carries
// no source, ListActiveByNodeKeys), says nothing of a robot and offers CLEAR
// BIN. Core should refuse that clear, saying a robot is staged at the home,
// and change nothing: the cart keeps its part and count, the supply its hold
// and its wait, the card its LOADED. At 92351072 Core takes it: the cart is
// zeroed under the robot and the supply carries an empty to the line. With
// the cart lifted off the home the card offers no CLEAR BIN and Core answers
// that there is no bin; with the swap done and the old cart on the home, held
// by nothing, the card offers CLEAR BIN and Core takes it, as the loader's own.
func TestScenario_TwoRobotLine_ThroughItsHome_LoaderCardWhileStaged(t *testing.T) {
	t.Parallel()
	o := homeLine(0, 0)
	o.bareHomeA = true
	c := newKsrCell(t, o)
	r := newDloRig(c)
	coreDB := c.core.eng.DB()

	// asked runs a door that waits for Core's answer while the bus carries the
	// ask and the answer, as the two outbox drainers do on a plant.
	asked := func(door func() error) error {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- door() }()
		for {
			select {
			case err := <-done:
				c.settle()
				return err
			case <-time.After(10 * time.Millisecond):
				c.bus.PumpAll()
			}
		}
	}

	// ── the loader's screen, made by its own door, and the home's cart loaded at it ──
	loaders, err := c.edge.DB.ListCoreLoaders()
	mustNil(t, err, "the Edge's loaders")
	key := ""
	for _, l := range loaders {
		if l.Name == "KSR-LOADER" {
			key = l.LoaderKey
		}
	}
	if key == "" {
		t.Fatalf("the Edge holds no KSR-LOADER from Core's node list (%d loaders)", len(loaders))
	}
	loadProc, err := c.edge.DB.CreateProcess("KSR-LOAD-OPS", "the loader's screen", "", "", false)
	mustNil(t, err, "the loader's process")
	board, err := c.edge.Engine.StationService().CreateLoaderBoard(key, loadProc)
	mustNil(t, err, "the loader's screen")
	windows, err := c.edge.DB.ListProcessNodesByStation(board)
	mustNil(t, err, "the screen's windows")
	var home int64
	for _, n := range windows {
		if n.CoreNodeName == ksrHomeA {
			home = n.ID
		}
	}
	if home == 0 {
		t.Fatalf("the loader's screen has no window at %s (%d windows)", ksrHomeA, len(windows))
	}
	hn, err := coreDB.GetNodeByDotName(ksrHomeA)
	mustNil(t, err, "the home")
	empty := &corebins.Bin{BinTypeID: c.typeA.ID, Label: "KSR-HOME-A-FULL", NodeID: &hn.ID, Status: "available"}
	mustNil(t, coreDB.CreateBin(empty), "an empty of A's carrier on the home")
	c.bus.PushBins()
	c.settle()
	pack := int64(40)
	mustNil(t, asked(func() error { return c.edge.Engine.LoadBin(home, ksrPartA, &pack, nil) }),
		"the loader operator's LOAD of 40 on the home")

	card := func() homeCard {
		t.Helper()
		res, err := http.Get(fmt.Sprintf("%s/api/operator-stations/%d/view", r.page, board))
		mustNil(t, err, "GET the loader screen's view")
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		mustNil(t, err, "read the loader screen's view")
		if res.StatusCode != http.StatusOK {
			t.Fatalf("GET the loader screen's view: %d %s", res.StatusCode, body)
		}
		var view struct {
			Nodes []homeCard `json:"nodes"`
		}
		mustNil(t, json.Unmarshal(body, &view), "decode the loader screen's view")
		for _, h := range view.Nodes {
			if h.Node.ID == home {
				return h
			}
		}
		t.Fatalf("the loader screen's view has no card for %s (node %d): %s", ksrHomeA, home, body)
		return homeCard{}
	}
	// clearBin is the card's CLEAR BIN: the door it posts to, with the body it
	// posts (operator-util.js postAction), and the door's answer: its status
	// and the sentence the board toasts ("ok" for an accept).
	clearBin := func() (int, string) {
		t.Helper()
		var status int
		var said string
		mustNil(t, asked(func() error {
			res, err := http.Post(fmt.Sprintf("%s/api/process-nodes/%d/clear-bin", r.page, home), "application/json",
				strings.NewReader("{}"))
			if err != nil {
				return err
			}
			defer res.Body.Close()
			var ans struct {
				Status string `json:"status"`
				Error  string `json:"error"`
			}
			if err := json.NewDecoder(res.Body).Decode(&ans); err != nil {
				return err
			}
			status, said = res.StatusCode, ans.Error
			if said == "" {
				said = ans.Status
			}
			return nil
		}), "press CLEAR BIN")
		return status, said
	}
	// cartsAt is Core's carts at node, each as label, part, count and holder.
	cartsAt := func(node string) []string {
		t.Helper()
		var out []string
		for _, b := range c.binsAt(node) {
			held := "nobody"
			if b.ClaimedBy != nil {
				held = fmt.Sprintf("order %d", *b.ClaimedBy)
			}
			out = append(out, fmt.Sprintf("%s %q %d held by %s", b.Label, b.PayloadCode, b.UOPRemaining, held))
		}
		return out
	}

	loaded := card()
	t.Logf("loaded: the home's card reads %s; Core's home holds %v", loaded, cartsAt(ksrHomeA))
	if want := []string{`KSR-HOME-A-FULL "KSR-PA" 40 held by nobody`}; !slices.Equal(cartsAt(ksrHomeA), want) ||
		loaded.word() != "LOADED (40 UoP)" || !loaded.offersClear() {
		t.Fatalf("fixture: after the LOAD the card reads %s and Core's home holds %v; want LOADED (40 UoP) over %v",
			loaded, cartsAt(ksrHomeA), want)
	}

	// ── the supply staged at the home, under the cart it holds ──
	supply, evac := c.homeLineStaged("the request")
	defer func() { c.coreRoute("at the end", c.rows()...) }()
	sp := c.coreOf(supply)
	heldCart := []string{fmt.Sprintf(`KSR-HOME-A-FULL "KSR-PA" 40 held by order %d`, sp.ID)}
	if got := cartsAt(ksrHomeA); sp.Status != protocol.StatusStaged || !slices.Equal(got, heldCart) {
		t.Fatalf("fixture: the supply (Core order %d) reads %s and Core's home holds %v; want staged, holding %v",
			sp.ID, sp.Status, got, heldCart)
	}
	staged := card()
	t.Logf("staged: the supply's robot at the home (Core order %d %s); its card reads %s", sp.ID, sp.Status, staged)
	if staged.word() != "LOADED (40 UoP)" || len(staged.Orders) != 0 || staged.BinState == nil ||
		staged.BinState.BinLabel != "KSR-HOME-A-FULL" || !staged.BinState.Stocked || !staged.offersClear() {
		t.Errorf("staged: the card reads %s; want it as it is: LOADED (40 UoP), no order, KSR-HOME-A-FULL stocked, "+
			"CLEAR BIN offered", staged)
	}
	status, said := clearBin()
	t.Logf("staged: CLEAR BIN answers %d %q", status, said)
	want := "clear bin: Not cleared: a robot is staged at " + ksrHomeA + " to take KSR-HOME-A-FULL to " + ksrLine + "."
	if status != http.StatusBadRequest || said != want {
		t.Errorf("staged: CLEAR BIN answers %d %q; want %d %q", status, said, http.StatusBadRequest, want)
	}
	after := card()
	t.Logf("staged, after CLEAR BIN: Core's home holds %v; the card reads %s; %s; %s", cartsAt(ksrHomeA), after,
		c.legRow("supply", supply), c.legRow("evac", evac))
	if got := cartsAt(ksrHomeA); !slices.Equal(got, heldCart) || after.word() != "LOADED (40 UoP)" ||
		c.coreOf(supply).Status != protocol.StatusStaged || c.edgeRow(supply.ID).Status != protocol.StatusStaged {
		t.Errorf("staged, after CLEAR BIN: Core's home holds %v, the card reads %s, the supply %s; want nothing "+
			"changed: %v, LOADED (40 UoP), staged", got, after, c.legRow("supply", supply), heldCart)
	}

	// ── the supply lifts the cart and goes: nothing staged at the home ──
	mustNil(t, c.edge.Engine.ReleaseStagedOrders(c.nodeID, edgeengine.ReleaseDisposition{CalledBy: "ksr-operator"}),
		"RELEASE")
	c.settle()
	c.eventually("the one RELEASE sends both legs on", func() bool {
		_, s := c.fleetBlocks(supply)
		_, e := c.fleetBlocks(evac)
		return s && e
	})
	if !c.pickupAt(c.coreOf(supply), ksrHomeA) {
		t.Fatalf("the supply holds no pickup at %s", ksrHomeA)
	}
	lifted := card()
	t.Logf("lifted: Core's home holds %v; the card reads %s", cartsAt(ksrHomeA), lifted)
	if got := cartsAt(ksrHomeA); len(got) != 0 || lifted.word() != "EMPTY" || lifted.offersClear() ||
		len(lifted.Orders) != 0 {
		t.Errorf("lifted: Core's home holds %v and the card reads %s; want nothing there, EMPTY, no CLEAR BIN", got,
			lifted)
	}
	status, said = clearBin()
	t.Logf("lifted: CLEAR BIN answers %d %q", status, said)
	if want := "clear bin: no bin at node " + ksrHomeA; status != http.StatusBadRequest || said != want {
		t.Errorf("lifted: CLEAR BIN answers %d %q; want %d %q", status, said, http.StatusBadRequest, want)
	}

	// ── the swap done: the old cart on the home, held by nothing ──
	if !c.pickupAt(c.coreOf(evac), ksrLine) {
		t.Fatalf("the evac holds no pickup at %s", ksrLine)
	}
	c.drive(c.coreOf(evac), "RUNNING", "FINISHED")
	c.drive(c.coreOf(supply), "RUNNING", "FINISHED")
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
	// The line has the full; the home the old cart (Core counts it 40: this
	// cell does not carry the line's draws to Core), held by nothing.
	swapped := card()
	t.Logf("swapped: the line holds %v; Core's home holds %v; the card reads %s", cartsAt(ksrLine),
		cartsAt(ksrHomeA), swapped)
	if got := cartsAt(ksrLine); !slices.Equal(got, []string{`KSR-HOME-A-FULL "KSR-PA" 40 held by nobody`}) {
		t.Errorf("swapped: the line holds %v; want the full the supply carried, KSR-HOME-A-FULL \"KSR-PA\" 40", got)
	}
	if got := cartsAt(ksrHomeA); !slices.Equal(got, []string{`KSR-LINE-BIN "KSR-PA" 40 held by nobody`}) ||
		swapped.word() != "LOADED (40 UoP)" || !swapped.offersClear() {
		t.Errorf("swapped: Core's home holds %v and the card reads %s; want the old cart, held by nobody, "+
			"LOADED (40 UoP), CLEAR BIN offered", got, swapped)
	}
	status, said = clearBin()
	t.Logf("swapped: CLEAR BIN answers %d %q; Core's home holds %v", status, said, cartsAt(ksrHomeA))
	if got := cartsAt(ksrHomeA); status != http.StatusOK || said != "ok" ||
		!slices.Equal(got, []string{`KSR-LINE-BIN "" 0 held by nobody`}) {
		t.Errorf("swapped: CLEAR BIN answers %d %q and Core's home holds %v; want it taken: 200, the old cart "+
			"emptied", status, said, got)
	}
}
