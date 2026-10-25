package domain

import (
	"testing"

	"shingo/protocol"
	"shingo/shared/fedshape"
)

// TestFedShape_TheSharedShapeTable: the Edge's shape test answers every case
// Core's FedClaimSQL answers (shared/fedshape): the paired position names no
// claim of the case's claims, asked through PairedClaimed over those claims by
// core node, as SEND BACK's gatherers ask it.
func TestFedShape_TheSharedShapeTable(t *testing.T) {
	t.Parallel()
	for _, c := range fedshape.MustLoad() {
		byNode := map[string]NodeClaim{}
		for _, cl := range c.Claims {
			byNode[cl.Node] = NodeClaim{CoreNodeName: cl.Node, PairedCoreNode: cl.Paired, SecondPairedCoreNode: cl.SecondPaired}
		}
		ask := byNode[c.Ask]
		if got := FedShape(ask.PairedCoreNode, PairedClaimed(&ask, byNode)); got != c.WantFed {
			t.Errorf("%s: FedShape = %v, want %v\nwhy this case exists: %s", c.Name, got, c.WantFed, c.Why)
		}
	}
}

func sbInt(n int) *int { return &n }

// toteCart is a tote cart's row: 8 a layer, totes on it.
func toteCart(label string, totes int) *protocol.NodeBinsRow {
	return &protocol.NodeBinsRow{Occupied: true, BinLabel: label, BinTypeCode: "TOTE", TotesPerLayer: sbInt(8), TotesOnCart: sbInt(totes)}
}

// sendBackPress is the facts of a produce tote press fed from behind, its
// outside holding a cart 3 high, its core a tote cart, nothing live: enabled.
func sendBackPress() SendBackFacts {
	return SendBackFacts{Produce: true, Paired: "OUT", OutsideNode: true, Outside: toteCart("CART-7", 24),
		Core: toteCart("CART-6", 0), Outbound: "UNLOADER"}
}

// TestSendBackLine_TheStates: each absent case, enabled, disabled and waiting,
// in the precedence the line gives them.
func TestSendBackLine_TheStates(t *testing.T) {
	t.Parallel()
	knockdown := &protocol.NodeBinsRow{Occupied: true, BinLabel: "KD-1", BinTypeCode: "KD"}
	bare := toteCart("BARE", 0)
	bare.Bare = true
	unverified := toteCart("UNV", 0)
	unverified.TotesOnCart = nil
	pull := Order{OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Want: protocol.TopsAndBottomsOnly,
		Status: protocol.StatusQueued, DeliveryNode: "OUT", SourceNode: "SUPER", QueueReason: "Waiting for tops & bottoms in SUPER"}
	move := Order{OrderType: protocol.OrderTypeMove, Status: protocol.StatusInTransit, SourceNode: "OUT"}
	for _, k := range []struct {
		name      string
		edit      func(*SendBackFacts)
		want      SendBackState
		canCancel bool
	}{
		{"a fed produce tote press", func(*SendBackFacts) {}, SendBackEnabled, false},
		{"consume", func(f *SendBackFacts) { f.Produce = false }, SendBackAbsent, false},
		{"a sequential cell (the paired position is a claim)", func(f *SendBackFacts) { f.PairedClaimed = true }, SendBackAbsent, false},
		{"no paired position", func(f *SendBackFacts) { f.Paired = "" }, SendBackAbsent, false},
		{"an outside with no process node row", func(f *SendBackFacts) { f.OutsideNode = false }, SendBackAbsent, false},
		{"a knockdown press", func(f *SendBackFacts) { f.Outside, f.Core = knockdown, knockdown }, SendBackAbsent, false},
		{"a wire basket on the press", func(f *SendBackFacts) { f.Core = knockdown }, SendBackAbsent, false},
		{"nothing on the outside", func(f *SendBackFacts) { f.Outside = &protocol.NodeBinsRow{} }, SendBackAbsent, false},
		{"no row for the outside", func(f *SendBackFacts) { f.Outside = nil }, SendBackAbsent, false},
		{"a bare cart outside", func(f *SendBackFacts) { f.Outside = bare }, SendBackAbsent, false},
		{"tops & bottoms already outside", func(f *SendBackFacts) { f.Outside = toteCart("TB", 0) }, SendBackAbsent, false},
		{"a cart nobody counted is sent back", func(f *SendBackFacts) { f.Outside = unverified }, SendBackEnabled, false},
		{"no cart on the press", func(f *SendBackFacts) { f.Core = &protocol.NodeBinsRow{} }, SendBackEnabled, false},
		{"a changeover armed", func(f *SendBackFacts) { f.ChangeoverArmed = true }, SendBackAbsent, false},
		{"another order places at the outside", func(f *SendBackFacts) { f.PlacingAtOutside = true }, SendBackDisabled, false},
		{"a swap queued or in flight", func(f *SendBackFacts) { f.SwapLive = true }, SendBackDisabled, false},
		{"its own orders, both unmoved", func(f *SendBackFacts) {
			queued := move
			queued.Status = protocol.StatusQueued
			f.Own = []Order{queued, pull}
		}, SendBackWaiting, true},
		{"the move has lifted, the pull waits", func(f *SendBackFacts) { f.Own = []Order{move, pull} }, SendBackWaiting, true},
		{"waiting wins over a swap and a changeover", func(f *SendBackFacts) {
			f.Own, f.SwapLive, f.ChangeoverArmed, f.Outside = []Order{pull}, true, true, toteCart("TB", 0)
		}, SendBackWaiting, true},
		{"both moving", func(f *SendBackFacts) {
			moving := pull
			moving.Status = protocol.StatusDispatched
			f.Own = []Order{move, moving}
		}, SendBackWaiting, false},
	} {
		f := sendBackPress()
		k.edit(&f)
		got := SendBackLine(f)
		if got.State != k.want || got.CanCancel != k.canCancel {
			t.Errorf("%s: state %q cancel %v, want %q cancel %v", k.name, got.State, got.CanCancel, k.want, k.canCancel)
		}
		if got.State == SendBackWaiting && got.QueueReason != "" && got.QueueReason != pull.QueueReason {
			t.Errorf("%s: the line says %q, want the pull's own %q", k.name, got.QueueReason, pull.QueueReason)
		}
	}
	if got := SendBackLine(sendBackPress()); got.Outside != "OUT" || got.Bin != "CART-7" || got.Outbound != "UNLOADER" {
		t.Errorf("the enabled line names %+v", got)
	}
}

// TestSendBackOrders_TheMoveAndThePullOnly: SEND BACK's pull is the empty
// retrieve to the outside wanting tops & bottoms only, as this Edge wrote it
// and as Core's echo leaves it (retrieve_empty). A fanned-out changeover's legs
// on the outside's process node (complex legs, a plain retrieve or an empty to
// the outside stating no want, a changeover Add still queued there) are never
// SEND BACK's, and neither is an empty to elsewhere stating the same want; a
// terminal one of its own is not live.
func TestSendBackOrders_TheMoveAndThePullOnly(t *testing.T) {
	t.Parallel()
	tb := protocol.TopsAndBottomsOnly
	rows := []Order{
		{ID: 1, OrderType: protocol.OrderTypeMove, Status: protocol.StatusQueued, SourceNode: "OUT", DeliveryNode: "UNLOADER"},
		{ID: 2, OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Want: tb, Status: protocol.StatusQueued, DeliveryNode: "OUT"},
		{ID: 3, OrderType: protocol.OrderTypeComplex, Status: protocol.StatusQueued, DeliveryNode: "OUT"},
		{ID: 4, OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusQueued, DeliveryNode: "OUT"},
		{ID: 5, OrderType: protocol.OrderTypeComplex, Status: protocol.StatusInTransit, SourceNode: "OUT"},
		{ID: 6, OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Want: tb, Status: protocol.StatusConfirmed, DeliveryNode: "OUT"},
		{ID: 7, OrderType: protocol.OrderTypeMove, Status: protocol.StatusQueued, SourceNode: "ELSEWHERE"},
		{ID: 8, OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Status: protocol.StatusQueued, DeliveryNode: "OUT"},
		{ID: 9, OrderType: protocol.OrderTypeRetrieve, RetrieveEmpty: true, Want: tb, Status: protocol.StatusQueued, DeliveryNode: "ELSEWHERE"},
		{ID: 10, OrderType: protocol.OrderTypeRetrieveEmpty, RetrieveEmpty: true, Want: tb, Status: protocol.StatusDispatched, DeliveryNode: "OUT"},
	}
	got := SendBackOrders(rows, "OUT")
	if len(got) != 3 || got[0].ID != 1 || got[1].ID != 2 || got[2].ID != 10 {
		t.Errorf("SEND BACK's orders: %+v, want orders 1, 2 and 10", got)
	}
}
