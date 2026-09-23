//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// drain_window_intake_docker_test.go — the automatic unloader pull (U1) through
// the real intake door, against a real Postgres.
//
// The envelope is the one the Edge builds for a U1: operator_demand_unloader.go
// calls CreateRetrieveOrder(&nodeID, false, 1, window, loader.InboundSource(),
// "", "standard", payload, false, true, orders.NoDemand()), and
// createRetrieveOrder (shingo-edge/orders/manager_create.go) turns that into the
// OrderRequest below field for field. A finder test that builds the order by
// hand cannot catch intake stamping something the finder then reads; this one
// goes through CreateInboundOrder and the planner.

// intakeEdgeU1 builds a consume window whose only reachable carrier of the part
// is a partial (400 of 1000), sends the Edge's U1 envelope for it through
// HandleOrderRequest, and returns the order after the scanner-mirror pass.
func intakeEdgeU1(t *testing.T, uuid string, acceptPartials bool) (*orders.Order, *bins.Bin) {
	t.Helper()
	db := testDB(t)
	storage, _, bp := setupTestData(t, db)

	window := &nodes.Node{Name: "D1-SMN_001", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(window), "create window")
	loaderID, err := db.CreateLoader(store.Loader{
		Name: "D1-UNLOADER", Role: loaders.RoleConsume, Layout: loaders.LayoutSharedWindow, Replenishment: "operator",
		AcceptPartials: acceptPartials,
	})
	testutil.MustNoErr(t, err, "create consume loader")
	testutil.MustNoErr(t, db.UpsertLoaderHome(store.LoaderHome{LoaderID: loaderID, PositionNodeID: window.ID}),
		"window home")

	partial := makeLoaderBin(t, db, bp.Code, storage.ID, "D1-PARTIAL", 400, time.Now().UTC().Add(-time.Hour))
	// A known capacity, so a park is the partial being judged partial and not an
	// unknown capacity being refused.
	if partial.UOPCapacity != 1000 || partial.UOPRemaining != 400 {
		t.Fatalf("fixture carrier is %d of %d, want 400 of 1000", partial.UOPRemaining, partial.UOPCapacity)
	}

	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID:       uuid,
		OrderType:       OrderTypeRetrieve,
		PayloadDesc:     bp.Description,
		PayloadCode:     bp.Code,
		RetrieveEmpty:   false,
		Quantity:        1,
		DeliveryNode:    window.Name,
		SourceNode:      "", // blank inbound source: Core's plant-wide FIFO
		StagingNode:     "",
		LoadType:        "standard",
		SkipAutoConfirm: true,
		OriginID:        "",
		OriginClass:     protocol.OriginClassNoDemand,
	})

	o := dispatchSimpleViaScanner(t, d, db, uuid)
	if o.OriginClass != protocol.OriginClassNoDemand {
		t.Fatalf("origin_class = %q, want %q — the fixture is not the production U1", o.OriginClass, protocol.OriginClassNoDemand)
	}
	return o, partial
}

func TestIntake_EdgeU1Envelope_PartialParksForAFullCarrier(t *testing.T) {
	t.Parallel()
	o, partial := intakeEdgeU1(t, "d1-u1-intake", false)
	if o.BinID != nil {
		t.Fatalf("U1 claimed bin %d (partial is %d) — a drain window is fed full carriers only", *o.BinID, partial.ID)
	}
	if o.QueueCause != string(CauseFinderNoFullCarrier) {
		t.Errorf("status=%s queue_cause=%q, want %q", o.Status, o.QueueCause, CauseFinderNoFullCarrier)
	}
}

// The same envelope at an unloader configured with accept_partials: the
// partial is claimed.
func TestIntake_EdgeU1Envelope_AcceptPartials_ClaimsThePartial(t *testing.T) {
	t.Parallel()
	o, partial := intakeEdgeU1(t, "d1-u1-intake-partials", true)
	if o.BinID == nil || *o.BinID != partial.ID {
		t.Fatalf("status=%s queue_cause=%q bin=%v, want the partial %d claimed", o.Status, o.QueueCause, o.BinID, partial.ID)
	}
}
