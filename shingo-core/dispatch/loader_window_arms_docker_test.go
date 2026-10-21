//go:build docker

package dispatch

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// loader_window_arms_docker_test.go — what holds a loader window against a
// person's ask, and which arm of the window choice (plan.LoaderWindows) sees
// an order a plant process sends straight to a window.
//
// The manual retrieve page's ask at a loader window comes to Core as the
// operator origin (www.Doors.AskLoaderRetrieve), in either direction. A window
// with a carrier standing on it, or with a carrier of the ask's direction
// already asked for there, orders nothing and is named in HeldBy: the sentence
// the person reads is built from it.
//
// The two arms. The capacity verdict's in-flight arm (plan.DropoffCapacity,
// orders.InFlightForDropoffSQL) counts any order holding a claimed bin for the
// window, of any type, whatever the ask's direction. The open-ask count
// (orders.CountLiveCarrierRequestsByDeliveryNode) counts live orders of the
// ask's own sourcing intent from birth, claim or none. So an order a claim
// sends to a window is seen in both directions once it holds its bin, and
// before that only by an ask of its own intent: a full retrieve by a full's
// ask, and a move (intent local) by neither.

// oneWindowLoader is a one-window shared loader for payload PART-R, fed from
// its own market, stored as operator-driven: a person's ask at it.
func oneWindowLoader(t *testing.T, db *store.DB, prefix string) (ReplenishRequest, LoaderReplenishConfig, *nodes.Node) {
	t.Helper()
	req, cfg, windows := replenishFixture(t, db, prefix, 1)
	cfg.Replenishment = loaders.ReplenishmentOperator
	w, err := db.GetNodeByDotName(windows[0])
	testutil.MustNoErr(t, err, "resolve the window")
	req.Origin, req.Want = ReplenishByOperator, 1
	req.Threshold, req.CurrentUOP, req.PerBinCapacity = 0, 0, 0
	return req, cfg, w
}

// TestManualRetrieve_AWindowHeldOrdersNothingAndIsNamed: a person's ask for one
// carrier at a one-window loader, empty or full, where a carrier stands on the
// window or one of the ask's direction is already asked for there, orders
// nothing and names the window with its cause.
func TestManualRetrieve_AWindowHeldOrdersNothingAndIsNamed(t *testing.T) {
	t.Parallel()
	for _, pull := range []protocol.OrderType{OrderTypeRetrieveEmpty, OrderTypeRetrieve} {
		for _, held := range []string{"resident", "inbound"} {
			t.Run(fmt.Sprintf("%s/%s", pull, held), func(t *testing.T) {
				t.Parallel()
				db := testDB(t)
				d, _ := newTestDispatcher(t, db, nil)
				req, cfg, w := oneWindowLoader(t, db, "MRH")
				req.OrderType = pull
				want := string(plan.CauseDropoffOccupied)
				if held == "resident" {
					makeEmptyBin(t, db, w.ID, "MRH-STANDING")
				} else {
					want = "window-order-open"
					testutil.MustNoErr(t, db.CreateOrder(&orders.Order{
						EdgeUUID: "MRH-INBOUND", StationID: "edge.line1", OrderType: pull, Status: StatusPending,
						Quantity: 1, SourceNode: cfg.InboundSource, DeliveryNode: w.Name, PayloadCode: req.PayloadCode,
						SourceIntent: SourceIntentForType(pull),
					}), "the carrier already asked for")
				}
				res, err := d.ReplenishLoader(req, cfg)
				testutil.MustNoErr(t, err, "ReplenishLoader")
				if len(res.Created) != 0 {
					t.Errorf("created %d, want none", len(res.Created))
				}
				if res.HeldBy[w.Name] != want {
					t.Errorf("HeldBy = %v (skipped %q), want %s held by %s", res.HeldBy, res.Skipped, w.Name, want)
				}
			})
		}
	}
}

// TestLoaderWindows_AClaimDrivenDeliveryIsSeenByItsArm: an order a plant
// process sends straight to a loader window (a cell's move to an unloader
// window, a claim's full retrieve to it), with its bin claimed (dispatched) and
// without (not yet sourced or dispatched), against an ask for an empty and for
// a full there. Each row names the arm that holds the window, or none.
func TestLoaderWindows_AClaimDrivenDeliveryIsSeenByItsArm(t *testing.T) {
	t.Parallel()
	inflight, open := string(plan.CauseDropoffInflight), "window-order-open"
	cases := []struct {
		name    string
		typ     protocol.OrderType
		claimed bool
		// held is the window's HeldBy for an ask of an empty, then of a full;
		// "" is a window the choice takes.
		heldEmpty, heldFull string
	}{
		{"move claimed", OrderTypeMove, true, inflight, inflight},
		{"move not yet claimed", OrderTypeMove, false, "", ""},
		{"full retrieve claimed", OrderTypeRetrieve, true, inflight, inflight},
		{"full retrieve not yet claimed", OrderTypeRetrieve, false, "", open},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			d, _ := newTestDispatcher(t, db, nil)
			req, cfg, w := oneWindowLoader(t, db, "CDD")
			cell := &nodes.Node{Name: "CDD-CELL", Enabled: true}
			testutil.MustNoErr(t, db.CreateNode(cell), "the cell")
			// A held claim is a dispatched order's (the claim is taken for the fleet
			// call); one not yet claimed is still pending.
			status := StatusPending
			if c.claimed {
				status = StatusDispatched
			}
			o := &orders.Order{
				EdgeUUID: "CDD-CLAIM", StationID: "edge.line1", OrderType: c.typ, Status: status,
				Quantity: 1, SourceNode: cell.Name, DeliveryNode: w.Name, PayloadCode: req.PayloadCode,
				SourceIntent: SourceIntentForType(c.typ),
			}
			testutil.MustNoErr(t, db.CreateOrder(o), "the claim's order to the window")
			if c.claimed {
				b := makeLoaderBin(t, db, req.PayloadCode, cell.ID, "CDD-CARRIED", 10, time.Now())
				// The claim book as a dispatch leaves it: the order holds its bin.
				_, err := db.DB.Exec(`UPDATE bins SET claimed_by = $1 WHERE id = $2`, o.ID, b.ID)
				testutil.MustNoErr(t, err, "the order holds its bin")
			}
			in := loaders.DeliveryTargetsInput{
				Layout: cfg.Layout, Homes: cfg.Homes, NodeNames: cfg.NodeNames, Payloads: cfg.Payloads,
				PayloadCode: req.PayloadCode,
			}
			for _, ask := range []struct {
				intent, want string
			}{{SourceIntentEmpty, c.heldEmpty}, {SourceIntentFull, c.heldFull}} {
				got := d.pure.LoaderWindows(in, 1, ask.intent)
				if got.HeldBy[w.Name] != ask.want {
					t.Errorf("ask of intent %q: HeldBy = %v, want %q", ask.intent, got.HeldBy, ask.want)
				}
				if taken := len(got.Chosen) == 1; taken != (ask.want == "") {
					t.Errorf("ask of intent %q: chosen %v, want the window taken = %v", ask.intent, got.Chosen, ask.want == "")
				}
			}
		})
	}
}
