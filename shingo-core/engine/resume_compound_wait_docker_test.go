//go:build docker

package engine

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// waitWrites counts the writes to one order's wait columns from here on.
func waitWrites(t *testing.T, db *store.DB, orderID int64) func() int {
	t.Helper()
	for _, q := range []string{
		`CREATE TABLE test_wait_writes (order_id BIGINT NOT NULL)`,
		`CREATE FUNCTION test_note_wait_write() RETURNS trigger LANGUAGE plpgsql AS $$
			BEGIN INSERT INTO test_wait_writes VALUES (NEW.id); RETURN NEW; END $$`,
		`CREATE TRIGGER test_note_wait_write
			AFTER UPDATE OF queue_reason, queue_code, queue_cause ON orders
			FOR EACH ROW EXECUTE FUNCTION test_note_wait_write()`,
	} {
		_, err := db.DB.Exec(q)
		testutil.MustNoErr(t, err, "install the wait-write counter")
	}
	return func() int {
		var n int
		testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM test_wait_writes WHERE order_id = $1`,
			orderID).Scan(&n), "count wait writes")
		return n
	}
}

// A RESUMED PARENT KEEPS THE WAIT ITS SCAN WRITES. The dig that parked it is
// over, so its old wait is cleared; the requeue runs the scanner on this
// goroutine, and when the scanner parks the parent again under a new wait, that
// wait is the one on the row afterwards. Clearing after the requeue erased it,
// leaving a parent that is still waiting with nothing on its row to say why.
func TestResumeCompound_KeepsTheWaitTheScanWrites(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sd := testdb.SetupStandardData(t, db)
	grp, _, _ := closedGroup(t, db, "RESUME", sd.Payload.Code, 1)
	testdb.CreateBinAtNode(t, db, sd.Payload.Code, sd.LineNode.ID, "RESUME-STORED")
	eng := newTestEngine(t, db, simulator.New())

	// A first request gives the plan its stored shape; the parent is that row in
	// `reshuffling`, still carrying the wait its dig parked it under.
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), closedGroupStore("resume-shape", sd, grp))
	shape := testdb.RequireOrder(t, db, "resume-shape")
	parent := &orders.Order{EdgeUUID: "resume-parent", StationID: shape.StationID, OrderType: shape.OrderType,
		Status: protocol.StatusReshuffling, Quantity: 1, PayloadCode: shape.PayloadCode,
		SourceNode: shape.SourceNode, DeliveryNode: shape.DeliveryNode, StepsJSON: shape.StepsJSON,
		Coordinated: true}
	testutil.MustNoErr(t, db.CreateOrder(parent), "the parent in reshuffling")
	eng.Dispatcher().SetQueueReason(parent, protocol.QueueWaitingForSlot, dispatch.CauseIntakeBuried,
		dispatch.QueueParams{Payload: sd.Payload.Code})
	writes := waitWrites(t, db, parent.ID)
	before := len(queueUpdates(t, db, parent.EdgeUUID))

	testutil.MustNoErr(t, eng.Dispatcher().Lifecycle().ResumeCompound(parent), "resume the parent")

	after := testdb.RequireOrder(t, db, "resume-parent")
	t.Logf("after resume: status %s, code %q, cause %q, wait writes %d, order.update messages %d",
		after.Status, after.QueueCode, after.QueueCause, writes(), len(queueUpdates(t, db, parent.EdgeUUID))-before)
	if !protocol.IsAcquiring(after.Status) {
		t.Fatalf("the resumed parent is %s — the fixture wants the scan to park it again", after.Status)
	}
	if after.QueueCause != string(dispatch.CauseNGRPResolve) || after.QueueReason == "" {
		t.Fatalf("the resumed parent is %s with cause %q and sentence %q, want the scan's %q — it is "+
			"waiting, and the row no longer says why", after.Status, after.QueueCause, after.QueueReason,
			dispatch.CauseNGRPResolve)
	}
}
