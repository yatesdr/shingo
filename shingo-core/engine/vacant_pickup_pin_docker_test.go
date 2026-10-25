//go:build docker

package engine

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// TestVacantPickupPin_AnOnDeckEmptyTakenWhileParkedIsFetchedFromTheInboundSource
// is DESIGN-D2 §7 and D2.0(iii), ruled into Lane 0 item 3 (F9): a produce
// press-index pair's index leg names its on-deck empty by node (the Edge's R2:
// wait at the paired position, pick up its empty, drop it at the press). The
// empty was taken while the pair waited, the node is vacant, and nothing re-plans
// the leg: widenSupplyPickups skips empty pickups, replay re-resolves only steps
// still naming a group, and the reserve holds a partial set for ever, retried
// every pass (carry-edge2's order 346 held its set 8,662 times).
//
// The leg asks the planner for an empty from its claim's inbound source and
// goes. RED at 545b8b57: it holds under reserve-holding pass after pass.
func TestVacantPickupPin_AnOnDeckEmptyTakenWhileParkedIsFetchedFromTheInboundSource(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	sd := testdb.SetupStandardData(t, db)
	eng := newTestEngine(t, db, simulator.New())

	mk := func(name string) *nodes.Node {
		n := &nodes.Node{Name: name, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(n), name)
		return n
	}
	press, onDeck, empties, fg := mk("VAC-PRESS"), mk("VAC-ONDECK"), mk("VAC-EMPTIES"), mk("VAC-FG")
	carrier := testdb.CreateBinAtNode(t, db, "", empties.ID, "VAC-EMPTY-1")

	const station, process, style = "line-1", "VAC-PROC", "VAC-STYLE"
	_, err := db.ReplacePlantClaimsFromStation(process, station,
		[]plantclaims.StyleRow{{ProcessID: process, StyleID: style, IsActive: true}},
		[]plantclaims.ClaimRow{{
			ProcessID: process, StyleID: style, CoreNodeName: press.Name,
			Role: protocol.ClaimRoleProduce, SwapMode: protocol.SwapModeTwoRobotPressIndex,
			PayloadCode: sd.Payload.Code, InboundSource: empties.Name,
			OutboundDestination: fg.Name, PairedCoreNode: onDeck.Name,
		}}, nil, 0)
	testutil.MustNoErr(t, err, "the press's claim")

	// The index leg, as the Edge builds it for a produce press-index claim: a
	// steady swap, attached to its cell's episode.
	origin := testdb.OpenEpisode(t, db, protocol.EpisodeKindCell,
		protocol.CellEpisodeKey(process, sd.Payload.Code, protocol.ClaimRoleProduce))
	eng.Dispatcher().HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: "vacant-index-leg", PayloadCode: sd.Payload.Code, Quantity: 1, ProcessNode: press.Name,
		OriginID: origin, OriginClass: protocol.OriginClassAttached,
		Steps: []protocol.ComplexOrderStep{
			{Action: protocol.ActionWait, Node: onDeck.Name},
			{Action: protocol.ActionPickup, Node: onDeck.Name, Empty: true},
			{Action: protocol.ActionDropoff, Node: press.Name},
		},
	})
	for range 3 {
		eng.RunFulfillmentScan()
	}

	o := testdb.RequireOrder(t, db, "vacant-index-leg")
	if o.Status != protocol.StatusDispatched {
		t.Fatalf("the index leg is %s (code %q, cause %q) after three passes; want dispatched with the empty from %s",
			o.Status, o.QueueCode, o.QueueCause, empties.Name)
	}
	var steps []protocol.ComplexOrderStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(o.StepsJSON), &steps), "steps")
	if steps[1].Node != empties.Name {
		t.Errorf("the empty pickup is at %s, want %s (the claim's inbound source)", steps[1].Node, empties.Name)
	}
	bin, err := db.GetBin(carrier.ID)
	testutil.MustNoErr(t, err, "the carrier")
	if bin.ClaimedBy == nil || *bin.ClaimedBy != o.ID {
		t.Errorf("the carrier is claimed by %v, want the index leg %d", bin.ClaimedBy, o.ID)
	}
}
