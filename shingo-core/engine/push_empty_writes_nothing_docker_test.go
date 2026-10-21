//go:build docker

package engine

import (
	"encoding/json"
	"reflect"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// push_empty_writes_nothing_docker_test.go — PUSH EMPTY, Core's half.
//
// The Edge's PUSH EMPTY (Engine.PushEmptyOut) reads node-bins and sends one
// order: a move from the unloader window to its outbound that names no part
// (createUnloaderEmptyOut). Core has no push-empty handler; what it sees is that
// move. This drives it as the Edge sends it, through intake, the pickup block
// and the delivery, and reads the carrier's row whole before and after.
//
// What changes is the move's own: where the carrier is (node_id, and the
// arrival's status and staged_at), and what the lift writes
// (bins.LiftToTransitTx: the anomaly note cleared, the generation bumped, one
// ledger row "lifted", and last_lift_at — every lift's stamp, the vacate
// fact's floor). Its type, payload, manifest, count and findings are not
// written, and none of the three statements that empty a carrier runs on it
// (no clear_for_reuse, clear_and_claim or released_empty row). The changed set
// is pinned by name, so a write that starts to ride the move (a stamp, a count, a
// type) moves the set, and the carried columns are named once more below.

func pushEmptyBinRow(t *testing.T, db *store.DB, id int64) map[string]any {
	t.Helper()
	var raw []byte
	testutil.MustNoErr(t, db.QueryRow(`SELECT to_jsonb(b) FROM bins b WHERE id=$1`, id).Scan(&raw), "read bin row")
	var row map[string]any
	testutil.MustNoErr(t, json.Unmarshal(raw, &row), "decode bin row")
	return row
}

func TestPushEmpty_CoreWritesNothingTheCarrierCarries(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())
	window := rebindNode(t, "PE-WINDOW", db.CreateNode)
	outbound := rebindNode(t, "PE-EMPTIES", db.CreateNode)
	carrier := testdb.CreateBinAtNode(t, db, "", window.ID, "PE-CARRIER")
	// An empty as a CLEAR leaves one (ClearForReuseTx's columns), with the
	// columns a move has no business writing moved off their defaults.
	_, err := db.Exec(`UPDATE bins SET manifest=NULL, uop_remaining=0, manifest_confirmed=false,
		loaded_at=NULL, undeclared_carrier_at=NULL, description='off-default', delta_epoch=7,
		last_counted_at='2020-01-03T00:00:00Z', last_counted_by='counter',
		anomaly_at='2020-01-04T00:00:00Z', anomaly_note='note', updated_at='2020-01-08T00:00:00Z'
		WHERE id=$1`, carrier.ID)
	testutil.MustNoErr(t, err, "make the carrier an empty, off its defaults")
	before := pushEmptyBinRow(t, db, carrier.ID)
	var ledgerBefore int64
	testutil.MustNoErr(t, db.QueryRow(`SELECT COALESCE(MAX(id), 0) FROM bin_uop_ledger WHERE bin_id=$1`,
		carrier.ID).Scan(&ledgerBefore), "ledger high-water mark")

	// The U2 exactly as createUnloaderEmptyOut sends it: a move from the window
	// to the outbound, naming no part and no bin.
	eng.Dispatcher().HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{
		OrderUUID: "pe-move", OrderType: dispatch.OrderTypeMove,
		SourceNode: window.Name, DeliveryNode: outbound.Name, Quantity: 1,
	})
	u2 := testdb.RequireOrder(t, db, "pe-move")
	if u2.BinID == nil || *u2.BinID != carrier.ID {
		t.Fatalf("fixture: the push claimed bin %v, want the carrier %d (status %q)", u2.BinID, carrier.ID, u2.Status)
	}
	eng.handlePickupBlockCompleted(BlockCompletedEvent{
		OrderID: u2.ID, BlockID: "pe-b1", Location: window.Name, BinTask: "JackLoad",
	})
	testutil.MustNoErr(t, db.UpdateOrderStatus(u2.ID, string(protocol.StatusDelivered), "test: delivered"), "delivered")
	delivered, err := db.GetOrder(u2.ID)
	testutil.MustNoErr(t, err, "reload the push")
	eng.handleOrderDelivered(delivered)

	after := pushEmptyBinRow(t, db, carrier.ID)
	if got, want := after["node_id"], float64(outbound.ID); got != want {
		t.Fatalf("the carrier is at node %v after delivery, want the outbound %d", got, outbound.ID)
	}
	var changed []string
	for k, v := range after {
		if !reflect.DeepEqual(before[k], v) {
			changed = append(changed, k)
		}
	}
	sort.Strings(changed)
	want := []string{"anomaly_note", "delta_epoch", "last_lift_at", "node_id", "staged_at", "status", "updated_at"}
	if !reflect.DeepEqual(changed, want) {
		t.Errorf("the push changed %v on the carrier, want only %v. Before %v, after %v",
			changed, want, before, after)
	}
	// The ledger rows the push wrote, by op: which statements ran on the row.
	rows, err := db.Query(`SELECT op FROM bin_uop_ledger WHERE bin_id=$1 AND id>$2 ORDER BY id`,
		carrier.ID, ledgerBefore)
	testutil.MustNoErr(t, err, "read the push's ledger rows")
	defer rows.Close()
	var ops []string
	for rows.Next() {
		var op string
		testutil.MustNoErr(t, rows.Scan(&op), "scan op")
		ops = append(ops, op)
	}
	if !reflect.DeepEqual(ops, []string{"lifted"}) {
		t.Errorf("ledger ops written by the push = %v, want only [lifted]", ops)
	}
	for _, col := range []string{"bin_type_id", "payload_code", "manifest", "uop_remaining",
		"manifest_confirmed", "loaded_at", "undeclared_carrier_at", "label", "description"} {
		if !reflect.DeepEqual(before[col], after[col]) {
			t.Errorf("the push wrote %s on the carrier: %v -> %v", col, before[col], after[col])
		}
	}
}
