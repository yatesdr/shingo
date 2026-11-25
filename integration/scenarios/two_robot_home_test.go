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
	"slices"
	"strings"
	"testing"

	"shingo/protocol"

	"shingocore/plantspec"
	coreorders "shingocore/store/orders"

	"shingoedge/domain"
	edgeengine "shingoedge/engine"
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
