//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_outbound_payload_docker_test.go — a loader's outbound move (the L2) as
// Core receives it, from either Edge. An Edge from before the B3 close-out files
// it when the empty is confirmed, usually with a blank part, and Core fills the
// part from the bin at dispatch (payloadForDispatch); a current Edge files it at
// LOAD with the part loaded. Core takes both, and both reach the fleet with the
// robot group of what the carrier holds.

// TestLoaderOutbound_CoreAcceptsBothEdges is the mixed-version pin, both ways:
// the same move request from a loader window holding a loaded carrier, once
// blank (the old Edge) and once carrying the part (the new Edge).
func TestLoaderOutbound_CoreAcceptsBothEdges(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name    string
		payload func(code string) string
	}{
		{"old_edge_blank_at_the_confirm", func(string) string { return "" }},
		{"new_edge_loaded_at_the_load", func(code string) string { return code }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			_, lineNode, bp := setupTestData(t, db)
			bp.RobotGroup = "HEAVY-1500"
			testutil.MustNoErr(t, db.UpdatePayload(bp), "set robot group")
			window := &nodes.Node{Name: "PLK-WIN-" + tc.name, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(window), "create loader window")
			createTestBinAtNode(t, db, bp.Code, window.ID, "BIN-L2-"+tc.name)

			sim := simulator.New()
			d, _ := newTestDispatcher(t, db, sim)
			req := &protocol.OrderRequest{
				OrderUUID:    "l2-" + tc.name,
				OrderType:    "move",
				Quantity:     1,
				SourceNode:   window.Name,
				DeliveryNode: lineNode.Name,
				PayloadCode:  tc.payload(bp.Code),
			}
			body, err := json.Marshal(req)
			body = testutil.Must(t, body, err, "marshal request")
			env := testEnvelope()
			data, err := json.Marshal(protocol.Data{Body: body})
			env.Payload = testutil.Must(t, data, err, "marshal envelope")
			d.HandleOrderRequest(env, req)
			o := dispatchSimpleViaScanner(t, d, db, req.OrderUUID)

			got, err := db.GetOrder(o.ID)
			testutil.MustNoErr(t, err, "reload L2")
			if got.VendorOrderID == "" {
				t.Fatalf("L2 status %q, no vendor order — the move never reached the fleet", got.Status)
			}
			if group, _ := sim.RobotGroupFor(got.VendorOrderID); group != "HEAVY-1500" {
				t.Errorf("fleet robot group %q, want HEAVY-1500 (the loaded part's)", group)
			}
			if got.PayloadCode != bp.Code {
				t.Errorf("L2 records payload %q, want %q", got.PayloadCode, bp.Code)
			}
		})
	}
}

// TestPayloadForDispatch_ReadsTheBinOnlyForABlankNeed: payloadForDispatch reads
// the bin only when the order names no part and does not want an empty. An
// order that carries its part (every L2 from a current Edge) costs no
// statement; an empty-intent order stays blank without a read (the exclusion
// TestRetrieveEmpty_StaysPayloadAgnostic states); a blank full-intent order
// reads its bin and records the part.
func TestPayloadForDispatch_ReadsTheBinOnlyForABlankNeed(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	storageNode, lineNode, bp := setupTestData(t, db)

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, testdb.NewTrackingBackend())

	// Each order points at a bin of its own and holds its reservation: a pending
	// order pointing at a bin it holds nothing on is a pointer wedge (C1 sweeps
	// top-level pending orders too), and the pointer is what payloadForDispatch
	// reads, so it stays.
	mk := func(uuid, payload, intent string) *orders.Order {
		bin := createTestBinAtNode(t, db, bp.Code, storageNode.ID, "BIN-"+uuid)
		o := &orders.Order{
			EdgeUUID: uuid, StationID: "test-station", OrderType: protocol.OrderTypeMove,
			Status: protocol.StatusPending, Quantity: 1, SourceNode: storageNode.Name,
			DeliveryNode: lineNode.Name, PayloadCode: payload, SourceIntent: intent, BinID: &bin.ID,
			OriginClass: protocol.OriginClassNoDemand,
		}
		testutil.MustNoErr(t, db.CreateOrder(o), "create "+uuid)
		testdb.ReserveBin(t, db, o.ID, bin.ID)
		return o
	}
	for _, tc := range []struct {
		name, payload, intent string
		want                  string
		stmts                 int64
	}{
		{"carries_its_part", bp.Code, "", bp.Code, 0},
		{"empty_intent", "", SourceIntentEmpty, "", 0},
		{"blank_full_intent", "", "", bp.Code, 3},
	} {
		o := mk("pfd-"+tc.name, tc.payload, tc.intent)
		counter.Reset()
		got := d.payloadForDispatch(o)
		if n := counter.Count(); got != tc.want || n != tc.stmts {
			t.Errorf("%s: payloadForDispatch = %q in %d statements, want %q in %d",
				tc.name, got, n, tc.want, tc.stmts)
		}
	}
}
