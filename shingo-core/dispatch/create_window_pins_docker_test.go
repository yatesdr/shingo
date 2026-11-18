//go:build docker

package dispatch

// THE CREATE WINDOW, PINNED AS IT STANDS. The stretch between an order's hand
// committing and the fleet answering its create: every reader of "Core has
// claimed it" and of "the fleet has it" is asked inside it, through
// testdb.CreateWindowFleet, which runs the test inside every create (refused or
// accepted) and takes its answer from it.
//
// Each test is one of two kinds. A CLAIMED reader (Core holds the order for its
// create) must keep its answer when the claim stops being a status; those pins
// stay green through the change. A FLEET reader (the fleet has the job) answers
// yes today only because the hand writes `dispatched` before asking; those pins
// flip when `dispatched` is written at the fleet's yes
// (the dispatch-claim pin list).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reconciliation"
)

// errCWRefused is the fleet's no in these pins.
var errCWRefused = errors.New("create window pin: the fleet refused the create")

// cwCell is one plain retrieve holding its bin softly, a fleet the test stands
// inside, and the dispatcher that hands the order to it.
type cwCell struct {
	t         *testing.T
	db        *store.DB
	d         *Dispatcher
	fleet     *testdb.CreateWindowFleet
	src, line *nodes.Node
	bin       *bins.Bin
	order     *orders.Order
	// preHand is the row as the current pass read it, before its hand: the
	// copy a second dispatcher holds when it reaches the order mid-create.
	preHand orders.Order
}

func newCWCell(t *testing.T, uuid string) *cwCell {
	t.Helper()
	db := testDB(t)
	tellToStations(db)
	f := testdb.NewCreateWindowFleet()
	d, _ := newTestDispatcher(t, db, f)
	src, line, bp := testdb.SetupTestData(t, db)
	bin := testdb.CreateBinAtNode(t, db, bp.Code, src.ID, strings.ToUpper(uuid)+"-BIN")
	order := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = uuid
		o.StationID = "edge.line1"
		o.OrderType = OrderTypeRetrieve
		o.PayloadCode = bp.Code
		o.SourceNode = src.Name
		o.DeliveryNode = line.Name
		o.Status = StatusPending
	})
	testutil.MustNoErr(t, d.binManifest.ReserveForDispatch(bin.ID, order.ID), "soft-hold the bin")
	testdb.HoldCart(t, db, order.ID, bin.ID)
	return &cwCell{t: t, db: db, d: d, fleet: f, src: src, line: line, bin: bin, order: order}
}

func (c *cwCell) row() *orders.Order {
	c.t.Helper()
	o, err := c.db.GetOrder(c.order.ID)
	testutil.MustNoErr(c.t, err, "read the order")
	return o
}

func (c *cwCell) count(q string, args ...any) int {
	c.t.Helper()
	var n int
	testutil.MustNoErr(c.t, c.db.DB.QueryRow(q, args...).Scan(&n), q)
	return n
}

// pass is one scanner-shaped attempt on a fresh read: the hand and the fleet
// (DispatchHand), then the one refusal door on a fleet refusal, as the
// scanner's fresh-bin arm disposes of it.
func (c *cwCell) pass() (string, error) {
	c.t.Helper()
	fresh := c.row()
	c.preHand = *fresh
	vendor, err := c.d.DispatchHand(fresh, c.bin.ID, c.src, c.line)
	if err != nil && !IsConcurrentTransition(err) {
		if _, refused := AsHandRefusal(err); refused {
			c.t.Fatalf("the hand refused (%v); these pins need a hand that commits", err)
		}
		code, cause, params := FleetRefusalCause(err, fresh.DeliveryNode)
		c.d.DemoteAfterFleetRefusal(fresh, code, cause, params)
	}
	return vendor, err
}

// refuseThenTake answers creates 1..refusals with the fleet's no and the next
// with its yes, running inside(n) in each create before it answers.
func (c *cwCell) refuseThenTake(refusals int, inside func(n int)) {
	c.t.Helper()
	c.fleet.Inside = func(n int, _ fleet.CreateOrderRequest) error {
		inside(n)
		if n <= refusals {
			return errCWRefused
		}
		return nil
	}
	for pass := 1; pass <= refusals+1; pass++ {
		vendor, err := c.pass()
		switch {
		case pass <= refusals && err == nil:
			c.t.Fatalf("pass %d: the fleet took a create it was told to refuse", pass)
		case pass > refusals && err != nil:
			c.t.Fatalf("pass %d: the fleet's yes came back as %v", pass, err)
		case pass > refusals && vendor == "":
			c.t.Fatalf("pass %d: dispatched with no vendor id", pass)
		}
	}
}

// TestCreateWindow_ClaimedReadersSayClaimed: inside every create, refused or
// accepted, each reader that means "Core holds this order for its create" says
// so. Today the row reads `dispatched` with no vendor id; once the claim is a
// column and the row stays at its waiting status, every one of these must say
// the same thing (during an ask, every reader answers what it answers today).
// A claimed pin: it stays green.
func TestCreateWindow_ClaimedReadersSayClaimed(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-claimed")
	inside := 0
	c.refuseThenTake(3, func(n int) {
		inside++
		row := c.row()
		where := fmt.Sprintf("inside create %d", n)
		if got := c.count(`SELECT COUNT(*) FROM bins WHERE id=$1 AND claimed_by=$2`, c.bin.ID, c.order.ID); got != 1 {
			t.Errorf("%s: the bin is not claimed by the order (the hand's hard claim)", where)
		}
		for _, h := range testdb.HoldsAtRest(t, c.db) {
			if strings.HasPrefix(h, fmt.Sprintf("order %d is ", c.order.ID)) {
				t.Errorf("%s: HoldsAtRest lists the order as at rest: %s", where, h)
			}
		}
		dug, err := orders.ListDigLocksAtRest(c.db.DB)
		testutil.MustNoErr(t, err, "dig locks at rest")
		for _, h := range dug {
			if h.OrderID == c.order.ID {
				t.Errorf("%s: DigLocksAtRest lists the order", where)
			}
		}
		if got := c.count(fmt.Sprintf(`SELECT COUNT(*) FROM orders WHERE id=$1 AND %s`, orders.AwaitingFleetSQL("")), c.order.ID); got != 0 {
			t.Errorf("%s: AwaitingFleetSQL reads the order as not yet handed over", where)
		}
		acq, err := c.db.ListAcquiringOrders()
		testutil.MustNoErr(t, err, "acquiring")
		for _, o := range acq {
			if o.ID == c.order.ID {
				t.Errorf("%s: the order is in the scanner's population", where)
			}
		}
		refs, err := c.db.ListActiveOrdersBySourceRef([]string{c.src.Name})
		testutil.MustNoErr(t, err, "source refs")
		for _, o := range refs {
			if o.ID == c.order.ID {
				t.Errorf("%s: the re-parent guard reads the order as not yet sent", where)
			}
		}
		inflight, err := orders.CountInFlightByDeliveryNode(c.db.DB, c.line.Name)
		testutil.MustNoErr(t, err, "in flight")
		if inflight != 1 {
			t.Errorf("%s: CountInFlightByDeliveryNode = %d, want 1 (the order holds its bin for the line)", where, inflight)
		}
		if !swapLegCommittedToFleet(row) {
			t.Errorf("%s: swapLegCommittedToFleet = false (gate 3 would read an undecided holder)", where)
		}
		if !pairLegCommitted(row.Status) {
			t.Errorf("%s: pairLegCommitted = false (a pair partner would wait on a committed leg)", where)
		}
		swept, err := reconciliation.ReleaseAcquiringOrphanClaims(c.db.DB)
		testutil.MustNoErr(t, err, "orphan sweep")
		if swept != 0 {
			t.Errorf("%s: the acquiring-orphan sweep took %d claim(s) off the order", where, swept)
		}
	})
	if inside != 4 {
		t.Fatalf("stood inside %d creates, want 4", inside)
	}
}

// TestCreateWindow_StaleMovesAndARivalHandLose: inside a create, every
// non-terminal move from the copy the pass read before its hand loses, and so
// does a second dispatcher's hand from that copy (the lock). Today the status
// CAS refuses them; once the claim is a column, the column's clause on the one
// status statement and the claim's own compare-and-swap must refuse them the
// same way. A claimed pin: it stays green.
func TestCreateWindow_StaleMovesAndARivalHandLose(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-lock")
	rival, _ := newTestDispatcher(t, c.db, c.fleet)
	asked := 0
	c.refuseThenTake(2, func(n int) {
		where := fmt.Sprintf("inside create %d (a copy read at %s)", n, c.preHand.Status)
		moves := []struct {
			name string
			move func(o *orders.Order) error
		}{
			{"BeginReshuffle", func(o *orders.Order) error { return c.d.lifecycle.BeginReshuffle(o, "create window pin") }},
			{"Acknowledge", func(o *orders.Order) error { return c.d.lifecycle.Acknowledge(o, "create window pin") }},
			{"Park", func(o *orders.Order) error {
				o.QueueCode = string(protocol.QueueWaitingForMaterial)
				return c.d.lifecycle.Park(o, "create window pin")
			}},
		}
		for _, m := range moves {
			if m.name == "Park" && c.preHand.Status == StatusQueued {
				continue // Park of a queued copy writes nothing by design
			}
			if !protocol.IsValidTransition(c.preHand.Status, map[string]protocol.Status{
				"BeginReshuffle": StatusReshuffling, "Acknowledge": StatusAcknowledged, "Park": StatusQueued,
			}[m.name]) {
				continue // illegal from this copy whatever the row says (pending → acknowledged)
			}
			cp := c.preHand
			if err := m.move(&cp); !IsConcurrentTransition(err) {
				t.Errorf("%s: %s = %v, want ConcurrentTransition", where, m.name, err)
			}
		}
		if n > 2 {
			return // the rival is asked inside the refused creates only
		}
		asked++
		cp := c.preHand
		if _, err := rival.DispatchHand(&cp, c.bin.ID, c.src, c.line); !IsConcurrentTransition(err) {
			t.Errorf("%s: a rival hand = %v, want ConcurrentTransition", where, err)
		}
	})
	if asked != 2 {
		t.Fatalf("the rival was asked inside %d creates, want 2", asked)
	}
	if got := len(c.fleet.Asked()); got != 3 {
		t.Errorf("the fleet was asked %d times, want 3 (a rival that lost asked nothing)", got)
	}
}

// TestCreateWindow_ARedirectIsRefusedAndChangesNothing: a redirect that lands
// inside a create is refused (invalid_state), and the order's status,
// destination and vendor id are as they were. Today the gate refuses it because
// the row reads `dispatched`; once the row stays at its waiting status, the
// re-keyed gate must refuse it on the claim, or PrepareRedirect rewrites the
// destination under a create that may already be driving to the old one
// (cinder Break 2). A claimed pin: it stays green.
func TestCreateWindow_ARedirectIsRefusedAndChangesNothing(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-redirect")
	redirected := false
	c.refuseThenTake(1, func(n int) {
		if n != 1 {
			return
		}
		redirected = true
		before := c.row()
		env := &protocol.Envelope{Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.line1"}}
		c.d.HandleOrderRedirect(env, &protocol.OrderRedirect{OrderUUID: "cw-redirect", NewDeliveryNode: c.src.Name})
		after := c.row()
		if after.Status != before.Status || after.DeliveryNode != c.line.Name || after.VendorOrderID != before.VendorOrderID {
			t.Errorf("a redirect inside the create moved the order: %s→%s, delivery %s, vendor %q",
				before.Status, after.Status, after.DeliveryNode, after.VendorOrderID)
		}
		if code := cwLastErrorCode(t, c.db, "cw-redirect"); code != "invalid_state" {
			t.Errorf("the redirect's reply code = %q, want invalid_state", code)
		}
	})
	if !redirected {
		t.Fatal("the redirect was never sent inside a create")
	}
}

// TestCreateWindow_FleetReadersSayTheFleetHasIt: inside a create the fleet then
// refuses, every reader that means "the fleet has this job" says yes, because
// the hand wrote `dispatched` before asking. It is a lie the station acts on: a
// changeover START is refused as if a robot carried the order. A FLEET pin: it
// flips when `dispatched` is written at the fleet's yes.
func TestCreateWindow_FleetReadersSayTheFleetHasIt(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-fleet")
	c.refuseThenTake(2, func(n int) {
		if n > 2 {
			return // the accepted create is not this pin's question
		}
		row := c.row()
		where := fmt.Sprintf("inside refused create %d", n)
		if row.Status != StatusDispatched || row.VendorOrderID != "" {
			t.Errorf("%s: the row reads %s vendor %q, want dispatched with no vendor id", where, row.Status, row.VendorOrderID)
		}
		if !protocol.IsVendorActive(row.Status) || !protocol.IsVendorTracked(row.Status) {
			t.Errorf("%s: IsVendorActive=%v IsVendorTracked=%v, want both true as built", where,
				protocol.IsVendorActive(row.Status), protocol.IsVendorTracked(row.Status))
		}
		if got := protocol.ChangeoverStartActionFor(row.Status); got != protocol.ChangeoverStartBlock {
			t.Errorf("%s: changeover START reads %s, want block as built", where, got)
		}
		_, ok, err := c.db.OrderCommittedToFleetAt(c.order.ID)
		testutil.MustNoErr(t, err, "committed-to-fleet time")
		if !ok {
			t.Errorf("%s: OrderCommittedToFleetAt has no row, want the hand's `dispatched` row as built", where)
		}
	})
}

// TestRefusedPasses_StationIsToldEachPassTwice: three refusals and then the
// fleet's yes. Every pass tells the station `dispatched` (the hand, before the
// fleet is asked) and then `queued` (the park after the refusal); inside each
// create the projection and the snapshot (both copy the row) read `dispatched`
// with no wait, because the hand's write ended it. A FLEET pin: it flips to
// `queued` once and then `dispatched`, with each create read at its waiting
// status.
func TestRefusedPasses_StationIsToldEachPassTwice(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-tells")
	var inside []string
	c.refuseThenTake(3, func(n int) {
		row := c.row()
		p := ProjectionFor(row)
		inside = append(inside, fmt.Sprintf("%s/%s v%d", p.Status, p.QueueCode, p.Version))
		if string(row.Status) != p.Status || row.QueueCode != p.QueueCode || row.Version != p.Version {
			t.Errorf("inside create %d: the projection (%s/%s v%d) is not the row (%s/%s v%d)", n,
				p.Status, p.QueueCode, p.Version, row.Status, row.QueueCode, row.Version)
		}
	})
	if want := "dispatched/ v2, dispatched/ v4, dispatched/ v6, dispatched/ v8"; strings.Join(inside, ", ") != want {
		t.Errorf("the projection inside each create = %v, want %s", inside, want)
	}
	want := "dispatched v2, queued v3, dispatched v4, queued v5, dispatched v6, queued v7, dispatched v8"
	if got := strings.Join(stationTellsOf(t, c.db, "cw-tells"), ", "); got != want {
		t.Errorf("the station's tells = %s, want %s", got, want)
	}
}

// TestRefusedPasses_HistoryNamesEveryAskedJob: each pass asks under a new id,
// and each writes a `dispatched` history row "vendor order X created" for the
// id it asked, before the fleet answers; the fleet created only the last. Three
// of the four rows name a job that never existed, and the time-to-dispatch and
// soakstat readers count them. A FLEET pin: it flips to one row, the created
// job's.
func TestRefusedPasses_HistoryNamesEveryAskedJob(t *testing.T) {
	t.Parallel()
	c := newCWCell(t, "cw-history")
	c.refuseThenTake(3, func(int) {})
	asked := c.fleet.Asked()
	created := c.fleet.CreateRequests()
	if len(asked) != 4 || len(created) != 1 || created[0].OrderID != asked[3] {
		t.Fatalf("the fleet was asked %v and created %d; want 4 asks, the last one created", asked, len(created))
	}
	hist, err := c.db.ListOrderHistory(c.order.ID)
	testutil.MustNoErr(t, err, "history")
	var named []string
	for _, h := range hist {
		if h.Status == StatusDispatched {
			named = append(named, h.Detail)
		}
	}
	var want []string
	for _, id := range asked {
		want = append(want, fmt.Sprintf("vendor order %s created", id))
	}
	if strings.Join(named, " | ") != strings.Join(want, " | ") {
		t.Errorf("`dispatched` history rows = %q, want one per asked id %q", named, want)
	}
	if row := c.row(); row.VendorOrderID != asked[3] {
		t.Errorf("Core names %q, want the created job %q", row.VendorOrderID, asked[3])
	}
}

// cwLastErrorCode is the error code of the newest order.error queued for uuid.
func cwLastErrorCode(t *testing.T, db *store.DB, uuid string) string {
	t.Helper()
	msgs, err := db.ListPendingOutbox(500)
	testutil.MustNoErr(t, err, "list outbox")
	code := ""
	for _, m := range msgs {
		if m.MsgType != protocol.TypeOrderError {
			continue
		}
		var env protocol.Envelope
		var e protocol.OrderError
		if json.Unmarshal(m.Payload, &env) != nil || json.Unmarshal(env.Payload, &e) != nil || e.OrderUUID != uuid {
			continue
		}
		code = e.ErrorCode
	}
	return code
}
