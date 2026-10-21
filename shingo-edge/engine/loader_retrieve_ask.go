package engine

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/orders"
	storeorders "shingoedge/store/orders"
	"shingoedge/store/processes"
)

// loader_retrieve_ask.go — carriers at a loader or an unloader, asked of Core.
//
// Core sizes and places them (protocol.AskLoaderRetrieve: dispatch.ReplenishLoader,
// the windows chosen by plan.LoaderWindows under the loader's lock, each order
// born through AdmitCoreAsk), so the count of what is in flight, the windows
// already spoken for and the resident bins are read where they are written,
// plant-wide. The orders reach this Edge as projections (ApplyOrderProjection),
// as every Core-authored order does. Nothing here creates an order.
//
// The askers: a person's empties at a loader (askLoaderEmpties: REQUEST EMPTY
// and the window-free push), a person's full at a consume window
// (RequestFullBin), the manual retrieve at a loader window
// (CreateRetrieveForAPI), and a release's full-in at an unloader
// (offerUnloaderFulls, which does not wait).

// askLoaderEmpties asks Core for count empties at dl for payload, routed to
// member on a dedicated loader, attributed to origin, waiting AskWait. The
// answer is what Core made and, per window it did not order to, why. A Core
// that does not answer the kind is refused here: there is no Edge path left
// that orders an operator's empty on its own count.
//
// A loader with no inbound source has Core search the plant for the empty
// (AnySource), as REQUEST EMPTY's own order with no source group did (Core
// planned it from any compatible empty); the window-free push never asks for
// one (stageOperatorEmpty).
func (e *Engine) askLoaderEmpties(dl *domain.Loader, payload domain.PayloadCode, count int, member domain.NodeID, origin orders.Origin) (*protocol.LoaderRetrieveResponse, error) {
	return askOrRefuse[protocol.LoaderRetrieveResponse](e, protocol.AskLoaderRetrieve, protocol.LoaderRetrieveRequest{
		LoaderKey: string(dl.ID()), Direction: protocol.LoaderRetrieveEmpty, Origin: protocol.LoaderAskOperator,
		PayloadCode: string(payload), MemberNode: string(member),
		Count: count, OriginID: origin.ID, OriginClass: origin.Class,
		AnySource: dl.InboundSource() == "",
	})
}

// noEmptiesSentence is the operator's answer when Core made no empty.
func noEmptiesSentence(node string, res *protocol.LoaderRetrieveResponse) string {
	return noneOrderedSentence(node, "empty", res)
}

// noneOrderedSentence is a person's answer when Core made none of what they
// asked for (an empty or a full): why, from Core's Skipped, or each window
// Core did not order to with its cause (heldWhy).
func noneOrderedSentence(node, carrier string, res *protocol.LoaderRetrieveResponse) string {
	// OWNER-WORDS
	return fmt.Sprintf("node %s: no %s ordered — %s", node, carrier, heldWhy(res))
}

// trackAskedOrder makes the order Core made for a tap at nodeID the node's:
// tracked at the node and its runtime's active order. The projection guesses
// the window it delivers to, and on a shared loader that can be a sibling's;
// the order is the node that asked, as an Edge-made one was, and a later
// projection keeps a stored node (UpsertProjection). Made on Core and not yet
// projected here: the board shows it when the projection lands, and the tap's
// answer is the order Core named.
func (e *Engine) trackAskedOrder(nodeID int64, node *processes.Node, uuid string, retrieveEmpty bool) *storeorders.Order {
	order, gerr := e.db.GetOrderByUUID(uuid)
	if gerr != nil || order == nil {
		log.Printf("bin_ops: node %s: order %s made on Core, not yet projected: %v", node.Name, uuid, gerr)
		return &storeorders.Order{UUID: uuid, ProcessNodeID: &nodeID, RetrieveEmpty: retrieveEmpty}
	}
	if order.ProcessNodeID == nil || *order.ProcessNodeID != nodeID {
		if uerr := e.db.UpdateOrderProcessNode(order.ID, &nodeID); uerr != nil {
			log.Printf("bin_ops: track order %s at node %d: %v", order.UUID, nodeID, uerr)
		} else {
			order.ProcessNodeID = &nodeID
		}
	}
	if uerr := e.db.SetProcessNodeRuntimeActiveOrder(nodeID, &order.ID); uerr != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", nodeID, uerr)
	}
	return order
}
