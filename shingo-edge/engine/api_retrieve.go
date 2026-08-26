package engine

import (
	"fmt"

	"shingoedge/domain"
	ordermgr "shingoedge/orders"
	"shingoedge/store/orders"
)

// APIRetrieveRequest is what the HTTP order API asks for. It is the handler's
// request shape lifted into the engine so the routing decision below — seam or
// direct — lives with the seam rather than in a web handler.
type APIRetrieveRequest struct {
	ProcessNodeID *int64
	RetrieveEmpty bool
	Quantity      int64
	DeliveryNode  string
	SourceNode    string
	StagingNode   string
	LoadType      string
	PayloadCode   string
	AutoConfirm   bool
	// Count > 1 asks for a batch of empty-bin orders. The seam decides how many
	// it will actually allow; see below.
	Count int
}

// CreateRetrieveForAPI is the HTTP order API's creation path.
//
// The API used to call CreateRetrieveOrder directly, which is the ONE creation
// route that never went through the reservation seam. Every other way to ask for
// a bin at a loader window — the operator's Request Empty and Request Full
// buttons, the threshold signal, the push sweeps, the unloader's U1 — counts
// what is already in flight for that loader and refuses to exceed it. This one
// counted nothing, so an HTTP caller could put a second empty on a window that
// already had one, and the never-2N invariant simply did not cover the path.
//
// It does now, when the destination is a loader's. Resolution is by DESTINATION
// NODE rather than by anything the caller says about itself: LoaderAt asks the
// aggregate whether some loader or unloader owns that node, in either role. If
// one does, the order goes through withLoaderBudget on that loader's key, so
// this door contends on the same mutex as every other door onto the same
// windows. If no loader owns the node — a press position, a supermarket slot, a
// quality-hold spot — nothing changes: those have no budget to belong to, and
// inventing one for them is what the RequestEmptyBin simple-mode guard comment
// already explains is wrong.
//
// THE BATCH ARM INHERITS THE BUDGET, which is the point of routing it here
// rather than beside the single arm. Asking for five empties at a loader window
// with room for one used to create five; now the seam caps it, and the answer
// says how many were actually made. A batch to a non-loader destination behaves
// exactly as before.
func (e *Engine) CreateRetrieveForAPI(req APIRetrieveRequest) ([]*orders.Order, error) {
	count := req.Count
	if count < 1 {
		count = 1
	}

	loader := e.loaderOwning(req.DeliveryNode)
	if loader == nil {
		return e.createRetrieveDirect(req, count)
	}

	var made []*orders.Order
	created, err := e.withLoaderBudget(loader, domain.PayloadCode(req.PayloadCode), count,
		domain.NodeID(req.DeliveryNode), req.RetrieveEmpty,
		func(deliveryNodes []string) (int, error) {
			n := 0
			for _, deliveryNode := range deliveryNodes {
				// deliveryNode comes from the seam's free-window assignment, not
				// from the request: a shared loader spreads across its windows,
				// and the whole reason to be here is to land on one that is free.
				order, cerr := e.orderMgr.CreateRetrieveOrder(
					req.ProcessNodeID, req.RetrieveEmpty, req.Quantity,
					deliveryNode, req.SourceNode, req.StagingNode, req.LoadType,
					req.PayloadCode, req.AutoConfirm, false,
					// NoDemand: a direct API command belongs to no cell episode and
					// never will. Not "nobody wanted it" — the caller did — but the
					// demand grain measures EPISODES, and this order is structurally
					// outside them. Leaving it unstated put it in the orphan bucket
					// beside the genuinely lost origins.
					ordermgr.NoDemand(),
				)
				if cerr != nil {
					return n, cerr
				}
				made = append(made, order)
				n++
			}
			return n, nil
		})
	if err != nil {
		return nil, fmt.Errorf("order api: retrieve at %s: %w", req.DeliveryNode, err)
	}
	if created == 0 {
		// Not an error the caller got wrong — the windows are already covered.
		// The handler turns this into a 409, the same answer the operator's
		// buttons give for the same situation.
		return nil, ErrLoaderBudgetExhausted
	}
	return made, nil
}

// ErrLoaderBudgetExhausted means the destination's loader already has as much
// inbound as it has room for. It is a conflict with the state of the plant, not
// a malformed request, and the HTTP layer maps it to 409.
var ErrLoaderBudgetExhausted = fmt.Errorf("a bin is already inbound for every window at that destination")

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
		order, err := e.orderMgr.CreateRetrieveOrder(
			req.ProcessNodeID, req.RetrieveEmpty, req.Quantity,
			req.DeliveryNode, req.SourceNode, req.StagingNode, req.LoadType,
			req.PayloadCode, req.AutoConfirm, false,
			ordermgr.NoDemand(), // see createRetrieveForLoader
		)
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
