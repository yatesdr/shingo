//go:build docker

package engine

import (
	"database/sql"
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// After a Core restart the poller re-tracks an open order with no memory of
// its blocks, so every block already FINISHED is reported again. A replayed
// pickup that the junction and claimed-bin arms cannot name falls to
// resolvePickupBin's single-bin fallback, which used to answer order.BinID
// wherever that bin was: a bin the order had already delivered to the line
// was named as lifted at storage and moved back to _TRANSIT. A lift releases
// a vacate hold (bins.LeftSince reads last_lift_at), so the fallback now
// answers only when the bin's record stands on the block's node.

// r10Fixture is a bin the order already delivered to the line and its order,
// still live: the replayed pickup names storage.
func r10Fixture(t *testing.T, prefix string) (db *store.DB, storage, line *nodes.Node, bin *bins.Bin, o *orders.Order) {
	t.Helper()
	db = testDB(t)
	storage, line, bp := setupTestData(t, db)
	bin = createTestBinAtNode(t, db, bp.Code, line.ID, prefix+"-BIN")
	o = testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID, o.StationID, o.Status = strings.ToLower(prefix), "line-1", "in_transit"
	})
	testutil.MustNoErr(t, db.UpdateOrderBinID(o.ID, bin.ID), "order bin")
	return db, storage, line, bin, o
}

// TestReplayedPickup_SingleBinFallbackAsksWhereTheBinIs: the resolver does not
// name a bin whose record is on another node.
func TestReplayedPickup_SingleBinFallbackAsksWhereTheBinIs(t *testing.T) {
	t.Parallel()
	db, storage, line, bin, o := r10Fixture(t, "SYN-R10")
	eng := newUnstartedEngine(t, db, simulator.New())
	binID, _, from, ok := eng.resolvePickupBin(o.ID, storage.Name) // the replayed pickup at storage
	if got := fmt.Sprintf("ok=%v bin=%v from_line=%v", ok, binID == bin.ID, from == line.ID); got != "ok=false bin=false from_line=false" {
		t.Fatalf("a pickup at %s resolved the order's bin standing on %s: %s, want nothing resolved",
			storage.Name, line.Name, got)
	}
	// Control: the same fallback at the bin's own node still answers it.
	binID, _, from, ok = eng.resolvePickupBin(o.ID, line.Name)
	if !ok || binID != bin.ID || from != line.ID {
		t.Fatalf("a pickup at the bin's own node %s: ok=%v bin=%d from=%d, want bin %d from %d",
			line.Name, ok, binID, from, bin.ID, line.ID)
	}
}

// TestPickupBlock_FallbackDoesNotLiftABinRecordedElsewhere: the pickup block
// completes at storage for an order whose bin is recorded on the line. The
// bin does not move, last_lift_at stays unwritten, EventBinEnteredTransit is
// not emitted, and the refusal is logged naming the bin and both nodes.
func TestPickupBlock_FallbackDoesNotLiftABinRecordedElsewhere(t *testing.T) {
	t.Parallel()
	db, storage, line, bin, o := r10Fixture(t, "SYN-FL")
	eng := newUnstartedEngine(t, db, simulator.New())
	var mu sync.Mutex
	var logs []string
	eng.logFn = func(format string, args ...any) {
		mu.Lock()
		defer mu.Unlock()
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	emitted := 0
	eng.Events.SubscribeTypes(func(Event) { emitted++ }, EventBinEnteredTransit)

	eng.handlePickupBlockCompleted(BlockCompletedEvent{OrderID: o.ID, BlockID: "b7", Location: storage.Name, BinTask: "JackLoad"})

	after, err := db.GetBin(bin.ID)
	testutil.MustNoErr(t, err, "bin after")
	var lastLift sql.NullTime
	testutil.MustNoErr(t, db.DB.QueryRow(`SELECT last_lift_at FROM bins WHERE id=$1`, bin.ID).Scan(&lastLift), "last_lift_at")
	if after.NodeID == nil || *after.NodeID != line.ID || lastLift.Valid || emitted != 0 {
		t.Errorf("a pickup at %s for a bin recorded on %s: node=%v last_lift_at=%v events=%d, want it left on the line, "+
			"no lift written and no transit event", storage.Name, line.Name, after.NodeID, lastLift, emitted)
	}
	mu.Lock()
	defer mu.Unlock()
	named := false
	for _, l := range logs {
		named = named || (strings.Contains(l, bin.Label) && strings.Contains(l, storage.Name) && strings.Contains(l, line.Name))
	}
	if !named {
		t.Errorf("no log line names %s, %s and %s: %q", bin.Label, storage.Name, line.Name, logs)
	}
}
