//go:build docker

package dispatch

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// order_press_docker_test.go — who a plain order is for: the press a station
// names on its request (protocol.OrderRequest.ProcessNode), what Core stores
// and what a strict kanban reads of it.

// opCell is a press whose part rides one carrier type, drawing empties from a
// strict kanban that supports it, with one empty carrier in the kanban's slot.
type opCell struct {
	press, part, kanban string
	empty               int64
}

func newOPCell(t *testing.T, db *store.DB, prefix string) *opCell {
	t.Helper()
	c := &opCell{press: prefix + "-PRESS", part: prefix + "-PART", kanban: prefix + "-KAN"}
	pl := &payloads.Payload{Code: c.part, UOPCapacity: 40}
	testutil.MustNoErr(t, db.CreatePayload(pl), "the part")
	bt := &bins.BinType{Code: prefix + "-BT"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "the carrier type")
	testutil.MustNoErr(t, db.SetPayloadBinTypes(pl.ID, []int64{bt.ID}), "the part rides the carrier type")
	press := &nodes.Node{Name: c.press, Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, press), "press")
	kan, err := nodes.CreateGroup(db.DB, c.kanban)
	testutil.MustNoErr(t, err, "the kanban")
	testdb.MarkGroup(t, db, kan)
	testdb.KanbanStrict(t, db, kan, true)
	testutil.MustNoErr(t, nodes.SetKanbanSupports(db.DB, kan, []int64{press.ID}), "the kanban supports the press")
	slot := &nodes.Node{Name: c.kanban + "-1", Enabled: true, ParentID: &kan}
	testutil.MustNoErr(t, nodes.Create(db.DB, slot), "the kanban's slot")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status)
		VALUES ($1, $2, $3, 'available') RETURNING id`, bt.ID, prefix+"-EMPTY", slot.ID).Scan(&c.empty), "the empty")
	return c
}

// emptyFor is the station's plain empty retrieve for the press, naming press
// as the press it serves ("" names none, as an Edge older than the field).
func (c *opCell) emptyFor(d *Dispatcher, uuid, press string) {
	d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid,
		OrderType: OrderTypeRetrieve, RetrieveEmpty: true, PayloadCode: c.part,
		SourceNode: c.kanban, DeliveryNode: c.press, ProcessNode: press, Quantity: 1})
}

// TestOrderPress_AStrictKanbanServesThePlainEmptyThatNamesItsPress: a plain
// empty retrieve that names the press a strict kanban supports is served from
// it, and the row stores the press. The same retrieve naming no press, which
// is what an Edge older than the field sends, is an outsider at the kanban and
// waits fenced, as it did before the field.
func TestOrderPress_AStrictKanbanServesThePlainEmptyThatNamesItsPress(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newOPCell(t, db, "OPS")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())

	c.emptyFor(d, "ops-blank", "")
	blank := testdb.RequireOrder(t, db, "ops-blank")
	if blank.Status != protocol.StatusQueued || blank.QueueCause != string(CauseFinderGroupFenced) || blank.ProcessNode != "" {
		t.Fatalf("naming no press: %s under %q (process node %q), want queued fenced with none stored",
			blank.Status, blank.QueueCause, blank.ProcessNode)
	}

	c.emptyFor(d, "ops-press", c.press)
	named := testdb.RequireOrder(t, db, "ops-press")
	if named.ProcessNode != c.press {
		t.Errorf("the row stores process node %q, want %s", named.ProcessNode, c.press)
	}
	if named.QueueCause == string(CauseFinderGroupFenced) || named.Plan == nil || named.Plan.BinID != c.empty {
		t.Fatalf("naming the press: %s under %q with pick %+v, want the kanban's empty %d picked",
			named.Status, named.QueueCause, named.Plan, c.empty)
	}
}

// TestOrderPress_ThePressAPlainOrderNamesIsWhatCoreKeysItOn: the readers that
// key on an order's process node read a plain order that names its press as
// that press's, where the same order naming none read as nobody's. Both
// orders are a full retrieve to the press's outside position that finds
// nothing and is cancelled out of queued.
//
//   - futility: the tuple (station, press, part) counts it; with no press the
//     tuple is too vague to count (FutilityKey.incomplete).
//   - the history row's reference names the press; with none, the delivery.
//   - the lane-entry origin is the press's (process, style) set; with none,
//     unclassified.
func TestOrderPress_ThePressAPlainOrderNamesIsWhatCoreKeysItOn(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	c := newOPCell(t, db, "OPR")
	outside := &nodes.Node{Name: "OPR-OUT", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, outside), "the press's outside")
	pressNode, err := db.GetNodeByDotName(c.press)
	testutil.MustNoErr(t, err, "the press")
	pressOrigin(t, db, pressNode, "OPR-P", "OPR-S")
	d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
	clk := clock.NewManual(time.Date(2026, 11, 11, 9, 0, 0, 0, time.UTC))
	aud := armDetector(t, d, 2, clk)

	cancelled := func(uuid, press string) *orders.Order {
		t.Helper()
		d.HandleOrderRequest(testEnvelope(), &protocol.OrderRequest{OrderUUID: uuid, OrderType: OrderTypeRetrieve,
			PayloadCode: c.part, DeliveryNode: outside.Name, ProcessNode: press, Quantity: 1})
		o := testdb.RequireOrderStatus(t, db, uuid, protocol.StatusQueued)
		d.Lifecycle().CancelOrder(o, o.StationID, "no_source_bin", CancelCause{})
		clk.Advance(time.Minute)
		return testdb.RequireOrder(t, db, uuid)
	}
	refNode := func(o *orders.Order) string {
		t.Helper()
		hist, err := db.ListOrderHistory(o.ID)
		testutil.MustNoErr(t, err, "history")
		last := hist[len(hist)-1]
		if last.Ref == nil {
			return ""
		}
		return last.Ref.Node
	}

	for _, uuid := range []string{"opr-blank-1", "opr-blank-2"} {
		o := cancelled(uuid, "")
		if got := refNode(o); got != outside.Name {
			t.Errorf("naming no press: the history refers to %q, want the delivery %s", got, outside.Name)
		}
		if got, err := d.laneEntryOriginFor(o); err != nil || got != "" {
			t.Errorf("naming no press: lane-entry origin %q (%v), want unclassified", got, err)
		}
	}
	if got := len(aud.all()); got != 0 {
		t.Fatalf("two futile orders naming no press were counted (%d records): a blank tuple is too vague", got)
	}

	for _, uuid := range []string{"opr-press-1", "opr-press-2"} {
		o := cancelled(uuid, c.press)
		if got := refNode(o); got != c.press {
			t.Errorf("naming its press: the history refers to %q, want %s", got, c.press)
		}
		if got, err := d.laneEntryOriginFor(o); err != nil || got != originKeyForStyles([]string{"OPR-P|OPR-S"}) {
			t.Errorf("naming its press: lane-entry origin %q (%v), want the press's styles", got, err)
		}
	}
	if got := len(aud.all()); got != 1 {
		t.Errorf("two futile orders naming the press at threshold 2 recorded %d times, want once", got)
	}
}
