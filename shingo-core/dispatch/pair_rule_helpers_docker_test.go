//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// pair_rule_helpers_docker_test.go — fixture helpers for the pair-rule tests:
// nodes, a resident bin with its manifest, hand-written leg steps, a scan pass,
// and the "holds nothing" check. The tests that use them are in
// pair_rule_docker_test.go, pair_rule_doors_docker_test.go,
// relay_pair_docker_test.go, dropoff_holders_docker_test.go and
// pair_rule_keep_staged_docker_test.go.

// ── fixture helpers ─────────────────────────────────────────────────────────

func prNode(t *testing.T, db *store.DB, name string) *nodes.Node {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "create node "+name)
	return n
}

// prResident stands a carrier on a line or press position — the bin a removal
// leg lifts, or the on-deck carrier a press-index index leg shifts forward.
func prResident(t *testing.T, db *store.DB, node *nodes.Node, binTypeID int64, payload, label string) *bins.Bin {
	t.Helper()
	b := &bins.Bin{BinTypeID: binTypeID, Label: label, NodeID: &node.ID, Status: "staged"}
	testutil.MustNoErr(t, db.CreateBin(b), "create resident bin "+label)
	// CreateBin writes no payload; a resident carries one, the way
	// testdb.CreateBinAtNode sets it.
	testutil.MustNoErr(t, db.SetBinManifest(b.ID, `{"items":[]}`, payload, 100), "manifest "+label)
	testutil.MustNoErr(t, db.ConfirmBinManifest(b.ID, ""), "confirm manifest "+label)
	got, err := db.GetBin(b.ID)
	testutil.MustNoErr(t, err, "reload resident bin "+label)
	return got
}

func prPick(n string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionPickup, Node: n}
}

func prDrop(n string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: n}
}

// prDropExcl is the declared-exclusive staging dropoff the Edge's stagingDropoff
// builder writes.
func prDropExcl(n string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionDropoff, Node: n, ExclusiveSlot: true}
}

func prWait(n string) protocol.ComplexOrderStep {
	return protocol.ComplexOrderStep{Action: protocol.ActionWait, Node: n}
}

// prResolved turns wire steps into the stored shape, unresolved: a node group
// stays a node group, which is what makes the pair pass the one that resolves it.
func prResolved(steps ...protocol.ComplexOrderStep) []resolvedStep {
	out := make([]resolvedStep, 0, len(steps))
	for _, s := range steps {
		out = append(out, resolvedStep{Action: s.Action, Node: s.Node, Empty: s.Empty,
			PayloadCode: s.PayloadCode, ExclusiveSlot: s.ExclusiveSlot})
	}
	return out
}

// prLegRow inserts one leg of a coordinated pair as intake leaves it: born
// `sourcing`, coordinated, naming its partner. Used where the pass, not intake,
// has to be what meets the condition under test.
func prLegRow(t *testing.T, db *store.DB, uuid, sibling, processNode, deliveryNode, sourceNode, payload string,
	steps []resolvedStep) *orders.Order {
	t.Helper()
	j, err := json.Marshal(steps)
	testutil.MustNoErr(t, err, "marshal steps "+uuid)
	o := &orders.Order{
		EdgeUUID: uuid, StationID: "line-1", OrderType: OrderTypeComplex, Status: StatusSourcing,
		Quantity: 1, PayloadCode: payload, SourceNode: sourceNode, DeliveryNode: deliveryNode,
		ProcessNode: processNode, SiblingOrderUUID: sibling, Coordinated: true, StepsJSON: string(j),
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "create pair leg "+uuid)
	return o
}

// prSubmitLeg sends one leg through the real intake, already naming its partner
// the way every door that pre-mints both uuids does.
func prSubmitLeg(d *Dispatcher, uuid, sibling, payload, processNode string, steps ...protocol.ComplexOrderStep) {
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: uuid, PayloadCode: payload, Quantity: 1, ProcessNode: processNode,
		SiblingOrderUUID: sibling, Steps: steps,
	})
}

func prReload(t *testing.T, db *store.DB, id int64) *orders.Order {
	t.Helper()
	o, err := db.GetOrder(id)
	testutil.MustNoErr(t, err, "reload order")
	if o == nil {
		t.Fatalf("order %d vanished", id)
	}
	return o
}

func prReloadUUID(t *testing.T, db *store.DB, uuid string) *orders.Order {
	t.Helper()
	o, err := db.GetOrderByUUID(uuid)
	testutil.MustNoErr(t, err, "reload order "+uuid)
	if o == nil {
		t.Fatalf("order %s missing — intake refused it", uuid)
	}
	return o
}

// prScanPass stands in for one fulfillment scan over the named legs: each leg
// still acquiring goes to DispatchPreparedComplex in the order given. A pair's
// non-leader call is the election's no-op, so the order given must not change
// the outcome — the callers deliberately pass the non-leader first.
func prScanPass(t *testing.T, d *Dispatcher, db *store.DB, uuids ...string) {
	t.Helper()
	for _, u := range uuids {
		if o := prReloadUUID(t, db, u); protocol.IsAcquiring(o.Status) {
			_ = d.DispatchPreparedComplex(o)
		}
	}
}

// prHoldings reports the three things a parked leg must not keep: claimed bins,
// reservation rows (bin and slot), and lane mouth rows.
func prHoldings(t *testing.T, db *store.DB, orderID int64) (claimed, reserved int, lanes []int64) {
	t.Helper()
	bs, err := db.ListBinsByClaim(orderID)
	testutil.MustNoErr(t, err, "list claimed bins")
	rs, err := db.ListReservationsByOrder(orderID)
	testutil.MustNoErr(t, err, "list reservations")
	ls, err := reservations.LanesHeldByOwner(db.DB, orderID)
	testutil.MustNoErr(t, err, "list held lanes")
	return len(bs), len(rs), ls
}

func prAssertHoldsNothing(t *testing.T, db *store.DB, o *orders.Order, when string) {
	t.Helper()
	c, r, l := prHoldings(t, db, o.ID)
	if c != 0 || r != 0 || len(l) != 0 {
		t.Errorf("%s: order %d (%s) still holds %d claimed bin(s), %d reservation row(s) and lane(s) %v. "+
			"A leg that is not dispatching this pass holds nothing, so the bins and slots it is not using "+
			"stay available to the pair whose completion would free what this one waits on",
			when, o.ID, o.EdgeUUID, c, r, l)
	}
	// AND IT NAMES NOTHING. orders.bin_id is the pointer half of the same book: a
	// leg that gave its claim back but still names the bin disagrees with itself,
	// and every reader of the pointer between this pass and the next is told the
	// order holds a bin somebody else may already have taken.
	if fresh := prReload(t, db, o.ID); fresh.BinID != nil {
		t.Errorf("%s: order %d (%s) released its holds but still points at bin %d — a hold released without "+
			"its pointer", when, o.ID, o.EdgeUUID, *fresh.BinID)
	}
}

// prDigRow reports whether owner holds a mode='dig' mouth row on the lane — the
// row a §R.91 dig takes in its demand's own name.
func prDigRow(t *testing.T, db *store.DB, laneID, owner int64) bool {
	t.Helper()
	hs, err := reservations.ActiveMouthRows(db.DB, laneID)
	testutil.MustNoErr(t, err, "read lane mouth rows")
	for _, h := range hs {
		if h.OrderID == owner && h.Mode == reservations.ModeDig {
			return true
		}
	}
	return false
}
