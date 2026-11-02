package dispatch

import (
	"shingocore/dispatch/binresolver"
	"shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// window_push.go — dispatch's face on the one chooser (plan.Planner.ChoosePush): which
// orders are asked of it, and the cart it is asked with.

// WindowPushShaped reports whether an order could be a push of a kanban's loop by what it says
// of itself, with no read: a station's move for one empty cart, named from its source. The
// chooser's one statement then asks whether that source is of the group's loop; a shaped order
// that is not comes back to the cap's path.
//
// A PERSON'S NAMED DROP IS NOT SHAPED, and stays named. Core's own bin move (a person
// at a page, station-unaware) names its drop as a person chose it, and a station's
// move naming no bin (an operator's typed move) names a drop and no cart. A slot named
// directly is no group, and no door asks. Pin:
// TestKanbanDoor_APersonsNamedDropStaysNamed.
//
// IT DOES NOT ASK aPersonNamedTheDrop. An unloader's empty-out is no-demand too (the
// system's consequence of a clear), so the origin class cannot tell them apart.
func WindowPushShaped(order *orders.Order) bool {
	return order != nil && order.ParentOrderID == nil && !order.Coordinated && !order.StationUnaware &&
		order.RecoversOrderID == nil && order.SourceIntent == SourceIntentLocal &&
		order.NamedBinID != nil && order.PayloadCode == "" && order.SourceNode != ""
}

// carrierOf is the cart an order on the plain path is putting down into group, read
// once: its one cart off the order in hand, else the cart it names (the first two of
// store.DB.BinTypeForOrder's sources, with no second read of the row), else what the
// order holds. An order with several carts reads as unknown here and asks the store,
// which answers by the row setting down into group. Unknown says why, and the
// resolver logs it.
func (d *Dispatcher) carrierOf(order *orders.Order, group string) binresolver.BinTypeStatement {
	only, _ := order.OnlyBin()
	for _, id := range []*int64{only, order.NamedBinID} {
		if id == nil {
			continue
		}
		if bin, err := d.db.GetBin(*id); err == nil && bin != nil {
			return binresolver.KnownCarrier(bin)
		}
	}
	unknown := binresolver.UnknownBinType("scanner deferral keeper: order holds no bin and no hold")
	if order.ID == 0 {
		return unknown
	}
	id, state, err := d.db.BinTypeForOrder(order.ID, group)
	if err != nil || id == nil {
		return unknown
	}
	return binresolver.KnownTypeInState(*id, state)
}

// ChooseWindowPush asks the chooser of one acquiring push, its cart read here, and
// returns the cart so the caller's gate reads it no more. refused is the pass's memo.
func (d *Dispatcher) ChooseWindowPush(order *orders.Order, group *nodes.Node, refused plan.PushRefusals) (plan.PushChoice, binresolver.BinTypeStatement) {
	stated := d.carrierOf(order, group.Name)
	return d.pure.ChoosePush(plan.PushAsk{Group: group, Source: order.SourceNode, Stated: stated,
		Asker: digAskerFor(order), Refused: refused}), stated
}

// ChooseCancelledPush asks the chooser where a cancelled push's cart goes back to. source
// is where the cart was lifted: its own row's pick node, not the order's source_node, which
// names one of a multi-cart order's lifts at most. The cancelled order is terminal, so no
// asker is exempt from anything.
func (d *Dispatcher) ChooseCancelledPush(source string, group *nodes.Node, stated binresolver.BinTypeStatement) plan.PushChoice {
	return d.pure.ChoosePush(plan.PushAsk{Group: group, Source: source, Stated: stated, Asker: domain.Anyone})
}

// PushParkBlock is the park of a push no slot of its door takes, named at group, in the sentence
// a full group's park says. A loop cart never parks at a level.
func PushParkBlock(group string, c plan.PushChoice) CapacityBlock {
	return CapacityBlock{Cause: CauseNGRPFull, Params: QueueParams{Destination: group, Family: c.Family}}
}
