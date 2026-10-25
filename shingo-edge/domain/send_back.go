package domain

import (
	"shingo/protocol"
	"shingoedge/release"
)

// send_back.go — SEND BACK, decided pure: from facts its readers hold as
// values (the station view, the route, CANCEL and START read them), the line
// on a tote press's core position and the orders that are SEND BACK's.
//
// The operator on a tote press taps SEND BACK: the cart on the press's outside
// position goes to the outbound (the unloader's card decides what it goes back
// as), and tops & bottoms of the press's tote come in its place.

// SendBackState is the line's one state.
type SendBackState string

const (
	// SendBackAbsent: no line. The zero value.
	SendBackAbsent SendBackState = ""
	// SendBackEnabled: the button acts.
	SendBackEnabled SendBackState = "enabled"
	// SendBackDisabled: shown, not acting: another order places at the
	// outside, or a swap is queued or in flight on the press.
	SendBackDisabled SendBackState = "disabled"
	// SendBackWaiting: SEND BACK's own orders are live.
	SendBackWaiting SendBackState = "waiting"
)

// FedShape is the Edge's test of a press fed from behind: its paired position
// is set and no claim of the running style stands on it. One shape with Core's
// (store/bins FedClaimSQL), pinned by the table both read (shared/fedshape).
func FedShape(paired string, pairedClaimed bool) bool {
	return paired != "" && !pairedClaimed
}

// SendBackFacts is what the line is decided from. The caller reads them; the
// decision reads nothing.
type SendBackFacts struct {
	// Produce is the claim's role: produce is the one role SEND BACK serves.
	Produce bool
	// Paired is the claim's paired position, the outside; PairedClaimed says a
	// claim of the running style stands on it (FedShape).
	Paired        string
	PairedClaimed bool
	// OutsideNode: the outside has a process node row on the process, where
	// SEND BACK's orders are created.
	OutsideNode bool
	// Outside is the outside's row in the Edge's copy of Core's bins, nil when
	// the copy has none; Core the core position's, nil when none.
	Outside *protocol.NodeBinsRow
	Core    *protocol.NodeBinsRow
	// Outbound is the claim's outbound destination, where the cart goes.
	Outbound        string
	ChangeoverArmed bool
	// Own is SEND BACK's live orders (SendBackOrders).
	Own []Order
	// PlacingAtOutside: an order other than SEND BACK's own places a carrier
	// at the outside. SwapLive: a swap is queued or in flight on the press.
	PlacingAtOutside bool
	SwapLive         bool
}

// SendBack is the decided line.
type SendBack struct {
	State    SendBackState `json:"state"`
	Outside  string        `json:"outside"`
	Bin      string        `json:"bin"`
	Outbound string        `json:"outbound"`
	// CanCancel: waiting, with an order of its own that CANCEL would abort
	// (protocol.ChangeoverStartActionFor says Cancel).
	CanCancel bool `json:"can_cancel"`
	// QueueReason is the pull's own, Core's sentence; never composed here.
	QueueReason string `json:"queue_reason"`
}

// SendBackLine decides the line. Precedence: waiting, then disabled, then
// enabled; absent when the press is not SEND BACK's.
//
//   - Absent: a claim that is not produce (THE ONE ROLE READ), not of the fed
//     shape, or whose outside has no process node row; then, unless waiting,
//     an outside holding no cart, or a cart that is not a tote type, is bare,
//     or is tops & bottoms already; a core cart standing that is not a tote
//     type; a changeover armed.
//   - Waiting: SEND BACK's own orders are live, whatever stands on the outside
//     (its own pull places there), with CANCEL while one is unmoved.
//   - Disabled: another order places at the outside, or a swap is queued or in
//     flight on the press.
func SendBackLine(f SendBackFacts) SendBack {
	if !f.Produce || !FedShape(f.Paired, f.PairedClaimed) || !f.OutsideNode {
		return SendBack{}
	}
	line := SendBack{Outside: f.Paired, Outbound: f.Outbound}
	if f.Outside != nil {
		line.Bin = f.Outside.BinLabel
	}
	if len(f.Own) > 0 {
		line.State = SendBackWaiting
		for _, o := range f.Own {
			if protocol.ChangeoverStartActionFor(o.Status) == protocol.ChangeoverStartCancel {
				line.CanCancel = true
			}
			if o.OrderType == protocol.OrderTypeRetrieveTopsAndBottoms {
				line.QueueReason = o.QueueReason
			}
		}
		return line
	}
	cart := f.Outside
	if cart == nil || !cart.Occupied || cart.TotesPerLayer == nil || cart.Bare ||
		cart.CarrierState() == protocol.CarrierTopsAndBottoms {
		return SendBack{}
	}
	if f.Core != nil && f.Core.Occupied && f.Core.TotesPerLayer == nil {
		return SendBack{}
	}
	if f.ChangeoverArmed {
		return SendBack{}
	}
	line.State = SendBackEnabled
	if f.PlacingAtOutside || f.SwapLive {
		line.State = SendBackDisabled
	}
	return line
}

// SendBackOrders is SEND BACK's live orders among rows, the orders of the
// outside's own process node the caller read: the move whose source is the
// outside (the cart sent back, by name) and the tops & bottoms pull whose
// delivery is the outside, nothing else. A changeover's per-position legs
// share that process node and are complex orders and plain retrieves, so
// neither shape is one of them.
func SendBackOrders(rows []Order, outside string) []Order {
	var out []Order
	for _, o := range rows {
		if protocol.IsTerminal(o.Status) || outside == "" {
			continue
		}
		if (o.OrderType == protocol.OrderTypeMove && o.SourceNode == outside) ||
			(o.OrderType == protocol.OrderTypeRetrieveTopsAndBottoms && o.DeliveryNode == outside) {
			out = append(out, o)
		}
	}
	return out
}

// OrderFacts sets the facts the line reads from orders, from rows the caller
// already holds: the press's live orders, the outside's (its own process
// node's), and the press's runtime swap slots. Own is SendBackOrders over the
// outside's rows; PlacingAtOutside is any other of either set placing a
// carrier at the outside (PlacesAt); SwapLive is a slot's order among the
// press's rows that has not left the cell.
func (f *SendBackFacts) OrderFacts(pressRows, outsideRows []Order, swapSlots ...*int64) {
	f.Own = SendBackOrders(outsideRows, f.Paired)
	own := map[int64]bool{}
	for _, o := range f.Own {
		own[o.ID] = true
	}
	var others []Order
	for _, rows := range [][]Order{pressRows, outsideRows} {
		for _, o := range rows {
			if !own[o.ID] {
				own[o.ID] = true
				others = append(others, o)
			}
		}
	}
	f.PlacingAtOutside = PlacesAt(others, f.Paired)
	for _, slot := range swapSlots {
		if slot == nil {
			continue
		}
		for _, o := range pressRows {
			if o.ID == *slot && !protocol.IsTerminal(o.Status) && !o.Departed {
				f.SwapLive = true
			}
		}
	}
}

// PlacesAt reports whether a live order among rows leaves a carrier at node,
// read as the release reads a leg (store PlacingAt, in Go over rows in hand): a
// plain order at its delivery node, a leg at the nodes its release facts place
// at, and a leg whose facts are missing or unreadable as placing (it must not
// let a button act while a carrier may be coming).
func PlacesAt(rows []Order, node string) bool {
	for _, o := range rows {
		if protocol.IsTerminal(o.Status) || node == "" {
			continue
		}
		if o.OrderType != protocol.OrderTypeComplex {
			if o.DeliveryNode == node {
				return true
			}
			continue
		}
		f, err := release.DecodeFacts(o.ReleaseFacts)
		if o.ReleaseFacts == "" || err != nil || f.PlacesBinAt(node) {
			return true
		}
	}
	return false
}
