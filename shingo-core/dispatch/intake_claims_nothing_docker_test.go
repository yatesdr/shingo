//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
)

// INTAKE CLAIMS NOTHING, ON ANY PATH.
//
// The fulfillment scanner is the single claimer: it gates the dropoff, finds,
// reserves and confirms under scanMu, so two orders to one node cannot both pass
// the gate before either holds a bin. That guarantee is only as good as the
// claim that nothing ELSE takes. A plain order's intake (planTransport) used to
// take one on exactly one path — a buried source, where it planned the dig and
// the compound claimed the retrieve child's bin outside scanMu
// (engine TestTwoHoldersThroughIntakeBuriedArm). It now names the burial and
// queues.
//
// This dispatcher has no scanner wired (mockEmitter runs nothing on the queued
// event), so whatever is claimed or reserved after HandleOrderRequest returns
// was taken by intake itself.
func TestIntake_ClaimsNothingOnAnyPath(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storage, line, bp := setupTestData(t, db)
	testdb.CreateBinAtNode(t, db, bp.Code, storage.ID, "INC-FULL")
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "INCB", NumSlots: 2, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	// The buried move gets its own lane: sharing the retrieve's would meet that
	// order's lane lock instead of a burial.
	scMove := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "INCM", NumSlots: 2, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: "INC-DRY", UOPCapacity: 10}), "dry payload")
	moveSrc := &nodes.Node{Name: "INC-MOVE-SRC", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(moveSrc), "move source")
	testdb.CreateBinAtNode(t, db, bp.Code, moveSrc.ID, "INC-MOVE-BIN")

	d := NewDispatcher(db, testdb.NewTrackingBackend(), &mockEmitter{}, "core", "shingo.dispatch",
		&DefaultResolver{DB: db})

	cases := []struct {
		name string
		req  protocol.OrderRequest
	}{
		{"retrieve, source found", protocol.OrderRequest{OrderType: OrderTypeRetrieve,
			PayloadCode: bp.Code, DeliveryNode: line.Name}},
		{"retrieve, source dry", protocol.OrderRequest{OrderType: OrderTypeRetrieve,
			PayloadCode: "INC-DRY", DeliveryNode: line.Name}},
		{"retrieve, source buried", protocol.OrderRequest{OrderType: OrderTypeRetrieve,
			PayloadCode: sc.Payload.Code, SourceNode: sc.Grp.Name, DeliveryNode: sc.LineNode.Name}},
		{"move, named concrete source", protocol.OrderRequest{OrderType: OrderTypeMove,
			PayloadCode: bp.Code, SourceNode: moveSrc.Name, DeliveryNode: line.Name}},
		{"move, buried group source", protocol.OrderRequest{OrderType: OrderTypeMove,
			PayloadCode: scMove.Payload.Code, SourceNode: scMove.Grp.Name, DeliveryNode: scMove.LineNode.Name}},
		{"retrieve_empty", protocol.OrderRequest{OrderType: OrderTypeRetrieve, RetrieveEmpty: true,
			PayloadCode: bp.Code, DeliveryNode: line.Name}},
	}
	for i, c := range cases {
		req := c.req
		req.OrderUUID = "inc-" + string(rune('a'+i))
		req.Quantity = 1
		d.HandleOrderRequest(testEnvelope(), &req)
		o := testdb.RequireOrder(t, db, req.OrderUUID)

		var held, reserved, children int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM bins WHERE claimed_by = $1
			OR claimed_by IN (SELECT id FROM orders WHERE parent_order_id = $1)`, o.ID).Scan(&held), "held")
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM reservations WHERE order_id = $1
			OR order_id IN (SELECT id FROM orders WHERE parent_order_id = $1)`, o.ID).Scan(&reserved), "reserved")
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM orders WHERE parent_order_id = $1`,
			o.ID).Scan(&children), "children")
		if held != 0 || reserved != 0 || children != 0 {
			t.Errorf("%s: intake left %d claimed bins, %d reservations, %d compound children (status %q)",
				c.name, held, reserved, children, o.Status)
		}
	}
}
