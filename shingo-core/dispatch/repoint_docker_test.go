//go:build docker

package dispatch

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
	"shingocore/store/plantclaims"
)

// repoint_docker_test.go — a drop Core moves at a release, after its waits were
// spliced, plans the rest of the route again (repoint): the waits follow the
// new drop. Each pin is the release of a staged leg whose carrier Core sends
// somewhere by what it holds.

// rpCell is a line whose claim sends its carriers to OUT, a kanban KAN (flat,
// three slots) supporting the line, and a part of capacity 100.
type rpCell struct {
	t       *testing.T
	db      *store.DB
	d       *Dispatcher
	backend *testdb.MockTrackingBackend
	prefix  string
	part    string
	kd      int64
	line    *nodes.Node
	kan     int64
}

func newRPCell(t *testing.T, prefix string, role protocol.ClaimRole, outbound, partialHome string) rpCell {
	t.Helper()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	d, backend := newTestDispatcherWithResolver(t, db)
	d.DebugLog = testDebugLog(t)
	c := rpCell{t: t, db: db, d: d, backend: backend, prefix: prefix, part: prefix + "-PART",
		line: prNode(t, db, prefix+"-LINE")}
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: c.part, UOPCapacity: 100}), "the part")
	_, err := db.ReplacePlantClaimsFromStation(prefix+"-PROC", "line-1",
		[]plantclaims.StyleRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STYLE", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STYLE", CoreNodeName: c.line.Name,
			Role: role, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: c.part, OutboundDestination: outbound,
			PartialDestination: partialHome}}, nil, 0)
	testutil.MustNoErr(t, err, "the line's claim")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, prefix+"-KD").Scan(&c.kd), "type")
	c.kan, err = db.CreateNodeGroup(prefix + "-KAN")
	testutil.MustNoErr(t, err, "kanban")
	for i := 1; i <= 3; i++ {
		testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: fmt.Sprintf("%s-KAN-%d", prefix, i), Enabled: true,
			ParentID: &c.kan}), "kanban slot")
	}
	testutil.MustNoErr(t, db.SetKanbanSupports(c.kan, []int64{c.line.ID}), "supports")
	testutil.MustNoErr(t, db.SetKanbanLevelAndLayerWants(store.KanbanLevel{GroupNodeID: c.kan, BinTypeID: c.kd, Want: 2}, nil), "level")
	return c
}

// rpLanedGroup is a marked group with one lane of three slots.
func rpLanedGroup(t *testing.T, db *store.DB, name string) (grp, lane *nodes.Node, slots []*nodes.Node) {
	t.Helper()
	id := testdb.MarkedGroup(t, db, name)
	laneType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "LANE type")
	lane = &nodes.Node{Name: name + "-L1", IsSynthetic: true, NodeTypeID: &laneType.ID, ParentID: id, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "lane")
	for d := 1; d <= 3; d++ {
		depth := d
		s := &nodes.Node{Name: fmt.Sprintf("%s-L1-S%d", name, d), ParentID: &lane.ID, Enabled: true, Depth: &depth}
		testutil.MustNoErr(t, db.CreateNode(s), "lane slot")
		slots = append(slots, s)
	}
	grp, err = db.GetNode(*id)
	testutil.MustNoErr(t, err, "group")
	return grp, lane, slots
}

// laneWait is the wait the splice puts in front of an entry into lane of grp.
func laneWait(grp, lane *nodes.Node) resolvedStep {
	return resolvedStep{Action: protocol.ActionWait, Node: grp.Name + "-WAIT", WaitKind: WaitKindLane,
		WaitLane: lane.ID, WaitGroup: grp.ID}
}

// carrier stands a carrier of the cell's type on the line, holding count of the part.
func (c rpCell) carrier(count int) *bins.Bin {
	c.t.Helper()
	b := makeEmptyBin(c.t, c.db, c.line.ID, c.prefix+"-CART")
	_, err := c.db.Exec(`UPDATE bins SET bin_type_id=$1 WHERE id=$2`, c.kd, b.ID)
	testutil.MustNoErr(c.t, err, "the carrier's type")
	testutil.MustNoErr(c.t, c.db.SetBinManifest(b.ID, `{"items":[]}`, c.part, count), "manifest")
	testutil.MustNoErr(c.t, c.db.ConfirmBinManifest(b.ID, ""), "confirm")
	got, err := c.db.GetBin(b.ID)
	testutil.MustNoErr(c.t, err, "reload the carrier")
	return got
}

// stagedLeg is a leg staged at its station wait on the line, its plan as the
// splice left it, holding b.
func (c rpCell) stagedLeg(uuid string, b *bins.Bin, steps ...resolvedStep) *orders.Order {
	c.t.Helper()
	delivery := steps[lastDropIndex(steps)].Node
	o := testdb.CreateOrder(c.t, c.db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.OrderType, o.Status = uuid, "line-1", OrderTypeComplex, StatusStaged
		o.ProcessNode, o.DeliveryNode, o.PayloadCode, o.StepsJSON = c.line.Name, delivery, c.part, string(mustJSON(c.t, steps))
	})
	testutil.MustNoErr(c.t, c.db.UpdateOrderVendor(o.ID, "V-"+uuid, "WAITING", ""), "vendor")
	testdb.HoldCart(c.t, c.db, o.ID, b.ID)
	_, err := c.db.Exec(`UPDATE bins SET claimed_by=$1 WHERE id=$2`, o.ID, b.ID)
	testutil.MustNoErr(c.t, err, "claim the carrier")
	return prReload(c.t, c.db, o.ID)
}

// release presses the station's RELEASE with count and answers the stored plan.
func (c rpCell) release(o *orders.Order, count int) []resolvedStep {
	c.t.Helper()
	c.d.HandleOrderRelease(c.d.syntheticEnvelope("line-1"), &protocol.OrderRelease{OrderUUID: o.EdgeUUID, RemainingUOP: &count})
	got := prReload(c.t, c.db, o.ID)
	steps, ok := decodeSteps(got.StepsJSON)
	if !ok {
		c.t.Fatalf("stored plan: %s", got.StepsJSON)
	}
	return steps
}

// lastAppend is the blocks of the segment the release appended, "" for none.
func (c rpCell) lastAppend() (string, bool) {
	calls := c.backend.ReleaseCalls()
	if len(calls) == 0 {
		return "", false
	}
	last := calls[len(calls)-1]
	return blockShape(last.Blocks), last.Complete
}

// TestRepoint_AnEmptySentToItsKanbanDropsTheMarketsLaneWait is the STOP A shape:
// an empty released off a supported line, its planned drop a market slot behind
// the market lane's wait, goes to the kanban, which has no lanes. The rest of
// the route is planned again: no lane wait is left in front of the kanban's
// slot, and the release sends the lift and the set-down, sealed.
func TestRepoint_AnEmptySentToItsKanbanDropsTheMarketsLaneWait(t *testing.T) {
	t.Parallel()
	db0 := "RPA"
	c := newRPCell(t, db0, protocol.ClaimRoleConsume, db0+"-MKT", "")
	mkt, lane, slots := rpLanedGroup(t, c.db, db0+"-MKT")
	b := c.carrier(40)
	drop := resolvedStep{Action: protocol.ActionDropoff, Node: slots[0].Name, Group: mkt.Name}
	o := c.stagedLeg("rpa-leg", b, rlWait(c.line.Name, WaitKindStation), vsPick(c.line.Name), laneWait(mkt, lane), drop)

	steps := c.release(o, 0)
	if got := shape(steps); !strings.HasPrefix(got, "wait@RPA-LINE:station pickup@RPA-LINE dropoff@RPA-KAN-") || strings.Contains(got, ":lane") {
		t.Errorf("stored plan %q, want the lift and a kanban slot with no lane wait", got)
	}
	if err := c.d.assertEachWaitGatesItsEntry(steps); err != nil {
		t.Errorf("the stored plan's waits: %v", err)
	}
	if blocks, sealed := c.lastAppend(); !strings.Contains(blocks, "@RPA-KAN-") || !sealed {
		t.Errorf("the release appended %q (sealed %v), want the lift and the kanban drop, sealed", blocks, sealed)
	}
	if fresh := prReload(t, c.db, o.ID); cartsPlanMismatch(steps, fresh.Carts) != nil {
		t.Errorf("the carts after the re-plan: %v", cartsPlanMismatch(steps, fresh.Carts))
	}
}

// TestRepoint_APartialHomeInALanedGroupIsEnteredThroughItsGate: the planned drop
// is a node in no lane, so the plan has no lane wait; the partial home is a
// laned group. The re-point puts the lane's wait in front of the home's slot:
// the station's release sends the lift and the wait at the home's waiting spot,
// and the set-down in the lane is the gate's append, not the station's.
func TestRepoint_APartialHomeInALanedGroupIsEnteredThroughItsGate(t *testing.T) {
	t.Parallel()
	c := newRPCell(t, "RPB", protocol.ClaimRoleProduce, "RPB-OUT", "RPB-PH")
	out := prNode(t, c.db, "RPB-OUT")
	_, lane, _ := rpLanedGroup(t, c.db, "RPB-PH")
	b := c.carrier(40)
	o := c.stagedLeg("rpb-leg", b, rlWait(c.line.Name, WaitKindStation), vsPick(c.line.Name), vsDrop(out.Name))

	before := len(c.backend.ReleaseCalls())
	steps := c.release(o, 40)
	f := lastDropIndex(steps)
	if f < 1 || !strings.HasPrefix(steps[f].Node, "RPB-PH-L1-") {
		t.Fatalf("stored plan %q, want the unfinished carrier at its partial home", shape(steps))
	}
	if w := steps[f-1]; w.Action != protocol.ActionWait || w.WaitKind != WaitKindLane || w.WaitLane != lane.ID {
		t.Errorf("stored plan %q: the home's slot has no wait for its lane in front of it", shape(steps))
	}
	var appends []string
	for _, call := range c.backend.ReleaseCalls()[before:] {
		appends = append(appends, fmt.Sprintf("[%s sealed=%v]", blockShape(call.Blocks), call.Complete))
	}
	want := fmt.Sprintf("[JackLoad@RPB-LINE Wait@RPB-PH-WAIT sealed=false] [JackUnload@%s sealed=true]", steps[f].Node)
	if got := strings.Join(appends, " "); got != want {
		t.Errorf("appends %s, want %s: the station's release stops at the lane's wait and the gate lets it in", got, want)
	}
}

// rpTurnCell is an rpCell whose dispatcher turns tables.
func rpTurnCell(t *testing.T, prefix, partialHome string) (rpCell, turnRig) {
	t.Helper()
	r := newTurnRig(t)
	c := rpCell{t: t, db: r.db, d: r.d, backend: r.backend, prefix: prefix, part: prefix + "-PART",
		line: prNode(t, r.db, prefix+"-LINE")}
	testutil.MustNoErr(t, r.db.CreatePayload(&payloads.Payload{Code: c.part, UOPCapacity: 100}), "the part")
	_, err := r.db.ReplacePlantClaimsFromStation(prefix+"-PROC", "line-1",
		[]plantclaims.StyleRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STYLE", IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: prefix + "-PROC", StyleID: prefix + "-STYLE", CoreNodeName: c.line.Name,
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobot, PayloadCode: c.part,
			OutboundDestination: prefix + "-OUT", PartialDestination: partialHome}}, nil, 0)
	testutil.MustNoErr(t, err, "the line's claim")
	testutil.MustNoErr(t, r.db.QueryRow(`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, prefix+"-KD").Scan(&c.kd), "type")
	return c, r
}

// turnBeforeSetDown sets node to turn the table before the set-down.
func turnBeforeSetDown(t *testing.T, r turnRig, node string) {
	t.Helper()
	n, err := r.db.GetNodeByDotName(node)
	n = testutil.Must(t, n, err, "node "+node)
	testutil.MustNoErr(t, r.db.SetNodeProperty(n.ID, PropTableTurnSide, TableTurnSideSetDown), "side")
	r.turnAt(t, node)
}

// TestRepoint_TheTurnWaitFollowsTheDrop: a drop moved onto a node that turns the
// table before the set-down gains its turn wait; a drop moved off one loses the
// turn wait the splice put in front of it.
func TestRepoint_TheTurnWaitFollowsTheDrop(t *testing.T) {
	t.Parallel()
	t.Run("onto a turn node", func(t *testing.T) {
		t.Parallel()
		c, r := rpTurnCell(t, "RPC", "RPC-PH")
		out, home := prNode(t, c.db, "RPC-OUT"), prNode(t, c.db, "RPC-PH")
		turnBeforeSetDown(t, r, home.Name)
		b := c.carrier(40)
		o := c.stagedLeg("rpc-leg", b, rlWait(c.line.Name, WaitKindStation), vsPick(c.line.Name), vsDrop(out.Name))
		steps := c.release(o, 40)
		if got := shape(steps); got != "wait@RPC-LINE:station pickup@RPC-LINE wait@RPC-PH:turn dropoff@RPC-PH" {
			t.Errorf("stored plan %q, want a turn wait in front of the set-down at the home", got)
		}
	})
	t.Run("off a turn node", func(t *testing.T) {
		t.Parallel()
		c, r := rpTurnCell(t, "RPD", "RPD-PH")
		out, home := prNode(t, c.db, "RPD-OUT"), prNode(t, c.db, "RPD-PH")
		turnBeforeSetDown(t, r, out.Name)
		b := c.carrier(40)
		o := c.stagedLeg("rpd-leg", b, rlWait(c.line.Name, WaitKindStation), vsPick(c.line.Name),
			voWait(out.Name, WaitKindTurn), vsDrop(out.Name))
		steps := c.release(o, 40)
		if got := shape(steps); got != "wait@RPD-LINE:station pickup@RPD-LINE dropoff@"+home.Name {
			t.Errorf("stored plan %q, want the home's set-down with no turn wait left for %s", got, out.Name)
		}
	})
}

// TestRepoint_AVacateStampLandsOnTheHeldDrop: the leg lifts an empty off the
// line, sets it down in the market behind the lane's wait, then lifts its supply
// and sets it down on a node whose bin its partner has not lifted yet. Sent to
// the kanban instead, the empty's drop loses the lane wait, and the stamped drop
// is in the released segment now: the release is held, and the hold is written
// on the stamped drop where it stands in the new plan.
func TestRepoint_AVacateStampLandsOnTheHeldDrop(t *testing.T) {
	t.Parallel()
	c := newRPCell(t, "RPE", protocol.ClaimRoleConsume, "RPE-MKT", "")
	mkt, lane, slots := rpLanedGroup(t, c.db, "RPE-MKT")
	sup, dst := prNode(t, c.db, "RPE-SUP"), prNode(t, c.db, "RPE-DST")
	resident := makeEmptyBin(t, c.db, dst.ID, "RPE-RESIDENT")
	partner := testdb.CreateOrder(t, c.db, func(o *orders.Order) { o.EdgeUUID, o.Status = "rpe-partner", StatusDispatched })
	b := c.carrier(40)
	drop := resolvedStep{Action: protocol.ActionDropoff, Node: slots[0].Name, Group: mkt.Name}
	held := voDrop(dst.Name, &vacateStamp{Node: dst.Name, Bin: resident.ID, Partner: partner.ID})
	o := c.stagedLeg("rpe-leg", b, rlWait(c.line.Name, WaitKindStation), vsPick(c.line.Name), laneWait(mkt, lane), drop,
		vsPick(sup.Name), held)

	before := len(c.backend.ReleaseCalls())
	steps := c.release(o, 0)
	if n := len(c.backend.ReleaseCalls()) - before; n != 0 {
		t.Errorf("%d appends, want the release held until the resident leaves %s", n, dst.Name)
	}
	f := lastDropIndex(steps)
	if f < 0 || steps[f].Node != dst.Name || steps[f].Vacate == nil || steps[f].Vacate.HeldAt == nil {
		t.Errorf("stored plan %q: the stamped drop at %s carries no hold", shape(steps), dst.Name)
	}
}

// createdUnload is where the first create the fleet was sent sets its carrier
// down: the location of its first unload block, "" when nothing was created.
func createdUnload(backend *testdb.MockTrackingBackend) string {
	for _, c := range backend.CreateRequests() {
		for _, b := range c.Blocks {
			if strings.HasPrefix(b.BinTask, "JackUnload") {
				return b.Location
			}
		}
	}
	return ""
}

// TestRepoint_APlanningRepointReachesTheCreate: a planning door's re-point
// edits the plan the pass sends. A leg with no wait ahead of its drop goes to
// the fleet whole at its create, so a divert written only to the stored plan
// never reached the robot: a contained part's leg drove to finished goods, a
// loader's return to the outbound it was parked away from.
func TestRepoint_APlanningRepointReachesTheCreate(t *testing.T) {
	t.Parallel()
	t.Run("containment at a node", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		prod, fgn, hold := containmentFixture(t, db)
		testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: "PART-QC", UOPCapacity: 10}), "the part")
		d, backend := newTestDispatcherWithResolver(t, db)
		makeLoaderBin(t, db, "PART-QC", prod.ID, "qc-nowait-bin", 4, time.Now().UTC())
		prSubmitLeg(d, "qc-nowait", "", "PART-QC", prod.Name, prPick(prod.Name), prDrop(fgn.Name))
		prScanPass(t, d, db, "qc-nowait")
		if got := createdUnload(backend); got != hold.Name {
			t.Errorf("the create sets the contained part down at %q, want containment %s", got, hold.Name)
		}
	})
	t.Run("containment in a group", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		prod, fgn, _ := containmentFixture(t, db)
		testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: "PART-QC", UOPCapacity: 10}), "the part")
		d, backend := newTestDispatcherWithResolver(t, db)
		area, err := db.CreateNodeGroup("QC-AREA")
		testutil.MustNoErr(t, err, "the containment area")
		for _, n := range []string{"QC-AREA-1", "QC-AREA-2"} {
			testutil.MustNoErr(t, db.CreateNode(&nodes.Node{Name: n, Enabled: true, ParentID: &area}), n)
		}
		_, err = db.DB.Exec(`UPDATE style_claims SET containment_destination = 'QC-AREA' WHERE core_node_name = 'PROD-QC'`)
		testutil.MustNoErr(t, err, "contain into the area")
		makeLoaderBin(t, db, "PART-QC", prod.ID, "qc-area-bin", 4, time.Now().UTC())
		prSubmitLeg(d, "qc-area", "", "PART-QC", prod.Name, prPick(prod.Name), prDrop(fgn.Name))
		prScanPass(t, d, db, "qc-area")
		prScanPass(t, d, db, "qc-area")
		if got := createdUnload(backend); !strings.HasPrefix(got, "QC-AREA-") {
			t.Errorf("the create sets the contained part down at %q, want a slot of the containment area", got)
		}
	})
	t.Run("a loader's return parked at its buffer", func(t *testing.T) {
		t.Parallel()
		db := testDB(t)
		home, buffer, outbound, _ := parkFixture(t, db)
		d, backend := newTestDispatcherWithResolver(t, db)
		makeLoaderBin(t, db, "PART-X", home.ID, "park-create-bin", 4, time.Now().UTC())
		makeInFlightTo(t, db, "park-create-restock", home.Name)
		prSubmitLeg(d, "park-create", "", "PART-X", "", prPick(home.Name), prDrop(outbound.Name))
		prScanPass(t, d, db, "park-create")
		if got := createdUnload(backend); got != buffer.Name {
			t.Errorf("the create sets the return down at %q, want the buffer %s it was parked at", got, buffer.Name)
		}
	})
}

// TestByContents_ACarrierBroughtBackToItsPositionHasNotLeft: a tool change's
// carry-over round trip lifts the cell's carrier at START, sets it down at
// staging, and brings it back after Tooling done. With no wait before the lift
// its drop is chosen at the reserve, and the carrier's set-down for good is
// its own position: an unfinished carrier whose claim names a partial home
// still comes back, or nothing refills the position.
func TestByContents_ACarrierBroughtBackToItsPositionHasNotLeft(t *testing.T) {
	t.Parallel()
	c := newRPCell(t, "RPF", protocol.ClaimRoleProduce, "RPF-OUT", "RPF-PH")
	prNode(t, c.db, "RPF-PH")
	stage := prNode(t, c.db, "RPF-STAGE")
	c.carrier(40)
	prSubmitLeg(c.d, "rpf-round-trip", "", c.part, c.line.Name, prPick(c.line.Name), prDrop(stage.Name),
		prWait(stage.Name), prPick(stage.Name), prDrop(c.line.Name))
	o := prReloadUUID(t, c.db, "rpf-round-trip")
	steps, ok := decodeSteps(o.StepsJSON)
	if !ok {
		t.Fatalf("steps: %s", o.StepsJSON)
	}
	if _, _, st := c.d.reserveComplexSources(o, steps); st.done {
		t.Fatalf("the reserve stopped: %v", st.err)
	}
	got, _ := decodeSteps(prReload(t, c.db, o.ID).StepsJSON)
	if f := lastDropIndex(got); f < 0 || got[f].Node != c.line.Name {
		t.Errorf("stored plan %q, want the carrier back on %s", shape(got), c.line.Name)
	}
}
