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
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// TestComingVectors_TheWindowReadersAgree: each vector's live row, stored as
// Core stores it (store TestComingVectors_CoreAgreesOverStoredRows: a complex
// leg's steps and its last-acted node as delivery_node; a plain order's
// delivery node and plan), holding its carrier as a dispatched order does,
// against a window at every node the case names: FreeWindows and the window
// choice (LoaderWindows, an ask of each intent) each say whether a carrier is
// coming there.
//
// THEY DO NOT, AND THIS IS A PREDICTED RED (rule 8, STOPS: "the window readers
// and the coming rule"). Both read delivery_node, so a complex leg that places
// at a node it does not end at reads as not coming there: an evac that drops at
// OUT and then refills the press position, a single-robot swap that places at
// LINE and carries on. Every pair disagrees one way (the vectors say coming, all
// three readers say free). Composing OrderPlacesBinAt into FreeWindows costs a
// steps read per live row per window per pull, and into the choice's open-ask
// arm would count an evac's arrival as an ask, the 2026-08-05 window deadlock.
// Whether a plant aims such a leg at a loader window is the owner's to rule.
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
			free, err := d.FreeWindows([]*nodes.Node{w})
			testutil.MustNoErr(t, err, c.Name)
			in := loaders.DeliveryTargetsInput{Layout: loaders.LayoutDedicatedPositions,
				Homes: []loaders.Home{{PositionNodeID: w.ID}}, NodeNames: map[int64]string{w.ID: w.Name}}
			byFree := len(free) == 0
			byEmpty := d.pure.LoaderWindows(in, 1, SourceIntentEmpty).HeldBy[w.Name] != ""
			byFull := d.pure.LoaderWindows(in, 1, SourceIntentFull).HeldBy[w.Name] != ""
			if byFree != want || byEmpty != want || byFull != want {
				disagree = append(disagree, fmt.Sprintf("%s | at %s: vectors %v, FreeWindows %v, LoaderWindows(empty) %v, LoaderWindows(full) %v | delivery_node %s | steps %s",
					c.Name, n, want, byFree, byEmpty, byFull, coreDelivery, strings.ReplaceAll(string(c.Steps), "\n", "")))
			}
		}
		_, err = db.DB.Exec(`UPDATE bins SET claimed_by = NULL WHERE id = $1`, b.ID)
		testutil.MustNoErr(t, err, "release "+c.Name)
		_, err = db.DB.Exec(`UPDATE orders SET status = 'cancelled' WHERE id = $1`, o.ID)
		testutil.MustNoErr(t, err, "retire "+c.Name)
	}
	testdb.VerifyRed(t, "rule 8: the window readers against the coming vectors", disagree)
}
