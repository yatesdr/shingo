//go:build docker

package dispatch

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingo/shared/comingvectors"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestComingVectors_TheWindowReadersAgree: each vector's live row, stored as
// Core stores it (store TestComingVectors_CoreAgreesOverStoredRows: a complex
// leg's steps and its last-acted node as delivery_node; a plain order's
// delivery node and plan), holding its carrier as a dispatched order does,
// against a window at every node the case names.
//
// THE PULLS' WINDOWS ARE THE RULE'S: freeWindows (the drain's and stage 2's)
// says a window is taken exactly where the vectors say a carrier is coming,
// including a leg that sets its carrier down there before its last act (an
// evac that drops at OUT and then refills the press position, a single-robot
// swap that places at LINE and carries on).
//
// THE WINDOW CHOICE IS RUN, NOT HELD TO THE VECTORS (owner, 2026-10-25): an
// ask asks where nothing of its own intent is asked (the evac's arrival read as
// an ask deadlocked SMN_030 on 2026-08-05), and its arrivals are the dropoff
// gate's in-flight arm, which every drop shares. Both read delivery_node, so
// the choice (LoaderWindows, an ask of each intent) says taken exactly where
// the row's delivery node is the window, and that is what is asserted of it.
func TestComingVectors_TheWindowReadersAgree(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	d, _ := newTestDispatcher(t, db, nil)
	cases, err := comingvectors.Cases()
	testutil.MustNoErr(t, err, "the vectors")
	carry := &nodes.Node{Name: "CV-CARRY", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(carry), "the carry node")
	cartType := &bins.BinType{Code: "CV-CART"}
	testutil.MustNoErr(t, db.CreateBinType(cartType), "the cart type")
	var disagree []string
	for i, c := range cases {
		typ := protocol.OrderType(c.OrderType)
		if typ == protocol.OrderTypeComplex && len(c.Steps) == 0 {
			continue
		}
		prefix := fmt.Sprintf("CV%d-", i)
		windows := map[string]*nodes.Node{}
		for n := range c.Places {
			w := &nodes.Node{Name: prefix + n, Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(w), "window "+w.Name)
			windows[n] = w
		}
		var steps []protocol.ComplexOrderStep
		if len(c.Steps) > 0 {
			testutil.MustNoErr(t, json.Unmarshal(c.Steps, &steps), c.Name)
		}
		for j := range steps {
			if steps[j].Node != "" {
				steps[j].Node = prefix + steps[j].Node
			}
		}
		raw := ""
		if len(steps) > 0 {
			b, err := json.Marshal(steps)
			testutil.MustNoErr(t, err, c.Name)
			raw = string(b)
		}
		delivery, coreDelivery := "", ""
		if c.DeliveryNode != "" {
			delivery = prefix + c.DeliveryNode
		}
		if typ == protocol.OrderTypeComplex {
			_, delivery = extractEndpoints(stepsAsResolved(steps))
		}
		coreDelivery = strings.TrimPrefix(delivery, prefix)
		o := &orders.Order{EdgeUUID: prefix + "ROW", StationID: "line-1", OrderType: typ, Status: StatusDispatched,
			Quantity: 1, DeliveryNode: delivery, StepsJSON: raw, SourceIntent: SourceIntentForType(typ)}
		testutil.MustNoErr(t, db.CreateOrder(o), c.Name)
		b := &bins.Bin{BinTypeID: cartType.ID, Label: prefix + "CART", NodeID: &carry.ID, Status: "available"}
		testutil.MustNoErr(t, db.CreateBin(b), "the row's cart")
		_, err := db.DB.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, o.ID, b.ID)
		testutil.MustNoErr(t, err, "the row holds its carrier")
		names := make([]string, 0, len(c.Places))
		for n := range c.Places {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, n := range names {
			want, w := c.Places[n], windows[n]
			free, err := d.freeWindows([]*nodes.Node{w})
			testutil.MustNoErr(t, err, c.Name)
			in := loaders.DeliveryTargetsInput{Layout: loaders.LayoutDedicatedPositions,
				Homes: []loaders.Home{{PositionNodeID: w.ID}}, NodeNames: map[int64]string{w.ID: w.Name}}
			byFree := len(free) == 0
			byEmpty := d.pure.LoaderWindows(in, 1, SourceIntentEmpty).HeldBy[w.Name] != ""
			byFull := d.pure.LoaderWindows(in, 1, SourceIntentFull).HeldBy[w.Name] != ""
			byDelivery := coreDelivery == n
			if byFree != want || byEmpty != byDelivery || byFull != byDelivery {
				disagree = append(disagree, fmt.Sprintf("%s | at %s: vectors %v, freeWindows %v; delivery_node %s, LoaderWindows(empty) %v, LoaderWindows(full) %v | steps %s",
					c.Name, n, want, byFree, coreDelivery, byEmpty, byFull, strings.ReplaceAll(string(c.Steps), "\n", "")))
			}
		}
		_, err = db.DB.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, b.ID)
		testutil.MustNoErr(t, err, "release "+c.Name)
		_, err = db.DB.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, o.ID)
		testutil.MustNoErr(t, err, "retire "+c.Name)
	}
	if len(disagree) > 0 {
		t.Errorf("rule 8: %d (case, window) pairs disagree:\n%s", len(disagree), strings.Join(disagree, "\n"))
	}
}
