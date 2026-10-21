//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// vacate_one_read_docker_test.go — "who waits on this bin" is one statement.
//
// A home clear's partial move held before dispatch IS a vacate hold (owner,
// 2026-10-18): a row of PopVacateHeld's one population read
// (orders.VacateWaitCandidates), not a second kind read beside it. A lift asks
// that one read narrowed to the lifted bin, and it returns no row when nothing
// waits on it.

// TestVacate_ALiftIsOneReadAndNoRowWhenNothingWaits: with a clear's partial
// move held on bin E, the lift of another bin costs the re-drive exactly one
// statement and returns no row; the read narrowed to E returns the move, and
// the unnarrowed read (an order's end, the floor) returns it too.
func TestVacate_ALiftIsOneReadAndNoRowWhenNothingWaits(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	setupTestData(t, db)
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n
	}
	home, src, else_ := mk("VOR-H"), mk("VOR-S"), mk("VOR-ELSE")
	e := makeEmptyBin(t, db, home.ID, "VOR-E")
	p := makeEmptyBin(t, db, src.ID, "VOR-P")
	other := makeEmptyBin(t, db, else_.ID, "VOR-OTHER")
	parent := &orders.Order{EdgeUUID: orders.ClearUUIDPrefix + "vor", StationID: "edge.vor", OrderType: OrderTypeMove,
		Status: StatusReshuffling, Quantity: 1, SourceNode: home.Name, DeliveryNode: home.Name}
	testutil.MustNoErr(t, db.CreateOrder(parent), "parent")
	stamped := time.Now().UTC()
	steps, err := json.Marshal([]resolvedStep{{Action: protocol.ActionPickup, Node: src.Name},
		{Action: protocol.ActionDropoff, Node: home.Name, Vacate: &plan.Vacate{Node: home.Name, Bin: e.ID, StampedAt: &stamped}}})
	testutil.MustNoErr(t, err, "steps")
	child := &orders.Order{EdgeUUID: "vor-p", StationID: "edge.vor", OrderType: OrderTypeMove, Status: StatusPending,
		Quantity: 1, ParentOrderID: &parent.ID, Sequence: 2, BinID: &p.ID, SourceNode: src.Name,
		DeliveryNode: home.Name, StepsJSON: string(steps), SourceIntent: SourceIntentForType(OrderTypeMove)}
	_, err = db.CreateCompoundChildren([]store.CompoundChild{{Order: child, BinID: p.ID}}, nil)
	testutil.MustNoErr(t, err, "the partial's move")

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, testdb.NewSuccessBackend())

	counter.Reset()
	if got := d.RedriveVacateWaits(other.ID); len(got) != 0 {
		t.Errorf("the lift of a bin nobody waits on released %v", got)
	}
	if n := counter.Count(); n != 1 {
		t.Errorf("the lift of a bin nobody waits on cost %d statements, want 1: %v", n, counter.Statements())
	}
	for _, c := range []struct {
		bin  int64
		want int
	}{{other.ID, 0}, {e.ID, 1}, {0, 1}} {
		rows, err := cdb.VacateWaitCandidates(c.bin)
		testutil.MustNoErr(t, err, "who waits")
		if len(rows) != c.want || (c.want == 1 && rows[0].ID != child.ID) {
			t.Errorf("who waits on bin %d: %d rows, want %d (the partial's move)", c.bin, len(rows), c.want)
		}
	}
}
