package engine

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// BinSelection says who picked the bin.
//
// It is the one thing the two bin-move surfaces genuinely disagreed about.
// Everything else they differed on — the quantity, the reference number, what a
// mistyped node means, what losing a race means, whether a full destination is
// refused — was a difference nobody chose, and each got reconciled on its own.
// This one is real: an engineer says "take something off that node", an
// operator says "take THAT bin".
type BinSelection string

const (
	// BinSelectionAuto: any free bin at the named source node will do.
	BinSelectionAuto BinSelection = "auto"
	// BinSelectionByLabel: this bin, wherever it currently is. Its node is the
	// source, which is why a by-label request names no source of its own.
	BinSelectionByLabel BinSelection = "by_label"
)

// BinMoveRequest asks for one bin to be carried from where it is to somewhere
// else.
type BinMoveRequest struct {
	Selection BinSelection

	// SourceNodeID is where to take a bin from. Auto only.
	SourceNodeID int64
	// BinLabel names the bin to take. By-label only.
	BinLabel string

	// The destination, named either way — the two surfaces post different
	// things and there is no reason to make one of them translate. The
	// engineers' page picks from a list of node ids; the operator's screen
	// sends a node name. Exactly one of these is set.
	DestNodeID   int64
	DestNodeName string

	StationID string
	// EdgeUUID is the order's reference number. Blank mints one.
	EdgeUUID string
	Priority int
	Desc     string
}

// BinMoveResult describes the order that is now on its way.
type BinMoveResult struct {
	OrderID       int64
	VendorOrderID string
	FromNode      string
	ToNode        string
	BinLabel      string
}

// BinMoveErrKind classifies a refusal, so a caller can answer with the right
// status without reading the sentence.
type BinMoveErrKind int

const (
	// BinMoveBadRequest: the caller asked for something that does not exist or
	// makes no sense. Fixable by asking differently.
	BinMoveBadRequest BinMoveErrKind = iota
	// BinMoveConflict: the request is fine, the plant is busy. Fixable by
	// clearing a spot, picking another bin, or trying again.
	BinMoveConflict
	// BinMoveFault: not the caller's fault.
	BinMoveFault
)

// BinMoveError is a refusal a person can read.
//
// The sentence and the classification travel together because they used to
// travel apart. Each door wrote its own wording for the same refusals, and they
// drifted: losing the bin race told the operator which bin they had lost and
// told the engineer only that "that bin" was taken, on a page where several
// could be in play. Worse, the sentinel-wrapping idiom leaked its own name into
// the message, so a full destination read as "destination occupied: Waiting for
// a slot at LINE_01 — 1 bin there now".
//
// Building both here means one vocabulary for both surfaces, and the HTTP layer
// is left with the only job it should have: turning a kind into a number.
type BinMoveError struct {
	Kind     BinMoveErrKind
	Sentence string
	cause    error
}

func (e *BinMoveError) Error() string { return e.Sentence }

// Unwrap keeps errors.Is working against the sentinels below, so callers that
// already match on one keep matching.
func (e *BinMoveError) Unwrap() error { return e.cause }

func refuse(kind BinMoveErrKind, cause error, format string, a ...any) *BinMoveError {
	return &BinMoveError{Kind: kind, Sentence: fmt.Sprintf(format, a...), cause: cause}
}

// CreateBinMove moves one bin from where it is to somewhere else, and is the
// only way to do that on Core.
//
// There were two of these, one per screen, and they had drifted twelve ways.
// Most of those were not decisions — they were two people solving the same
// problem a year apart, and the gap is where the bugs lived: one door stored a
// quantity of zero, one let a bin be sent to where it already was, one reported
// a lost race as a server fault, one never wrote the order's creation history.
// Each was fixed on its own door as it was found, which is exactly the work
// that having two of these makes necessary forever.
func (e *Engine) CreateBinMove(req BinMoveRequest) (*BinMoveResult, error) {
	// Destination first, then source, then a bin — and the order is not
	// arbitrary. A request that names a node which does not exist, or names the
	// same node twice, is wrong however busy the plant is; answering it with
	// "no free bin at STORAGE-A1" would be true and useless. Whether a bin is
	// available is a fact about the plant, and it is asked last.
	destNode, rerr := e.resolveMoveDest(req)
	if rerr != nil {
		return nil, rerr
	}

	bin, sourceNode, rerr := e.resolveMoveSource(req)
	if rerr != nil {
		return nil, rerr
	}

	// Asking to move a bin to where it already is.
	//
	// The specific answer has to win over the generic one: the occupancy gate
	// below catches most of these incidentally — the bin is at the destination,
	// so the destination reads as occupied — and then tells the operator to go
	// clear a node whose only occupant is the bin they are moving. It also does
	// not catch all of them, because it defers on lane nodes, so a bin sitting
	// in a lane used to reach the fleet with nothing stopping it.
	//
	// Scoped to this door. It must never move into the shared writer or the
	// dispatch tail: a complex order's first and last step are legitimately the
	// same node — a robot lifts a bin off a position, takes it away and brings a
	// different one back — so a check placed there would break changeovers.
	if sourceNode.ID == destNode.ID {
		// Named the bin, so name it back: "already at" is the sentence that
		// tells an operator nothing needs doing. With no bin named yet there is
		// nothing to point at, and the request itself is the thing that is
		// wrong.
		if bin != nil {
			return nil, refuse(BinMoveBadRequest, nil,
				"bin %s is already at %s", bin.Label, destNode.Name)
		}
		return nil, refuse(BinMoveBadRequest, nil,
			"source and destination must be different (both are %s)", destNode.Name)
	}

	// Before the order row and before any claim, so a refusal leaves the source
	// bin exactly as it was. Rejecting later would strand a pending order and
	// leave a reservation the next move would be told to wait behind.
	if preview := e.dispatcher.PreviewDropoffCapacity(destNode.Name); preview.Blocked {
		return nil, refuse(BinMoveConflict, ErrDestinationOccupied, "%s", preview.Reason)
	}

	// The request is sound; now ask the plant. An auto request has named a node
	// rather than a bin, so this is where one gets chosen.
	if bin == nil {
		var rerr error
		if bin, rerr = e.pickFreeBinAt(sourceNode); rerr != nil {
			return nil, rerr
		}
	}

	edgeUUID := req.EdgeUUID
	if edgeUUID == "" {
		// A bare identifier, like the Edge mints. Full length, not the first
		// eight characters: eight hex characters is about four billion values,
		// which sounds like plenty until you notice nothing anywhere handles a
		// collision.
		edgeUUID = uuid.New().String()
	}

	order := &orders.Order{
		EdgeUUID:  edgeUUID,
		StationID: req.StationID,
		OrderType: protocol.OrderTypeMove,
		Status:    protocol.StatusPending,
		// One robot, one bin.
		Quantity:     1,
		SourceNode:   sourceNode.Name,
		DeliveryNode: destNode.Name,
		Priority:     req.Priority,
		PayloadDesc:  req.Desc,
		BinID:        &bin.ID,
		// NO_DEMAND, stamped where it is known. Somebody moving a bin from A to
		// B is not a place asking for material, so there is no episode and its
		// absence is not a finding. Left blank it would land orphan and put a
		// deliberate human action in the one bucket that is supposed to mean
		// "we lost a demand link".
		OriginClass: protocol.OriginClassNoDemand,
	}
	if err := e.db.CreateOrder(order); err != nil {
		return nil, refuse(BinMoveFault, err, "could not create the order: %v", err)
	}

	// BEFORE the reservation, and that is the deliberate half of this merge.
	//
	// The two doors did this at opposite points and neither said why. The status
	// column already says pending — the INSERT set it — so what this call is
	// really for is the HISTORY row, which transitions write and inserts do not.
	// Without it an order created directly at pending has no entry saying it
	// ever started, and its timeline begins at whatever happened next.
	//
	// Which makes the order matter. Reserve-then-stamp loses the creation entry
	// for exactly the orders that fail at the reservation — the ones that lost a
	// race with another person, whose history would then read as a failure with
	// no beginning. Those are the ones somebody is most likely to go and read.
	// So: stamp first, and every order has a start, including the ones that do
	// not get far.
	//
	// Logged rather than returned: the order is real and dispatchable either
	// way, and failing the request over a missing audit line is the worse trade.
	if err := e.dispatcher.Lifecycle().MarkPending(order, req.Desc); err != nil {
		e.logFn("engine: mark bin move %d pending: %v", order.ID, err)
	}

	// Soft-acquire the bin now, hard-claim it at dispatch. Another order can
	// take it in the gap between the check above and this call — that is a race
	// with a person, not a fault.
	if err := e.binManifest.ReserveForDispatch(bin.ID, order.ID); err != nil {
		// The order row exists by now, so a failure here has to fail it too or
		// it sits pending forever with nothing to dispatch, fail or clean it up.
		//
		// Through the lifecycle rather than a bare FailOrderAtomic: the direct
		// call transitions the row and stops there, leaving an order that failed
		// with no audit line and no notification, which is the state the
		// state-machine guard exists to prevent. failOrderAndEmit routes through
		// Lifecycle().Fail and fires EventOrderFailed, so this lands in the audit
		// trail and reaches the station like every other failure.
		e.failOrderAndEmit(order.ID, "bin_taken", "bin taken by another order before reservation")
		if errors.Is(err, reservations.ErrReservationConflict) {
			return nil, refuse(BinMoveConflict, ErrBinTaken,
				"bin %s was taken a moment ago — try again", bin.Label)
		}
		return nil, refuse(BinMoveFault, err, "could not reserve bin %s: %v", bin.Label, err)
	}

	// Confirm-at-dispatch: hard-claim the destination slot (if a storage
	// dropoff) and the bin in one step, immediately before the fleet call.
	if err := e.dispatcher.ConfirmForDispatch(order, bin.ID, sourceNode, destNode); err != nil {
		if rerr := e.db.ReleaseReservation(order.ID, bin.ID); rerr != nil {
			e.logFn("engine: release reservation for bin %d after confirm failure: %v", bin.ID, rerr)
		}
		return nil, refuse(BinMoveFault, err, "could not claim bin %s at dispatch: %v", bin.Label, err)
	}

	vendorOrderID, err := e.dispatcher.DispatchDirect(order, sourceNode, destNode)
	if err != nil {
		// Coupled rollback: clear the hard claim AND release the reservation, so
		// a failed dispatch cannot orphan a confirmed one. (DispatchDirect has
		// already failed the order, which released it — this is the idempotent
		// belt.)
		if uerr := e.db.ReleaseClaimForBin(bin.ID, order.ID); uerr != nil {
			e.logFn("engine: release claim for bin %d after dispatch failure: %v", bin.ID, uerr)
		}
		return nil, refuse(BinMoveFault, err, "the fleet did not accept the order: %v", err)
	}

	return &BinMoveResult{
		OrderID:       order.ID,
		VendorOrderID: vendorOrderID,
		FromNode:      sourceNode.Name,
		ToNode:        destNode.Name,
		BinLabel:      bin.Label,
	}, nil
}

// resolveMoveSource answers which bin is moving and where it is starting from.
//
// This is the whole of the difference between the two surfaces, which is why it
// is the only part of the move that branches.
func (e *Engine) resolveMoveSource(req BinMoveRequest) (*bins.Bin, *nodes.Node, error) {
	switch req.Selection {
	case BinSelectionByLabel:
		if req.BinLabel == "" {
			return nil, nil, refuse(BinMoveBadRequest, nil, "a bin is required")
		}
		bin, err := e.db.GetBinByLabel(req.BinLabel)
		if err != nil {
			return nil, nil, refuse(BinMoveBadRequest, err, "bin not found: %s", req.BinLabel)
		}
		// Two questions, because a bin is held in two stages: a soft
		// reservation taken at planning, then a hard claim taken immediately
		// before dispatch. For the whole window between them an in-flight
		// order's bin still has claimed_by NULL, so reading only the claim
		// showed a bin somebody already has as free.
		if bin.ClaimedBy != nil {
			return nil, nil, refuse(BinMoveConflict, nil,
				"bin %s is already claimed by order #%d", bin.Label, *bin.ClaimedBy)
		}
		if bin.HasPendingReservation {
			return nil, nil, refuse(BinMoveConflict, nil,
				"bin %s is already spoken for by another order — pick another bin or wait for that one to finish", bin.Label)
		}
		if bin.NodeID == nil {
			return nil, nil, refuse(BinMoveBadRequest, nil,
				"bin %s is not at any node, so there is nowhere to move it from", bin.Label)
		}
		sourceNode, err := e.db.GetNode(*bin.NodeID)
		if err != nil {
			// The bin points at a node that is not there. That is the
			// database disagreeing with itself, not the caller getting
			// anything wrong.
			return nil, nil, refuse(BinMoveFault, err,
				"bin %s says it is at a node that does not exist", bin.Label)
		}
		return bin, sourceNode, nil

	case BinSelectionAuto:
		// No bin yet, deliberately: which bin is a question about the plant,
		// and it is asked after the request itself has been found sound. See
		// pickFreeBinAt, called once the gates have passed.
		sourceNode, err := e.db.GetNode(req.SourceNodeID)
		if err != nil {
			return nil, nil, refuse(BinMoveBadRequest, ErrNodeNotFound,
				"source node not found: %d", req.SourceNodeID)
		}
		return nil, sourceNode, nil

	default:
		return nil, nil, refuse(BinMoveFault, nil, "unknown bin selection %q", req.Selection)
	}
}

// pickFreeBinAt chooses a bin to move off a node when the caller named the node
// rather than a bin.
//
// The order carries a concrete bin id either way. Without one the arrival
// handler silently skips on completion and the bin's node never reflects the
// move, which is the stuck-at-source bug.
func (e *Engine) pickFreeBinAt(sourceNode *nodes.Node) (*bins.Bin, error) {
	srcBins, err := e.db.ListBinsByNode(sourceNode.ID)
	if err != nil {
		return nil, refuse(BinMoveFault, err, "could not list bins at %s: %v", sourceNode.Name, err)
	}
	for _, b := range srcBins {
		// The same two questions the by-label path asks, asked while scanning:
		// skip a bin another order has reserved but not yet claimed, so the
		// soft-acquire later does not lose the race.
		if b.ClaimedBy == nil && !b.HasPendingReservation {
			return b, nil
		}
	}
	// A conflict with what the plant is doing, not a fault. This was a server
	// error, which told an engineer the system was broken when the answer was
	// that every bin on that node is already spoken for.
	return nil, refuse(BinMoveConflict, nil,
		"no free bin at %s — every bin there is claimed or spoken for", sourceNode.Name)
}

// resolveMoveDest answers where the bin is going, by whichever name the caller
// had.
func (e *Engine) resolveMoveDest(req BinMoveRequest) (*nodes.Node, error) {
	if req.DestNodeName != "" {
		destNode, err := e.db.GetNodeByName(req.DestNodeName)
		if err != nil {
			return nil, refuse(BinMoveBadRequest, ErrNodeNotFound,
				"destination node not found: %s", req.DestNodeName)
		}
		return destNode, nil
	}
	if req.DestNodeID == 0 {
		return nil, refuse(BinMoveBadRequest, nil, "a destination is required")
	}
	destNode, err := e.db.GetNode(req.DestNodeID)
	if err != nil {
		return nil, refuse(BinMoveBadRequest, ErrNodeNotFound,
			"destination node not found: %d", req.DestNodeID)
	}
	return destNode, nil
}
