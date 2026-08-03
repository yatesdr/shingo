package engine

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// ErrDestinationOccupied is returned when a move is refused because something
// is already at the destination.
//
// It is a sentinel rather than a plain error because the caller has to tell it
// apart: this is a conflict with the plant's current state, which the operator
// can resolve by clearing the spot or picking another one, and it deserves a
// 409 rather than the 500 every other failure on this path gets. Wrapped so the
// rendered sentence travels with it.
var ErrDestinationOccupied = errors.New("destination occupied")

// ErrBinTaken is returned when another order acquired the bin in the moment
// between choosing it and reserving it.
//
// A sentinel for the same reason as the one above: the caller has to tell it
// apart from a real fault. This one used to be tagged in the error TEXT — the
// words "transient reservation conflict, retry" were spliced into the message —
// which the comment beside it described as tagging it "so the caller can retry
// rather than surface a hard 500". Nothing could read a phrase in a string, so
// the caller never did, and it surfaced as a hard 500 with an instruction buried
// inside it. Now it is a value.
var ErrBinTaken = errors.New("bin taken by another order")

// DirectOrderRequest holds the parameters for creating a direct fleet order.
type DirectOrderRequest struct {
	FromNodeID int64
	ToNodeID   int64
	StationID  string
	Priority   int
	Desc       string
}

// DirectOrderResult holds the result of a successfully created direct order.
type DirectOrderResult struct {
	OrderID       int64
	VendorOrderID string
	FromNode      string
	ToNode        string
}

// CreateDirectOrder creates a transport order in the DB and dispatches it to the fleet.
func (e *Engine) CreateDirectOrder(req DirectOrderRequest) (*DirectOrderResult, error) {
	if req.FromNodeID == req.ToNodeID {
		return nil, fmt.Errorf("source and destination must be different")
	}

	sourceNode, err := e.db.GetNode(req.FromNodeID)
	if err != nil {
		return nil, fmt.Errorf("source node not found")
	}
	destNode, err := e.db.GetNode(req.ToNodeID)
	if err != nil {
		return nil, fmt.Errorf("destination node not found")
	}

	// The same occupancy gate the operator's bin-move takes. This door is only
	// reachable from the /test-orders page, which is an argument for exempting
	// it and the owner ruled against: the page is used occasionally and its
	// orders move real bins with real robots. An exemption here would also be
	// the kind that outlives the reason for it.
	//
	// Before the order row and before the reservation, so a refusal leaves the
	// source bin untouched.
	if preview := e.dispatcher.PreviewDropoffCapacity(destNode.Name); preview.Blocked {
		return nil, fmt.Errorf("%w: %s", ErrDestinationOccupied, preview.Reason)
	}

	// Pick an unclaimed bin at the source node so the order carries a
	// concrete BinID. Without it, applyBinArrivalForOrder silently skips on
	// completion and bins.node_id never reflects the move (CARRIER-0005
	// stuck-at-source bug).
	srcBins, err := e.db.ListBinsByNode(req.FromNodeID)
	if err != nil {
		return nil, fmt.Errorf("list bins at source: %w", err)
	}
	var srcBinID int64
	for _, b := range srcBins {
		// Reservation-aware (1b): skip bins another order has reserved but not yet
		// claimed, so the ReserveForDispatch soft-acquire below doesn't lose the
		// race. The hard claim lands later, at ConfirmForDispatch.
		if b.ClaimedBy == nil && !b.HasPendingReservation {
			srcBinID = b.ID
			break
		}
	}
	if srcBinID == 0 {
		return nil, fmt.Errorf("no unclaimed bin at source node %s", sourceNode.Name)
	}

	edgeUUID := req.StationID + "-" + uuid.New().String()[:8]

	order := &orders.Order{
		EdgeUUID:  edgeUUID,
		StationID: req.StationID,
		OrderType: protocol.OrderTypeMove,
		Status:    protocol.StatusPending,
		// One robot, one bin. The field was omitted here, and because the
		// INSERT names the column the table's DEFAULT 1 never applied — so
		// every direct move ever made through this door stored 0 and the order
		// screen printed "qty 0".
		Quantity:     1,
		SourceNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     req.Priority,
		PayloadDesc:  req.Desc,
		BinID:        &srcBinID,
		// NO_DEMAND, stamped here where it is known. A direct order is an
		// engineer moving a bin from A to B; no place asked for material, so
		// there is no episode and its absence is not a finding. Left blank it
		// would land orphan and put an admin action in the one bucket that is
		// supposed to mean "we lost a demand link."
		OriginClass: protocol.OriginClassNoDemand,
	}
	if err := e.db.CreateOrder(order); err != nil {
		return nil, fmt.Errorf("create order: %w", err)
	}
	// Rule 1: soft-acquire the bin (a pending reservation), then hard-claim it at
	// dispatch. Another order can take the bin in the gap between the scan above
	// and this call — that is a race with a person, not a fault, and it gets a
	// sentinel the caller can act on.
	//
	// The order row already exists at this point, so it has to be failed here or
	// it sits pending forever with nothing to dispatch, fail or clean it up.
	if err := e.binManifest.ReserveForDispatch(srcBinID, order.ID); err != nil {
		if ferr := e.db.FailOrderAtomic(order.ID, "bin taken by another order before reservation"); ferr != nil {
			e.logFn("engine: fail direct order %d after losing the bin: %v", order.ID, ferr)
		}
		if errors.Is(err, reservations.ErrReservationConflict) {
			return nil, fmt.Errorf("%w: bin %d", ErrBinTaken, srcBinID)
		}
		return nil, fmt.Errorf("reserve bin %d: %w", srcBinID, err)
	}
	if err := e.dispatcher.Lifecycle().MarkPending(order, req.Desc); err != nil {
		e.logFn("engine: mark direct order %d pending: %v", order.ID, err)
	}

	// Confirm-at-dispatch: hard-claim the destination slot (if a storage dropoff)
	// and the bin in one step, immediately before the fleet call.
	if err := e.dispatcher.ConfirmForDispatch(order, srcBinID, sourceNode, destNode); err != nil {
		if rerr := e.db.ReleaseReservation(order.ID, srcBinID); rerr != nil {
			e.logFn("engine: release reservation for bin %d after confirm failure: %v", srcBinID, rerr)
		}
		return nil, fmt.Errorf("confirm bin %d at dispatch: %w", srcBinID, err)
	}

	vendorOrderID, err := e.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		// Coupled rollback: clear the hard claim AND release the reservation, so a
		// failed dispatch can't orphan a confirmed reservation. (DispatchDirect
		// already Fail'd the order, which released it — this is the idempotent belt.)
		if uerr := e.db.ReleaseClaimForBin(srcBinID, order.ID); uerr != nil {
			e.logFn("engine: release claim for bin %d after dispatch failure: %v", srcBinID, uerr)
		}
		return nil, fmt.Errorf("fleet dispatch failed: %w", err)
	}

	return &DirectOrderResult{
		OrderID:       order.ID,
		VendorOrderID: vendorOrderID,
		FromNode:      sourceNode.Name,
		ToNode:        destNode.Name,
	}, nil
}

// TerminateOrder cancels an order, unclaims any payloads, and emits a cancellation event.
func (e *Engine) TerminateOrder(orderID int64, actor string) error {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		return fmt.Errorf("order not found")
	}

	// Reject terminal AND post-delivery statuses. Once the bin is at the
	// destination (Delivered/Confirmed) or terminal, terminate is a no-op.
	if dispatch.IsPostDelivery(order.Status) || protocol.IsTerminal(order.Status) {
		return fmt.Errorf("cannot terminate order in status %q", order.Status)
	}

	// Route through lifecycle.CancelOrder for atomic transition + emit.
	// CancelOrder also cancels the vendor leg if active (no need to call
	// e.fleet.CancelOrder separately).
	e.dispatcher.Lifecycle().CancelOrder(order, order.StationID, "cancelled by "+actor)
	return nil
}

// failOrderAndEmit fails an order in the DB AND emits EventOrderFailed so the
// standard handler chain (audit, return order, edge notification) fires.
//
// Use this from any caller that previously did a bare db.FailOrderAtomic and
// would otherwise leave the order silently failed in the DB. The fulfillment
// scanner's structural-error path uses this to ensure scanner-driven failures
// reach Edge via the same notification pipeline as fleet-driven failures.
//
// Looks up StationID and EdgeUUID from the order so the EventOrderFailed
// payload is complete — without these fields populated, the wiring.go
// handler's notification gate skips the edge push.
func (e *Engine) failOrderAndEmit(orderID int64, errorCode, detail string) {
	order, err := e.db.GetOrder(orderID)
	if err != nil {
		e.logFn("engine: load order %d for fail: %v", orderID, err)
		return
	}
	// Route through lifecycle.Fail for atomic transition + emit.
	if err := e.dispatcher.Lifecycle().Fail(order, order.StationID, errorCode, detail); err != nil {
		e.logFn("engine: fail order %d (%s): %v", orderID, errorCode, err)
	}
}
