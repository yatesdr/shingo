//go:build docker

package engine

import (
	"fmt"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// tote_lift_docker_test.go — the lift's tote hook (Engine.deriveTotesAtLift,
// store.DB.DeriveTotesAtLift): when a press cell's outside cart is lifted, what the
// front's build drew from it is written on both carts, once per (cart, order).
//
// The cell: <prefix>-PRESS is the press, <prefix>-OUT the paired position behind it. The
// part makes 10 UOP per tote, 5 layers of a type with 8 totes a layer (a full
// stack is 40). The front stands on the press with 135 counted and carried no
// totes; the outside cart stands on TL-OUT with 40.

type toteCell struct {
	db             *store.DB
	eng            *Engine
	press, out     *nodes.Node
	front, outside int64
}

func newToteCell(t *testing.T, db *store.DB, eng *Engine, prefix string) toteCell {
	t.Helper()
	c := toteCell{db: db, eng: eng}
	_, err := db.Exec(fmt.Sprintf(`INSERT INTO payloads (code, tote_layers, uop_per_tote) VALUES ('%s-PART', 5, 10)`, prefix))
	testutil.MustNoErr(t, err, "payload")
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, totes_per_layer) VALUES ($1, 8) RETURNING id`,
		prefix+"-TOTE").Scan(&btID), "tote type")
	c.press = &nodes.Node{Name: prefix + "-PRESS", Enabled: true}
	c.out = &nodes.Node{Name: prefix + "-OUT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(c.press), "press")
	testutil.MustNoErr(t, db.CreateNode(c.out), "outside position")
	_, err = db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node)
		VALUES ($1, 'S1', $2, 'produce', '', $3, $4)`, prefix+"-P", c.press.Name, prefix+"-PART", c.out.Name)
	testutil.MustNoErr(t, err, "claim")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins
		(bin_type_id, label, node_id, status, payload_code, uop_remaining, manifest_confirmed, totes_on_cart)
		VALUES ($1, $2, $3, 'available', $4, 135, true, 0) RETURNING id`,
		btID, prefix+"-FRONT", c.press.ID, prefix+"-PART").Scan(&c.front), "front")
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bins (bin_type_id, label, node_id, status, totes_on_cart)
		VALUES ($1, $2, $3, 'available', 40) RETURNING id`, btID, prefix+"-OUTSIDE", c.out.ID).Scan(&c.outside), "outside cart")
	return c
}

// order makes a complex order holding bin, with its two-robot sibling's uuid.
func (c toteCell) order(t *testing.T, uuid, sibling string, bin int64, processNode string) *orders.Order {
	t.Helper()
	o := &orders.Order{EdgeUUID: uuid, StationID: "line-1", OrderType: "complex", Status: "in_transit",
		Quantity: 1, ProcessNode: processNode, SiblingOrderUUID: sibling, BinID: &bin, StepsJSON: "[]"}
	testutil.MustNoErr(t, c.db.CreateOrder(o), "order "+uuid)
	_, err := c.db.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, o.ID, bin)
	testutil.MustNoErr(t, err, "claim")
	return o
}

func (c toteCell) lift(o *orders.Order, at string) {
	c.eng.handlePickupBlockCompleted(BlockCompletedEvent{OrderID: o.ID, BlockID: "b-" + at, Location: at, BinTask: "JackLoad"})
}

func totesOn(t *testing.T, db *store.DB, bin int64) string {
	t.Helper()
	var n *int
	testutil.MustNoErr(t, db.QueryRow(`SELECT totes_on_cart FROM bins WHERE id = $1`, bin).Scan(&n), "totes")
	if n == nil {
		return "unknown"
	}
	return fmt.Sprint(*n)
}

func derivedRows(t *testing.T, db *store.DB, bin int64) int {
	t.Helper()
	var n int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM bin_uop_ledger WHERE bin_id = $1 AND op = $2`,
		bin, bins.OpTotesDerived).Scan(&n), "ledger")
	return n
}

func (c toteCell) expect(t *testing.T, outside, front string, rows int) {
	t.Helper()
	if got := totesOn(t, c.db, c.outside); got != outside {
		t.Errorf("outside cart has %s totes, want %s", got, outside)
	}
	if got := totesOn(t, c.db, c.front); got != front {
		t.Errorf("front cart has %s totes, want %s", got, front)
	}
	if got := derivedRows(t, c.db, c.outside); got != rows {
		t.Errorf("outside cart has %d totes_derived ledger rows, want %d", got, rows)
	}
}

// TestToteLift_AnIndex: the index leg lifts the outside cart while the front
// still stands on the press: 135 counted is 14 totes filled, all drawn from the
// outside cart, which keeps 26; the front holds 14. A ledger row on each cart.
func TestToteLift_AnIndex(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLA")
	c.order(t, "tla-evac", "tla-index", c.front, c.press.Name)
	index := c.order(t, "tla-index", "tla-evac", c.outside, c.press.Name)
	c.lift(index, c.out.Name)
	c.expect(t, "26", "14", 1)
	if got := derivedRows(t, db, c.front); got != 1 {
		t.Errorf("front cart has %d totes_derived ledger rows, want 1", got)
	}
}

// TestToteLift_TheFrontAlreadyLiftedBySibling: the evac leg lifted the front
// first; the index finds it by id through the sibling's pickup off the press.
func TestToteLift_TheFrontAlreadyLiftedBySibling(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLS")
	evac := c.order(t, "tls-evac", "tls-index", c.front, c.press.Name)
	testutil.MustNoErr(t, db.InsertOrderBin(evac.ID, c.front, 1, "pickup", c.press.Name, "TLS-MARKET"), "junction")
	index := c.order(t, "tls-index", "tls-evac", c.outside, c.press.Name)
	c.lift(evac, c.press.Name)
	c.lift(index, c.out.Name)
	c.expect(t, "26", "14", 1)
}

// TestToteLift_ARetriedLiftWritesOnce: the vendor reports the pickup again (the
// cart is already in transit), and then the same order lifts the same cart
// again after setting it back: one derivation per (cart, order).
func TestToteLift_ARetriedLiftWritesOnce(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLR")
	c.order(t, "tlr-evac", "tlr-index", c.front, c.press.Name)
	index := c.order(t, "tlr-index", "tlr-evac", c.outside, c.press.Name)
	c.lift(index, c.out.Name)
	c.lift(index, c.out.Name)
	_, err := db.Exec(`UPDATE bins SET node_id = $1, totes_on_cart = 40 WHERE id = $2`, c.out.ID, c.outside)
	testutil.MustNoErr(t, err, "set the cart back")
	c.lift(index, c.out.Name)
	c.expect(t, "40", "14", 1)
}

// TestToteLift_AnIndexBeforeTheIngestLanded: the front's count is not yet
// confirmed, so it is not trusted at an index: the outside cart is written
// unknown, and the front keeps the totes it carried (0 here), which it still
// holds at least.
func TestToteLift_AnIndexBeforeTheIngestLanded(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLI")
	_, err := db.Exec(`UPDATE bins SET manifest_confirmed = false WHERE id = $1`, c.front)
	testutil.MustNoErr(t, err, "unconfirm")
	c.order(t, "tli-evac", "tli-index", c.front, c.press.Name)
	index := c.order(t, "tli-index", "tli-evac", c.outside, c.press.Name)
	c.lift(index, c.out.Name)
	c.expect(t, "unknown", "0", 1)
}

// TestToteLift_APerPositionLiftWithTheFrontGone: a changeover's per-position
// order (no sibling) lifts the outside cart after the front has left the press:
// there is no front to read, so the outside is written unknown.
func TestToteLift_APerPositionLiftWithTheFrontGone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLC")
	_, err := db.Exec(`UPDATE bins SET node_id = NULL WHERE id = $1`, c.front)
	testutil.MustNoErr(t, err, "the front leaves")
	per := c.order(t, "tlc-out", "", c.outside, c.press.Name)
	c.lift(per, c.out.Name)
	c.expect(t, "unknown", "0", 1)
}

// TestToteLift_OffANodeThatIsNoPressPosition: a numbered empty cart lifted off
// a node no claim pairs writes nothing.
func TestToteLift_OffANodeThatIsNoPressPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLN")
	free := &nodes.Node{Name: "TLN-FREE", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(free), "free node")
	_, err := db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, free.ID, c.outside)
	testutil.MustNoErr(t, err, "move the cart")
	o := c.order(t, "tln-move", "", c.outside, "")
	c.lift(o, free.Name)
	c.expect(t, "40", "0", 0)
}

// TestToteLift_ASequentialPartnerIsNoOutside: two presses of one process and
// style that name each other as paired. A numbered empty cart lifted off one of
// them is not an outside cart: nothing is written on it or on the other press's.
func TestToteLift_ASequentialPartnerIsNoOutside(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TLQ")
	_, err := db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, paired_core_node)
		VALUES ('TLQ-P', 'S1', $1, 'produce', '', 'TLQ-PART', $2)`, c.out.Name, c.press.Name)
	testutil.MustNoErr(t, err, "the partner's claim")
	c.order(t, "tlq-evac", "tlq-index", c.front, c.press.Name)
	index := c.order(t, "tlq-index", "tlq-evac", c.outside, c.press.Name)
	c.lift(index, c.out.Name)
	c.expect(t, "40", "0", 0)
}

// TestToteLift_ASecondPairedPositionIsNoOutside: in a three-position cell the
// second paired position's cart is not the outside cart: a numbered empty
// lifted off it writes nothing on it or on the front.
func TestToteLift_ASecondPairedPositionIsNoOutside(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	c := newToteCell(t, db, newTestEngine(t, db, simulator.New()), "TL3")
	rear := &nodes.Node{Name: "TL3-REAR", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(rear), "rear position")
	_, err := db.Exec(`UPDATE style_claims SET second_paired_core_node = $1 WHERE core_node_name = $2`, rear.Name, c.press.Name)
	testutil.MustNoErr(t, err, "three positions")
	_, err = db.Exec(`UPDATE bins SET node_id = $1 WHERE id = $2`, rear.ID, c.outside)
	testutil.MustNoErr(t, err, "the cart at the rear")
	c.order(t, "tl3-evac", "tl3-index", c.front, c.press.Name)
	index := c.order(t, "tl3-index", "tl3-evac", c.outside, c.press.Name)
	c.lift(index, rear.Name)
	c.expect(t, "40", "0", 0)
}

// TestToteLift_NoNumberNoStatement: at a plant that sets no tote numbers the
// lift issues exactly the statements it issued before the hook, and writes no
// tote and no ledger row; the same lift of a numbered cart off a press's
// outside position adds the hook's own (one read, one write, two ledger rows,
// in its own transaction).
func TestToteLift_NoNumberNoStatement(t *testing.T) {
	t.Parallel()
	db, cfg := testdb.OpenWithConfig(t)
	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting db")
	t.Cleanup(func() { cdb.Close() })
	eng := newUnstartedEngine(t, cdb, simulator.New())

	lifted := func(prefix string, numbered bool) []string {
		c := newToteCell(t, db, eng, prefix)
		if !numbered {
			_, err := db.Exec(`UPDATE bins SET totes_on_cart = NULL WHERE id IN ($1, $2)`, c.front, c.outside)
			testutil.MustNoErr(t, err, "no numbers")
		}
		c.order(t, strings.ToLower(prefix)+"-evac", strings.ToLower(prefix)+"-index", c.front, c.press.Name)
		index := c.order(t, strings.ToLower(prefix)+"-index", strings.ToLower(prefix)+"-evac", c.outside, c.press.Name)
		counter.Reset()
		c.lift(index, c.out.Name)
		stmts := counter.Statements()
		if !numbered {
			c.expect(t, "unknown", "unknown", 0)
		}
		return stmts
	}
	plain := lifted("TLP", false)
	numbered := lifted("TLM", true)
	t.Logf("a lift with no numbers: %d statements; a numbered cart off the outside position: %d",
		len(plain), len(numbered))
	for _, s := range plain {
		if strings.Contains(s, "style_claims") || strings.Contains(s, bins.OpTotesDerived) {
			t.Errorf("a lift with no numbers ran the tote hook:\n%s", s)
		}
	}
}
