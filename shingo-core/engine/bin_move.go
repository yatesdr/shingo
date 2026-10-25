package engine

import (
	"errors"
	"fmt"

	"github.com/google/uuid"

	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/plan"
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
	// Projects is the station whose Edge models the destination and binds the
	// bin when it arrives, so it is sent the move's order.delivered and
	// bin_picked_up and nothing else (dispatch.Need.Projects). The stage-2 pull
	// names its window's Edge; a person's move leaves it blank.
	Projects string
	// EdgeUUID is the order's reference number. Blank mints one.
	EdgeUUID string
	Priority int
	Desc     string
}

// BinMoveResult describes the order that is now on its way — or, when Queued is
// set, the order that is waiting for a lane and will go on its way by itself.
type BinMoveResult struct {
	OrderID       int64
	VendorOrderID string
	FromNode      string
	ToNode        string
	BinLabel      string

	// Queued is true when acquiring could not dispatch the move now (a lane, a
	// slot, the fleet). The order is real, holds its bin, and the fulfillment
	// scanner drives it in as soon as the wait clears — so this is not a refusal
	// and must not be answered as one. QueueReason is the operator sentence
	// saying what it is waiting for.
	//
	// The pick is never blocked at the UI; the queue message carries the wait.
	// The alternative — refusing at the door — would make an operator guess when
	// to try again for a condition Core is already watching.
	Queued      bool
	QueueReason string
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
//
// ONE PIPELINE SINCE B2 (v4 §2.1, §2.5). The door resolves what the person
// named, the planner validates or chooses the plan, the builder writes it
// `pending` with the take and the move's own soft hold in one transaction, and
// acquiring tries that one order once: dispatchHeldBin dispatches it straight
// from `pending` (no station knows it, so it takes SYNTH F8's edge), or parks
// it `queued` with its cause. The door no longer dispatches (ruling F2), so what
// it answers is the row as acquiring left it.
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
	// The specific answer has to win over the generic one: the capacity verdict
	// catches most of these incidentally — the bin is at the destination, so the
	// destination reads as occupied — and then tells the operator to go clear a
	// node whose only occupant is the bin they are moving. It also does not catch
	// all of them, because it defers on lane nodes.
	//
	// Scoped to this door. It must never move into the shared writer or the
	// dispatch tail: a complex order's first and last step are legitimately the
	// same node — a robot lifts a bin off a position, takes it away and brings a
	// different one back — so a check placed there would break changeovers.
	if sourceNode.ID == destNode.ID {
		if bin != nil {
			return nil, refuse(BinMoveBadRequest, nil,
				"bin %s is already at %s", bin.Label, destNode.Name)
		}
		return nil, refuse(BinMoveBadRequest, nil,
			"source and destination must be different (both are %s)", destNode.Name)
	}

	// THE PLAN. A named bin is validated (the person is authoritative over soft
	// holds, never over a robot); a named node has its bin chosen. Everything the
	// planner refuses, it refuses before any row exists.
	p := e.dispatcher.PlanMove(plan.Given{Bin: bin, Source: sourceNode, Dest: destNode})
	if p.Refused != nil {
		return nil, moveRefusal(p.Refused, bin, sourceNode, destNode)
	}
	// A BURIED BIN WHOSE GROUP HAS NOWHERE TO PARK ITS BLOCKERS IS REFUSED NOW
	// (owner, 2026-09-26): a person is waiting at this door. Born, the move would
	// be sent to the lane's mark and wait there under no-shuffle-slot while the
	// person is told "queued". The dig's own verdict, asked before the issue.
	var short *plan.DigShortError
	if err := e.dispatcher.MoveDigFits(p.Bin, p.Node, p.Dest); errors.As(err, &short) {
		return nil, refuse(BinMoveConflict, err,
			"bin %s is behind %d other bin(s) at %s, and its group has nowhere to park them right now "+
				"— free a slot in the group and try again", p.Bin.Label, short.Blockers, p.Node.Name)
	}

	edgeUUID := req.EdgeUUID
	if edgeUUID == "" {
		// A bare identifier, like the Edge mints. Full length, not the first
		// eight characters: eight hex characters is about four billion values,
		// which sounds like plenty until you notice nothing anywhere handles a
		// collision.
		edgeUUID = uuid.New().String()
	}
	need := dispatch.Need{
		Type:    protocol.OrderTypeMove,
		Station: req.StationID,
		Ref:     edgeUUID,
		// One robot, one bin.
		Quantity: 1,
		Priority: req.Priority,
		Desc:     req.Desc,
		// NO_DEMAND, stamped where it is known. Somebody moving a bin from A to
		// B is not a place asking for material, so there is no episode and its
		// absence is not a finding. Left blank it would land orphan and put a
		// deliberate human action in the one bucket that is supposed to mean
		// "we lost a demand link". The issue stamps SourceIntent LOCAL from the type,
		// through the same map as every other door.
		OriginClass: protocol.OriginClassNoDemand,
		// A bin move is Core-born, so its station has no row for it and is sent
		// no ack, waybill or status (v4 §2.5). It projects its delivery to the
		// Edge the door names, or to nobody.
		Projects: req.Projects,
		// The move is for the bin: its issue holds it softly and stamps it on the
		// row. A person asked, so a stranger's soft hold on it is taken out loud.
		Cart: p.Bin.ID,
		By:   dispatch.Person(moveActor(req)),
	}

	// THE ISSUE: the row, the take and the move's own soft hold, in one
	// transaction. orders.Create writes the creation history row inside it, so
	// every move's timeline begins, including the ones that go on to wait.
	order, taken, err := e.dispatcher.IssueOrder(need, p)
	if err != nil {
		// Another order reserved the bin between the plan and the transaction: a
		// race with a person, not a fault. Nothing was written.
		if errors.Is(err, reservations.ErrReservationConflict) {
			return nil, refuse(BinMoveConflict, ErrBinTaken,
				"bin %s was taken a moment ago — try again", p.Bin.Label)
		}
		return nil, refuse(BinMoveFault, err, "could not create the order: %v", err)
	}
	// A person's move that had promised itself this bin is ended by name, never
	// re-aimed at whatever else stands at its node (store.TakeSoftHolds).
	for _, id := range taken.Named {
		e.failOrderAndEmit(id, "bin_taken", fmt.Sprintf(
			"bin %s was taken by order #%d (a person's move)", p.Bin.Label, order.ID))
	}

	// ACQUIRING, this one order, once (the orc's Stop 2 ruling): when this
	// returns the move is dispatched, or queued with its cause on the row for
	// the periodic pass to ask again.
	e.acquireOne(order)
	// A move that took a promised bin un-pointed its holder (TakeSoftHolds),
	// which waits with the bin-taken cause. One pass now re-finds it, as main's
	// queue event did, instead of the 60 s floor. A move that took nothing
	// asks nothing else.
	if p.Take && e.fulfillment != nil {
		e.fulfillment.RunOnce()
	}
	return e.binMoveAsItStands(order, p)
}

// door runs a door's unit under the scan lock (dispatch/door.go), or directly in
// an engine built without a scanner (some tests).
func (e *Engine) door(fn func()) {
	if e.fulfillment != nil {
		e.fulfillment.Door(fn)
		return
	}
	fn()
}

// acquireOne hands a just-born order to acquiring. An engine built without a
// scanner (some tests) leaves it `pending`, in the scanner's population, for
// whoever scans: nothing writes a wait without a cause in place of a scan that
// is not there.
func (e *Engine) acquireOne(order *orders.Order) {
	if e.fulfillment != nil {
		e.fulfillment.AcquireOne(order.ID)
	}
}

// binMoveAsItStands answers the person with the order as acquiring left it.
//
// A move that waits is real and is not refused: it holds its bin, and the
// scanner drives it in when its wait clears. The sentence on the row says what
// it waits for. Only a move acquiring ended (a structural fault) is refused.
func (e *Engine) binMoveAsItStands(order *orders.Order, p plan.Plan) (*BinMoveResult, error) {
	res := &BinMoveResult{
		OrderID:  order.ID,
		FromNode: p.Node.Name,
		ToNode:   p.Dest.Name,
		BinLabel: p.Bin.Label,
	}
	fresh, err := e.db.GetOrder(order.ID)
	if err != nil {
		e.logFn("engine: re-read bin move order %d: %v", order.ID, err)
		return res, nil
	}
	switch {
	case protocol.IsTerminal(fresh.Status):
		return nil, refuse(BinMoveFault, nil, "the move could not be carried out: %s", fresh.ErrorDetail)
	case fresh.VendorOrderID != "":
		res.VendorOrderID = fresh.VendorOrderID
	default:
		res.Queued = true
		res.QueueReason = fresh.QueueReason
	}
	return res, nil
}

// moveRefusal says a planner refusal in the door's vocabulary: the kind a caller
// answers with, and the sentence a person reads.
func moveRefusal(r *plan.Refusal, bin *bins.Bin, source, dest *nodes.Node) *BinMoveError {
	label := ""
	if bin != nil {
		label = bin.Label
	}
	switch r.Rule {
	case plan.RefusedClaimed:
		return refuse(BinMoveConflict, nil, "bin %s is already claimed by order #%d", label, r.Holder)
	case plan.RefusedLocked:
		return refuse(BinMoveConflict, nil, "bin %s is locked for handling — try again once it is released", label)
	case plan.RefusedNotAtSource:
		return refuse(BinMoveBadRequest, nil, "bin %s is not at %s", label, source.Name)
	case plan.RefusedDestDisabled:
		return refuse(BinMoveBadRequest, nil, "%s is disabled, so nothing can be moved there", dest.Name)
	case plan.RefusedCarrier:
		if r.Err != nil {
			return refuse(BinMoveConflict, r.Err, "%s cannot take the bin right now (%v)", dest.Name, r.Err)
		}
		return refuse(BinMoveBadRequest, nil, "%s does not take bin %s's carrier", dest.Name, label)
	case plan.RefusedDestFull:
		return refuse(BinMoveConflict, ErrDestinationOccupied, "%s", dispatch.CapacitySentence(r.Capacity))
	case plan.RefusedNoFreeBin:
		// A conflict with what the plant is doing, not a fault: every bin on that
		// node is spoken for.
		return refuse(BinMoveConflict, nil,
			"no free bin at %s — every bin there is claimed or spoken for", source.Name)
	default:
		return refuse(BinMoveFault, r.Err, "could not plan the move: %v", r.Err)
	}
}

// moveActor is who the history records as taking a promised bin: the station
// that asked, or the door when none did.
func moveActor(req BinMoveRequest) string {
	if req.StationID != "" {
		return req.StationID
	}
	return "bin-move"
}

// resolveMoveSource answers which bin is moving and where it is starting from.
//
// This is the whole of the difference between the two surfaces, which is why it
// is the only part of the move that branches. It reads what the person named;
// whether the plant lets it move is the planner's (ValidateMove, ChooseMove).
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
		// and the planner asks it once the request itself has been found sound.
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
