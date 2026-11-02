//go:build docker

package dispatch

// order_carts_pins_docker_test.go — U0 of the order-carts stream, dispatch's
// readers. Each pin says what it asserts at main f8f22f19 (red, green, or
// unknown before it ran); the predictions are in the design folder's PINS.md.

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestOrderCartsPin_AConsumeSwapLiftingAFullTakesTheStrictGroup: a consume
// swap lifts the drained cart off the line (near-empty: the relaxed group) and
// a full one from storage (the payload's own group). The robot lifts both, so
// the order takes the strict group. RED at main: capabilityBin reads
// orders.bin_id, the drained cart, and answers the relaxed group
// (robot_group.go:264-279).
func TestOrderCartsPin_AConsumeSwapLiftingAFullTakesTheStrictGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	_, err := db.DB.Exec(`UPDATE payloads SET robot_group='GRP-STRICT', near_empty_robot_group='GRP-ANY',
		near_empty_enabled=true, near_empty_threshold_pct=20, uop_capacity=100 WHERE code=$1`, sd.Payload.Code)
	testutil.MustNoErr(t, err, "the payload's groups")
	drained := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "CARTS-DRAINED")
	full := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CARTS-FULL")
	_, err = db.DB.Exec(`UPDATE bins SET uop_remaining = CASE WHEN id=$1 THEN 0 ELSE 100 END WHERE id IN ($1, $2)`,
		drained.ID, full.ID)
	testutil.MustNoErr(t, err, "the counts")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.PayloadCode = "carts-group", protocol.OrderTypeComplex, sd.Payload.Code
		o.SourceNode, o.DeliveryNode, o.ProcessNode = sd.LineNode.Name, sd.LineNode.Name, sd.LineNode.Name
		o.BinID = &drained.ID
		o.StepsJSON = `[{"action":"pickup","node":"` + sd.LineNode.Name + `"},{"action":"dropoff","node":"OUT-CARTS"},` +
			`{"action":"pickup","node":"` + sd.StorageNode.Name + `"},{"action":"dropoff","node":"` + sd.LineNode.Name + `"}]`
	})
	testdb.SeedCartRow(t, db, o.ID, drained.ID, 0, 1)
	testdb.SeedCartRow(t, db, o.ID, full.ID, 2, 3)

	if got := d.robotGroupForOrder(o); got != "GRP-STRICT" {
		t.Fatalf("the swap's robot group = %q, want GRP-STRICT (it lifts a full cart)", got)
	}
}

// TestOrderCartsPin_BinForStepResolvesARelayFromItsRow: a single_robot swap
// re-lifts the fresh cart at inbound staging; the step that re-lifts it names
// the cart from its row, not from a node. RED at main: the junction has no
// relay row (one row per claimed cart), so the answer is positional (atNode,
// binID 0; bin_for_step.go relay arm).
func TestOrderCartsPin_BinForStepResolvesARelayFromItsRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	d, _ := newTestDispatcherWithResolver(t, db)
	in := &nodes.Node{Name: "CARTS-IN-STAGE", Enabled: true}
	out := &nodes.Node{Name: "CARTS-OUT-STAGE", Enabled: true}
	dest := &nodes.Node{Name: "CARTS-OUT-DEST", Enabled: true}
	for _, n := range []*nodes.Node{in, out, dest} {
		testutil.MustNoErr(t, db.CreateNode(n), "create "+n.Name)
	}
	fresh := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CARTS-RELAY-F")
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "CARTS-RELAY-O")
	d.HandleComplexOrderRequest(testdb.Envelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "carts-relay", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: sd.LineNode.Name,
		Steps: []protocol.ComplexOrderStep{
			{Action: "pickup", Node: sd.StorageNode.Name}, // 0 lift F
			{Action: "dropoff", Node: in.Name},            // 1
			{Action: "wait", Node: sd.LineNode.Name},      // 2
			{Action: "pickup", Node: sd.LineNode.Name},    // 3 lift O
			{Action: "dropoff", Node: out.Name},           // 4
			{Action: "pickup", Node: in.Name},             // 5 re-lift F: the relay
			{Action: "dropoff", Node: sd.LineNode.Name},   // 6
			{Action: "pickup", Node: out.Name},            // 7 re-lift O
			{Action: "dropoff", Node: dest.Name},          // 8
		},
	})
	prScanPass(t, d, db, "carts-relay")
	o := prReloadUUID(t, db, "carts-relay")
	if o.VendorOrderID == "" {
		t.Fatalf("the swap did not dispatch: %s (%s)", o.Status, o.QueueReason)
	}
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "read the plan")

	sb, err := d.binForStep(o, steps, 5)
	testutil.MustNoErr(t, err, "bin for the relay step")
	if sb.binID != fresh.ID {
		t.Fatalf("the relay step's cart = bin %d (at node %q), want the fresh cart %d from its row", sb.binID, sb.atNode, fresh.ID)
	}
}

// TestOrderCartsPin_AContainmentDivertKeepsEachCartsDropWithItsPlan is the
// brief's "run first, unknown today": a two-cart order whose carts are both set
// down at the FG outbound, at two steps, diverted to containment. The plan
// writer re-points the LAST dropoff only (applyFinalDeliveryNode); whatever
// each cart's record says after the divert must be the node its own drop step
// names in the plan. Ran at main: see PINS.md for what it found.
func TestOrderCartsPin_AContainmentDivertKeepsEachCartsDropWithItsPlan(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, fgn, hold := containmentFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	first := makeLoaderBin(t, db, "PART-QC", fgn.ID, "qc-first", 4, time.Now().UTC())
	second := makeLoaderBin(t, db, "PART-QC", fgn.ID, "qc-second", 4, time.Now().UTC())
	o := &orders.Order{
		EdgeUUID: "carts-divert", StationID: "test", OrderType: protocol.OrderTypeRetrieve, Status: "staged",
		Quantity: 1, SourceNode: "SRC-QC", DeliveryNode: fgn.Name, PayloadCode: "PART-QC",
		ProcessNode: "PROD-QC", BinID: &first.ID,
		StepsJSON: `[{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"` + fgn.Name + `"},` +
			`{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"` + fgn.Name + `"}]`,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "the order")
	testdb.SeedCartRow(t, db, o.ID, first.ID, 0, 1)
	testdb.SeedCartRow(t, db, o.ID, second.ID, 2, 3)
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC-QC"}, {Action: protocol.ActionDropoff, Node: fgn.Name},
		{Action: protocol.ActionPickup, Node: "SRC-QC"}, {Action: protocol.ActionDropoff, Node: fgn.Name},
	}

	if st := d.placeForContainment(o, steps); st != nil {
		t.Fatalf("the divert parked: %+v", st)
	}

	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload")
	var plan []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(got.StepsJSON), &plan), "read the plan")
	t.Logf("after the divert: plan drops at %s (step 1) and %s (step 3); the order delivers to %s; containment is %s",
		plan[1].Node, plan[3].Node, got.DeliveryNode, hold.Name)
	drops := testdb.CartDropNodes(t, db, o.ID)
	for bin, want := range map[int64]string{first.ID: plan[1].Node, second.ID: plan[3].Node} {
		if len(drops[bin]) != 1 || drops[bin][0] != want {
			t.Errorf("bin %d's record says it is set down at %v; its drop step in the plan names %s", bin, drops[bin], want)
		}
	}
}

const (
	// costMultiCartAllocation: recordClaimedBins for a two-cart order
	// (UpdateOrderBinID; ReplaceOrderBins' BEGIN, owner-live CTE, an INSERT per
	// cart, COMMIT). Budget 1.
	costMultiCartAllocation = 6
	// costRobotGroup: robotGroupForOrder for a cart-carrying order (GetOrder,
	// GetBin). Budget 1.
	costRobotGroup = 2
)

// cartsCostOrder is a live two-cart order on a database with a counting handle
// beside it: drained at the line (A), full at storage (B), and a dispatcher on
// the counting handle.
func cartsCostOrder(t *testing.T) (*Dispatcher, *store.QueryCounter, *orders.Order, []resolvedStep, [2]int64) {
	t.Helper()
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	a := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "CARTS-COST-A")
	b := testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.StorageNode.ID, "CARTS-COST-B")
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: sd.LineNode.Name}, {Action: protocol.ActionDropoff, Node: "CARTS-COST-W"},
		{Action: protocol.ActionPickup, Node: sd.StorageNode.Name}, {Action: protocol.ActionDropoff, Node: sd.LineNode.Name},
	}
	plan, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "the plan")
	o := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.OrderType, o.Status, o.PayloadCode = "carts-cost", protocol.OrderTypeComplex, "sourcing", sd.Payload.Code
		o.SourceNode, o.DeliveryNode, o.ProcessNode = sd.LineNode.Name, sd.LineNode.Name, sd.LineNode.Name
		o.StepsJSON = string(plan)
	})
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open the counting handle")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, testdb.NewSuccessBackend())
	return d, counter, o, steps, [2]int64{a.ID, b.ID}
}

func TestOrderCartsCost_MultiCartAllocation(t *testing.T) {
	t.Parallel()
	d, counter, o, steps, carts := cartsCostOrder(t)
	claimed := []claimedBin{
		{binID: carts[0], stepIndex: 0, nodeName: steps[0].Node},
		{binID: carts[1], stepIndex: 2, nodeName: steps[2].Node},
	}
	counter.Reset()
	testutil.MustNoErr(t, d.allocator.recordClaimedBins(o, steps, claimed), "record the carts")
	n := counter.Count()
	t.Logf("COST: multi-cart allocation %d statements: %s", n, testutil.StatementBreakdown(counter.Statements()))
	if n != costMultiCartAllocation {
		t.Fatalf("the multi-cart allocation took %d statements, pinned at %d", n, costMultiCartAllocation)
	}
}

func TestOrderCartsCost_RobotGroup(t *testing.T) {
	t.Parallel()
	d, counter, o, steps, carts := cartsCostOrder(t)
	claimed := []claimedBin{
		{binID: carts[0], stepIndex: 0, nodeName: steps[0].Node},
		{binID: carts[1], stepIndex: 2, nodeName: steps[2].Node},
	}
	testutil.MustNoErr(t, d.allocator.recordClaimedBins(o, steps, claimed), "record the carts")
	counter.Reset()
	d.robotGroupForOrder(o)
	n := counter.Count()
	t.Logf("COST: robot group %d statements: %s", n, testutil.StatementBreakdown(counter.Statements()))
	if n != costRobotGroup {
		t.Fatalf("the robot group took %d statements, pinned at %d", n, costRobotGroup)
	}
}
