package engine

import (
	"fmt"

	"shingo/protocol"
	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
)

// APIRetrieveRequest is what the HTTP order API asks for. It is the handler's
// request shape lifted into the engine so the routing decision below — Core's
// loader ask or a direct order — lives in one place rather than in a web
// handler.
type APIRetrieveRequest struct {
	ProcessNodeID *int64
	RetrieveEmpty bool
	Quantity      int64
	DeliveryNode  string
	SourceNode    string
	StagingNode   string
	PayloadCode   string
	AutoConfirm   bool
	// Count > 1 asks for a batch of orders. At a loader window Core decides how
	// many it will make; see below.
	Count int
}

// LoaderRetrieveRefused is Core's answer that it made none of a manual
// retrieve at a loader window: every window is spoken for, or why not. It is
// a conflict with the state of the plant, not a malformed request, and the
// HTTP layer maps it to 409 with the sentence.
type LoaderRetrieveRefused struct{ Sentence string }

func (r *LoaderRetrieveRefused) Error() string { return r.Sentence }

// CreateRetrieveForAPI is the HTTP order API's creation path (the manual
// order page).
//
// AT A LOADER'S WINDOW IT ASKS CORE (the loader retrieve ask, a person's
// origin): Core counts what is in flight for that loader plant-wide, what
// stands on its windows and which are spoken for, under the loader's lock,
// and makes the orders or says why not. So a manual retrieve to a window
// with a carrier on it, or with one already coming, makes nothing and the
// caller reads Core's sentence (owner's ruling 1, 2026-10-20: plant
// processes move straight to loader windows through node claims, and a
// person may want the manual page to as well). Resolution is by DESTINATION
// NODE rather than by anything the caller says about itself: LoaderAt asks
// the aggregate whether some loader or unloader owns that node, in either
// role.
//
// The request's overrides ride as Core reads them: the source it names, and
// a source left blank has Core search the plant (AnySource), as an order
// request naming none does. Core reads no staging node or auto-confirm on
// any order: both are written on this Edge's rows for the orders Core made,
// where the orders page shows the one and delivery acts on the other. A
// loader window takes one carrier per order, so the quantity is not carried.
//
// THE BATCH ARM IS CAPPED BY CORE. Asking for five empties at a loader window
// with room for one makes one, and the answer says how many were made. A
// batch to a non-loader destination behaves exactly as before.
//
// If no loader owns the node — a press position, a supermarket slot, a
// quality-hold spot — nothing changes: those have no window budget to belong
// to (createRetrieveDirect).
func (e *Engine) CreateRetrieveForAPI(req APIRetrieveRequest) ([]*orders.Order, error) {
	count := req.Count
	if count < 1 {
		count = 1
	}

	loader := e.loaderOwning(req.DeliveryNode)
	if loader == nil {
		return e.createRetrieveDirect(req, count)
	}

	direction, carrier := protocol.LoaderRetrieveFull, "full"
	if req.RetrieveEmpty {
		direction, carrier = protocol.LoaderRetrieveEmpty, "empty"
	}
	res, err := askOrRefuse[protocol.LoaderRetrieveResponse](e, protocol.AskLoaderRetrieve, protocol.LoaderRetrieveRequest{
		LoaderKey: string(loader.ID()), Direction: direction, Origin: protocol.LoaderAskOperator,
		PayloadCode: req.PayloadCode, MemberNode: req.DeliveryNode, Count: count,
		// NoDemand: a direct API command belongs to no cell episode and never
		// will. Not "nobody wanted it" — the caller did — but the demand grain
		// measures EPISODES, and this order is structurally outside them.
		OriginClass: ordermgr.NoDemand().Class,
		SourceNode:  req.SourceNode, AnySource: req.SourceNode == "",
	})
	if err != nil {
		return nil, fmt.Errorf("order api: retrieve at %s: %w", req.DeliveryNode, err)
	}
	if len(res.Created) == 0 {
		return nil, &LoaderRetrieveRefused{Sentence: noneOrderedSentence(req.DeliveryNode, carrier, res)}
	}
	made := make([]*orders.Order, 0, len(res.Created))
	for _, uuid := range res.Created {
		made = append(made, e.adoptAPIOrder(uuid, req))
	}
	return made, nil
}

// adoptAPIOrder is the Edge's own part of an order Core made for the manual
// retrieve: the asking node, the staging node and auto-confirm on its row
// (one write, none when there is nothing to record). Made on Core and not yet
// projected here, it is the order Core named, and the projection brings the
// row without them.
func (e *Engine) adoptAPIOrder(uuid string, req APIRetrieveRequest) *orders.Order {
	order, err := e.db.GetOrderByUUID(uuid)
	if err != nil || order == nil {
		e.logFn("order api: order %s made on Core, not yet projected here: %v", uuid, err)
		return &orders.Order{UUID: uuid, ProcessNodeID: req.ProcessNodeID, RetrieveEmpty: req.RetrieveEmpty,
			StagingNode: req.StagingNode, AutoConfirm: req.AutoConfirm}
	}
	if req.ProcessNodeID == nil && req.StagingNode == "" && !req.AutoConfirm {
		return order
	}
	if err := e.db.AdoptAskedOrder(order.ID, req.ProcessNodeID, req.StagingNode, req.AutoConfirm); err != nil {
		e.logFn("order api: record the manual retrieve's fields on %s: %v", uuid, err)
		return order
	}
	if req.ProcessNodeID != nil {
		order.ProcessNodeID = req.ProcessNodeID
	}
	order.StagingNode, order.AutoConfirm = req.StagingNode, req.AutoConfirm
	return order
}

// loaderOwning resolves the loader or unloader that owns a delivery node, in
// either role, or nil when none does. Produce is asked first only because it is
// the commoner case; a node cannot belong to both roles.
func (e *Engine) loaderOwning(deliveryNode string) *domain.Loader {
	if deliveryNode == "" {
		return nil
	}
	for _, role := range []domain.LoaderRole{domain.RoleProduce, domain.RoleConsume} {
		if l, err := e.loaders().LoaderAt(domain.NodeID(deliveryNode), role); err == nil && l != nil {
			return l
		}
	}
	return nil
}

// createRetrieveDirect is the unchanged path for a destination no loader owns.
func (e *Engine) createRetrieveDirect(req APIRetrieveRequest, count int) ([]*orders.Order, error) {
	made := make([]*orders.Order, 0, count)
	for i := 0; i < count; i++ {
		order, err := e.orderMgr.CreateRetrieveOrder(ordermgr.RetrieveSpec{Process: req.ProcessNodeID,
			Delivery: req.DeliveryNode, Source: req.SourceNode, Staging: req.StagingNode, Payload: req.PayloadCode,
			Quantity: req.Quantity, Empty: req.RetrieveEmpty, AutoConfirm: req.AutoConfirm,
			// NoDemand: a direct API command belongs to no cell episode (see
			// CreateRetrieveForAPI).
			Origin: ordermgr.NoDemand()})
		if err != nil {
			// A partial batch is reported as what it is: the orders that exist
			// are real and already enqueued, so swallowing them would lose work
			// the caller is entitled to know about.
			if len(made) > 0 {
				return made, fmt.Errorf("created %d of %d before failing: %w", len(made), count, err)
			}
			return nil, err
		}
		made = append(made, order)
	}
	return made, nil
}
