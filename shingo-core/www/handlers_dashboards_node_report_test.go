//go:build docker

package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/dashboards"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// Characterization tests for apiDashboardNodeReport (handlers_dashboards.go).
//
// This endpoint had no test of any kind. It is registered OUTSIDE the auth
// group (router.go) because the chromeless kiosk page polls it, so it is
// public, and it feeds a wall display at Springfield.
//
// The assertions below are deliberately about the TRANSIT KEYING as much as the
// row shape. Both branches of this handler build their order lookups keyed by
// order id and index them with bin.ClaimedBy, which is a claiming ORDER id. They
// were once keyed by bin id, and because both are dense small integers the
// lookup never failed loudly — it answered about whichever order happened to
// share a number with the bin, and every transit row's destination and robot was
// drawn from an unrelated order and rendered as authoritative.
//
// The fixture therefore forces bin.ID != order.ID. Without that the test passes
// under either keying and pins nothing.

type nodeReportResp struct {
	LoaderName string `json:"loader_name"`
	Layout     string `json:"layout"`
	HomesCount int    `json:"homes_count"`
	Rows       []struct {
		NodeName      string `json:"node_name"`
		GroupName     string `json:"group_name"`
		Occupied      bool   `json:"occupied"`
		PayloadCode   string `json:"payload_code"`
		UOPRemaining  int    `json:"uop_remaining"`
		IsActiveStyle bool   `json:"is_active_style"`
	} `json:"rows"`
	Transit []struct {
		PayloadCode  string `json:"payload_code"`
		DestNode     string `json:"dest_node"`
		SourceNode   string `json:"source_node"`
		RobotID      string `json:"robot_id"`
		UOPRemaining int    `json:"uop_remaining"`
		IsEmpty      bool   `json:"is_empty"`
		IsPartial    bool   `json:"is_partial"`
	} `json:"transit"`
}

// nodeReportFixture builds a dedicated-positions loader with two home slots, a
// dashboard pointing at it, and one bin in transit claimed by an order whose id
// is deliberately different from the bin's.
func nodeReportFixture(t *testing.T, h *Handlers, db *store.DB, tag string) (dashboardID int64, dest, src string) {
	t.Helper()
	sd := testdb.SetupStandardData(t, db)

	// Its own payload rather than the shared PART-A: UOPCapacity is joined from
	// payloads.uop_capacity, and this fixture needs a real capacity to make the
	// 40/100 partial meaningful. Mutating the standard fixture's payload would
	// leak into every other test sharing the database.
	payload := &payloads.Payload{Code: tag + "-PART", Description: "node report", UOPCapacity: 100}
	testutil.MustNoErr(t, db.CreatePayload(payload), "create payload")

	group := &nodes.Node{Name: tag + "-GRP", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(group), "create group")
	posA := &nodes.Node{Name: tag + "-POS-A", Enabled: true, ParentID: &group.ID}
	posB := &nodes.Node{Name: tag + "-POS-B", Enabled: true, ParentID: &group.ID}
	testutil.MustNoErr(t, db.CreateNode(posA), "create posA")
	testutil.MustNoErr(t, db.CreateNode(posB), "create posB")

	loaderID, err := db.CreateLoader(loaders.Loader{
		Name: tag + "-LDR", Role: loaders.RoleProduce,
		Layout: loaders.LayoutDedicatedPositions, Replenishment: loaders.ReplenishmentOperator,
		InboundSource: tag + "-MKT",
	})
	testutil.MustNoErr(t, err, "create loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{
		LoaderID: loaderID, PositionNodeID: posA.ID, PayloadCode: payload.Code}), "home A")
	testutil.MustNoErr(t, db.UpsertLoaderHome(loaders.Home{
		LoaderID: loaderID, PositionNodeID: posB.ID}), "home B")

	// A bin parked on posA, so one row reports occupied and one does not.
	testdb.CreateBinAtNode(t, db, payload.Code, posA.ID, tag+"-PARKED")

	// Burn bin ids so the transit bin cannot share an id with its order. Without
	// this the bin-keyed and order-keyed lookups agree and the test pins nothing.
	for i := 0; i < 3; i++ {
		testdb.CreateBinAtNode(t, db, payload.Code, sd.StorageNode.ID, tag+"-FILLER-"+string(rune('A'+i)))
	}

	order := &orders.Order{
		EdgeUUID: tag + "-ORD", StationID: "st-1", OrderType: "retrieve",
		Status: "in_transit", PayloadCode: payload.Code,
		DeliveryNode: posB.Name, SourceNode: sd.StorageNode.Name,
		Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(order), "create order")

	transitNode, err := db.GetNodeByName(domain.TransitNodeName)
	testutil.MustNoErr(t, err, "lookup _TRANSIT")
	inFlight := &bins.Bin{
		BinTypeID: sd.BinType.ID, Label: tag + "-FLYING", NodeID: &transitNode.ID,
		Status: "available",
	}
	testutil.MustNoErr(t, db.CreateBin(inFlight), "create transit bin")
	// These four columns are seeded with SQL rather than through their normal
	// writers, and that is correct for a read-model fixture: bins.Create takes
	// only type/label/node/status (payload and UOP arrive on delivery), the
	// orders INSERT has no robot_id (the fleet assigns it), and ClaimBin is
	// gated on a pending reservation row. The handler only ever READS all four.
	_, cerr := db.DB.Exec(
		`UPDATE bins SET claimed_by=$1, payload_code=$2, uop_remaining=40 WHERE id=$3`,
		order.ID, payload.Code, inFlight.ID)
	testutil.MustNoErr(t, cerr, "seed the transit bin's claim and contents")
	_, rerr := db.DB.Exec(`UPDATE orders SET robot_id='AMR-77' WHERE id=$1`, order.ID)
	testutil.MustNoErr(t, rerr, "seed the order's robot")

	if inFlight.ID == order.ID {
		t.Fatalf("fixture is not discriminating: bin id == order id (%d); "+
			"a bin-keyed lookup would give the same answer as an order-keyed one", inFlight.ID)
	}

	cfg := json.RawMessage(`{"loader_id": ` + strconv.FormatInt(loaderID, 10) + `}`)
	dashboardID, err = h.engine.DashboardService().Create(dashboards.Input{
		Name: tag + " Board", Kind: "node-report", Config: cfg, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create dashboard")
	return dashboardID, posB.Name, sd.StorageNode.Name
}

func getNodeReport(t *testing.T, h *Handlers, dashboardID int64) nodeReportResp {
	t.Helper()
	req := chiReq(http.MethodGet, "/api/dashboards/"+strconv.FormatInt(dashboardID, 10)+"/node-report",
		map[string]string{"id": strconv.FormatInt(dashboardID, 10)})
	rec := httptest.NewRecorder()
	h.apiDashboardNodeReport(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var out nodeReportResp
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	return out
}

func TestApiDashboardNodeReport_HomesRowsAndCounts(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, _, _ := nodeReportFixture(t, h, db, "NRH")

	got := getNodeReport(t, h, id)
	if got.Layout != loaders.LayoutDedicatedPositions {
		t.Errorf("layout = %q, want %q", got.Layout, loaders.LayoutDedicatedPositions)
	}
	if got.HomesCount != 2 {
		t.Errorf("homes_count = %d, want 2", got.HomesCount)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("got %d rows, want one per home: %+v", len(got.Rows), got.Rows)
	}

	byNode := map[string]bool{}
	for _, r := range got.Rows {
		byNode[r.NodeName] = r.Occupied
		if r.GroupName != "NRH-GRP" {
			t.Errorf("row %s group = %q, want NRH-GRP (the position's parent)", r.NodeName, r.GroupName)
		}
	}
	if !byNode["NRH-POS-A"] {
		t.Error("POS-A has a bin parked on it and reports unoccupied")
	}
	if byNode["NRH-POS-B"] {
		t.Error("POS-B has no bin and reports occupied")
	}
}

// The keying pin. A transit row's destination, source and robot must come from
// the order that CLAIMS the bin.
func TestApiDashboardNodeReport_TransitReadsTheClaimingOrder(t *testing.T) {
	t.Parallel()
	h, db := testHandlers(t)
	id, wantDest, wantSrc := nodeReportFixture(t, h, db, "NRT")

	got := getNodeReport(t, h, id)
	if len(got.Transit) != 1 {
		t.Fatalf("got %d transit rows, want 1: %+v", len(got.Transit), got.Transit)
	}
	tr := got.Transit[0]
	if tr.DestNode != wantDest {
		t.Errorf("dest_node = %q, want %q — a transit row must read the order that claims the bin", tr.DestNode, wantDest)
	}
	if tr.SourceNode != wantSrc {
		t.Errorf("source_node = %q, want %q", tr.SourceNode, wantSrc)
	}
	if tr.RobotID != "AMR-77" {
		t.Errorf("robot_id = %q, want AMR-77", tr.RobotID)
	}
	// 40 of 100 remaining: a partial, which is what puts it on the board as a
	// return trip rather than an outbound delivery.
	if !tr.IsPartial {
		t.Error("is_partial = false; 40/100 remaining is a partial bin")
	}
	if tr.IsEmpty {
		t.Error("is_empty = true for a bin carrying a payload")
	}
}
