//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/orders"
)

// queueDetailWrites counts every UPDATE of orders that sets one of the three
// wait columns, from the moment it is called. A trigger rather than a fake
// store: the dispatcher writes through the concrete store, and what is being
// counted is statements the database actually ran.
//
// Own database only (testDB, not testDBShared): it creates a table and a
// trigger.
func queueDetailWrites(t *testing.T, db *store.DB) func() int {
	t.Helper()
	mustExecDispatch(t, db, `CREATE TABLE test_wait_writes (order_id BIGINT NOT NULL)`)
	mustExecDispatch(t, db, `CREATE FUNCTION test_note_wait_write() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN INSERT INTO test_wait_writes VALUES (NEW.id); RETURN NEW; END $$`)
	mustExecDispatch(t, db, `CREATE TRIGGER test_note_wait_write
		AFTER UPDATE OF queue_reason, queue_code, queue_cause ON orders
		FOR EACH ROW EXECUTE FUNCTION test_note_wait_write()`)
	return func() int {
		var n int
		testutil.MustNoErr(t, db.QueryRow(`SELECT COUNT(*) FROM test_wait_writes`).Scan(&n), "count wait writes")
		return n
	}
}

// A BURIED DEMAND WAITING BEHIND ANOTHER DIG IS NOT REWRITTEN ON EVERY PASS.
//
// The scanner re-drives a parked complex demand on every pass, and each pass
// lands in handleComplexBurial again. It used to write "storage is being
// rearranged" before deciding the outcome, and then the outcome's own cause, so
// a demand whose lane another dig held flipped between two causes twice a pass:
// two UPDATEs, updated_at bumped each time, and two messages to the station
// whenever the outcome's sentence names the dig (here it does). Nothing about
// the wait had changed.
//
// Wanted: the first pass writes the wait once; every later pass with the same
// answer writes nothing and tells nobody.
func TestBuriedReplay_AnIdlePassWritesNothingAndTellsNobody(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sc := testdb.SetupCompound(t, db, testdb.CompoundConfig{
		Prefix: "IDLEPASS", NumSlots: 2, NumShuffles: 1, TargetSlot: 2, TargetAge: 2 * time.Hour,
	})
	d, emitter := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	// Another dig holds the lane for the whole test.
	holder := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "idlepass-holder"
		o.Status = StatusDispatched
		o.PayloadCode = sc.Payload.Code
	})
	if !d.laneLock.TryLock(sc.Lane.ID, holder.ID) {
		t.Fatal("fixture: could not take the lane lock for the other dig")
	}

	demand := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "idlepass-demand"
		o.StationID = "line-1"
		o.OrderType = OrderTypeComplex
		o.Status = StatusQueued
		o.PayloadCode = sc.Payload.Code
		o.DeliveryNode = sc.LineNode.Name
	})
	buried := &BuriedError{Bin: sc.TargetBin, Slot: sc.Slots[1], LaneID: sc.Lane.ID}
	writes := queueDetailWrites(t, db)

	var emits, wrote []int
	for pass := 1; pass <= 3; pass++ {
		// The scanner reads the order afresh on every pass.
		o, err := db.GetOrder(demand.ID)
		testutil.MustNoErr(t, err, "reload the demand")
		e0, w0 := len(emitter.waitChanged), writes()
		d.handleComplexBuriedOnReplay(o, buried)
		emits = append(emits, len(emitter.waitChanged)-e0)
		wrote = append(wrote, writes()-w0)
	}
	t.Logf("per pass: wait-changed emits %v, queue-detail writes %v", emits, wrote)

	after, err := db.GetOrder(demand.ID)
	testutil.MustNoErr(t, err, "read the demand back")
	if after.QueueCause != string(CauseLaneLocked) {
		t.Fatalf("queue_cause = %q, want %q — the fixture is not on the lane-busy arm", after.QueueCause, CauseLaneLocked)
	}
	if wrote[0] != 1 {
		t.Errorf("first pass wrote the wait %d time(s), want 1", wrote[0])
	}
	for i := 1; i < 3; i++ {
		if emits[i] != 0 || wrote[i] != 0 {
			t.Errorf("pass %d: %d wait-changed emit(s) and %d write(s), want 0 and 0 — the wait is "+
				"the same as the pass before", i+1, emits[i], wrote[i])
		}
	}
}
