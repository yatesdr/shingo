//go:build docker

package dispatch

import (
	"encoding/json"
	"strings"
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
	_, err = testdb.CreateCompoundChildren(t, db, []store.CompoundChild{{Order: child}}, nil)
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

// TestVacate_TheLiftAsksItsOwnTradeLegInTheSameRead: a home clear's trade leg
// with the fleet, holding its drop on H for E, and E already gone from H (its
// partner lifted it while nothing re-asked this leg). Its own lift of P names
// a bin its stamp does not: the plain lift's read does not return it, so
// nothing is appended; the lift's read that names the lifting order returns it
// in the same one statement, and its drop is appended. The lift of a bin
// nobody waits on, by an order that holds after no lift, is still one
// statement.
func TestVacate_TheLiftAsksItsOwnTradeLegInTheSameRead(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	setupTestData(t, db)
	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), "node "+name)
		return n
	}
	home, spot, else_ := mk("VTL-H"), mk("VTL-S"), mk("VTL-ELSE")
	e := makeEmptyBin(t, db, home.ID, "VTL-E")
	p := makeEmptyBin(t, db, spot.ID, "VTL-P")
	other := makeEmptyBin(t, db, else_.ID, "VTL-OTHER")
	parent := &orders.Order{EdgeUUID: orders.ClearUUIDPrefix + "vtl", StationID: "edge.vtl", OrderType: OrderTypeMove,
		Status: StatusReshuffling, Quantity: 1, SourceNode: home.Name, DeliveryNode: home.Name}
	testutil.MustNoErr(t, db.CreateOrder(parent), "parent")
	stamped := time.Now().UTC().Add(-time.Minute)
	steps, err := json.Marshal([]resolvedStep{{Action: protocol.ActionPickup, Node: spot.Name},
		{Action: protocol.ActionWait, WaitKind: WaitKindVacate},
		{Action: protocol.ActionDropoff, Node: home.Name, Vacate: &plan.Vacate{Node: home.Name, Bin: e.ID, StampedAt: &stamped}}})
	testutil.MustNoErr(t, err, "steps")
	leg := &orders.Order{EdgeUUID: "vtl-l1", StationID: "edge.vtl", OrderType: OrderTypeMove, Status: StatusDispatched,
		Quantity: 1, ParentOrderID: &parent.ID, Sequence: 2, BinID: &p.ID, SourceNode: spot.Name,
		DeliveryNode: home.Name, StepsJSON: string(steps)}
	testutil.MustNoErr(t, db.CreateOrder(leg), "the trade leg")
	testutil.MustNoErr(t, db.UpdateOrderVendor(leg.ID, "V-vtl-l1", "CREATED", ""), "with the fleet")
	vhLift(t, db, e.ID)
	vhLift(t, db, p.ID)

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "counting db")
	t.Cleanup(func() { cdb.Close() })
	backend := testdb.NewTrackingBackend()
	d, _ := newTestDispatcher(t, cdb, backend)

	counter.Reset()
	if got := d.RedriveVacateOnLift(other.ID, parent.ID); len(got) != 0 {
		t.Errorf("the lift of a bin nobody waits on released %v", got)
	}
	if n := counter.Count(); n != 1 {
		t.Errorf("the lift of a bin nobody waits on cost %d statements, want 1: %v", n, counter.Statements())
	}
	if got := d.RedriveVacateWaits(p.ID); len(got) != 0 || len(backend.ReleaseCalls()) != 0 {
		t.Fatalf("the plain lift's read released %v: the leg's stamp does not name the bin it lifts", got)
	}
	counter.Reset()
	got := d.RedriveVacateOnLift(p.ID, leg.ID)
	if len(got) != 1 || got[0] != leg.ID || len(backend.ReleaseCalls()) != 1 {
		t.Fatalf("the lift naming its own trade leg released %v (%d appends), want the leg's drop appended",
			got, len(backend.ReleaseCalls()))
	}
	var reads int
	for _, s := range counter.Statements() {
		if strings.Contains(s, "FROM orders") && strings.Contains(s, "wait_kind") {
			reads++
		}
	}
	if reads != 1 {
		t.Errorf("the lift asked who waits %d times, want once: %v", reads, counter.Statements())
	}
}
