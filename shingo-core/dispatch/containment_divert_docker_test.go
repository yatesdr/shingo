//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// containmentFixture builds the smallest plant the divert speaks: a producing
// process node whose claim routes PART-QC's containment to a hold node, the
// FG outbound the claim diverts FROM, and the containment flag on.
func containmentFixture(t *testing.T, db *store.DB) (prod, fgn, hold *nodes.Node) {
	t.Helper()
	setupTestData(t, db)

	prod = &nodes.Node{Name: "PROD-QC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(prod), "create process node")
	fgn = &nodes.Node{Name: "FGN-QC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(fgn), "create FG outbound")
	hold = &nodes.Node{Name: "QC-HOLD", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(hold), "create containment node")

	_, err := db.DB.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code,
		 containment_destination, outbound_destination)
		VALUES ('P-QC', 'S-QC', 'PROD-QC', 'produce', 'auto', 'PART-QC', 'QC-HOLD', 'FGN-QC')`)
	testutil.MustNoErr(t, err, "seed containment-routed claim")

	testutil.MustNoErr(t, db.SetPayloadContainment("PART-QC", "quality alert", "tester", true),
		"activate containment")
	return prod, fgn, hold
}

// containmentOrder inserts a complex order whose final dropoff is the FG
// outbound, with steps persisted so the re-point patches them.
func containmentOrder(t *testing.T, db *store.DB, uuid, fgnName, prodName string, binID *int64) *orders.Order {
	t.Helper()
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "test", OrderType: protocol.OrderTypeRetrieve, Status: "staged",
		Quantity: 1, SourceNode: "SRC-QC", DeliveryNode: fgnName, PayloadCode: "PART-QC",
		ProcessNode: prodName, BinID: binID,
		StepsJSON: `[{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"` + prodName + `"},` +
			`{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"` + fgnName + `"}]`,
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order %s: %v", uuid, err)
	}
	return o
}

// containmentSteps is the resolved shape of that plan: an interior dropoff at
// the process node and the FG-bound final leg the divert scopes against.
func containmentSteps(fgnName, prodName string) []resolvedStep {
	return []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC-QC"},
		{Action: protocol.ActionDropoff, Node: prodName},
		{Action: protocol.ActionPickup, Node: "SRC-QC"},
		{Action: protocol.ActionDropoff, Node: fgnName},
	}
}

// TestPlaceForContainment_MultiBinJunctionRepoints pins the junction half of
// the divert. A multi-bin complex order's bins land at their JUNCTION
// dest_node (applyMultiBinArrivalForOrder), not at the order's delivery_node
// - so re-pointing only the order row lands every FG-bound bin at FG with
// the divert a silent no-op. Every junction row destined to the diverted
// node re-points with it; the bins bound for the interior consuming node
// stay put.
func TestPlaceForContainment_MultiBinJunctionRepoints(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, fgn, hold := containmentFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	fgBin := makeLoaderBin(t, db, "PART-QC", fgn.ID, "qc-fg-bin", 4, time.Now().UTC())
	cellBin := makeLoaderBin(t, db, "PART-QC", fgn.ID, "qc-cell-bin", 4, time.Now().UTC())

	o := containmentOrder(t, db, "qc-multi-1", fgn.Name, "PROD-QC", &fgBin.ID)
	testutil.MustNoErr(t, db.ReplaceOrderBins(o.ID, []orders.OrderBinRow{
		{BinID: cellBin.ID, StepIndex: 0, Action: protocol.ActionPickup, NodeName: "SRC-QC", DestNode: "PROD-QC"},
		{BinID: fgBin.ID, StepIndex: 2, Action: protocol.ActionPickup, NodeName: "SRC-QC", DestNode: fgn.Name},
	}), "seed junction rows")

	if st := d.placeForContainment(o, containmentSteps(fgn.Name, "PROD-QC")); st != nil {
		t.Fatalf("divert parked instead of re-pointing: %+v", st)
	}

	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if got.DeliveryNode != hold.Name {
		t.Fatalf("DeliveryNode = %q, want %q", got.DeliveryNode, hold.Name)
	}
	rows, err := db.ListOrderBins(o.ID)
	testutil.MustNoErr(t, err, "list junction rows")
	if len(rows) != 2 {
		t.Fatalf("junction rows = %d, want 2", len(rows))
	}
	for _, r := range rows {
		want := hold.Name
		if r.BinID == cellBin.ID {
			want = "PROD-QC"
		}
		if r.DestNode != want {
			t.Errorf("bin %d junction DestNode = %q, want %q", r.BinID, r.DestNode, want)
		}
	}
	if want := `[{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"PROD-QC"},{"action":"pickup","node":"SRC-QC"},{"action":"dropoff","node":"QC-HOLD"}]`; got.StepsJSON != want {
		t.Errorf("StepsJSON = %s\nwant          %s", got.StepsJSON, want)
	}

	// Replay: the order row is already re-pointed, the junction patch still
	// runs (idempotent), and nothing moves.
	if st := d.placeForContainment(got, containmentSteps(fgn.Name, "PROD-QC")); st != nil {
		t.Fatalf("replay parked: %+v", st)
	}
	rows2, err := db.ListOrderBins(o.ID)
	testutil.MustNoErr(t, err, "re-list junction rows")
	for _, r := range rows2 {
		want := hold.Name
		if r.BinID == cellBin.ID {
			want = "PROD-QC"
		}
		if r.DestNode != want {
			t.Errorf("after replay: bin %d junction DestNode = %q, want %q", r.BinID, r.DestNode, want)
		}
	}
}

// TestPlaceForContainment_SingleBinRepoints pins the plain shape: a
// single-bin FG leg re-points, junction rows absent (the loop no-ops).
func TestPlaceForContainment_SingleBinRepoints(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, fgn, hold := containmentFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	fgBin := makeLoaderBin(t, db, "PART-QC", fgn.ID, "qc-single-bin", 4, time.Now().UTC())
	o := containmentOrder(t, db, "qc-single-1", fgn.Name, "PROD-QC", &fgBin.ID)

	if st := d.placeForContainment(o, containmentSteps(fgn.Name, "PROD-QC")); st != nil {
		t.Fatalf("divert parked instead of re-pointing: %+v", st)
	}
	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if got.DeliveryNode != hold.Name {
		t.Fatalf("DeliveryNode = %q, want %q", got.DeliveryNode, hold.Name)
	}
	rows, err := db.ListOrderBins(o.ID)
	testutil.MustNoErr(t, err, "list order bins")
	if len(rows) != 0 {
		t.Fatalf("single-bin order grew %d junction rows", len(rows))
	}
}

// TestPlaceForContainment_ScopeRefusal pins the scope check: a leg whose
// final dropoff is NOT the claim's FG outbound (the interior consuming
// dropoff, a swap return) passes through untouched.
func TestPlaceForContainment_ScopeRefusal(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, fgn, _ := containmentFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	o := containmentOrder(t, db, "qc-scope-1", fgn.Name, "PROD-QC", nil)
	steps := []resolvedStep{
		{Action: protocol.ActionPickup, Node: "SRC-QC"},
		{Action: protocol.ActionDropoff, Node: "PROD-QC"},
	}
	if st := d.placeForContainment(o, steps); st != nil {
		t.Fatalf("a non-FG leg parked: %+v", st)
	}
	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if got.DeliveryNode != fgn.Name {
		t.Fatalf("DeliveryNode = %q, want untouched %q", got.DeliveryNode, fgn.Name)
	}
}

// TestPlaceForContainment_FlagOff_NoDivert pins the flag guard: with the
// payload's containment deactivated nothing re-points, not even an order
// whose claim carries a route.
func TestPlaceForContainment_FlagOff_NoDivert(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	_, fgn, _ := containmentFixture(t, db)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	testutil.MustNoErr(t, db.SetPayloadContainment("PART-QC", "alert cleared", "tester", false),
		"deactivate containment")

	o := containmentOrder(t, db, "qc-flagoff-1", fgn.Name, "PROD-QC", nil)
	if st := d.placeForContainment(o, containmentSteps(fgn.Name, "PROD-QC")); st != nil {
		t.Fatalf("a flag-off divert parked: %+v", st)
	}
	got, err := db.GetOrder(o.ID)
	testutil.MustNoErr(t, err, "reload order")
	if got.DeliveryNode != fgn.Name {
		t.Fatalf("DeliveryNode = %q, want untouched %q", got.DeliveryNode, fgn.Name)
	}
}
