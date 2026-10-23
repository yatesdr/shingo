package dispatch

import (
	"errors"
	"fmt"

	"shingo/protocol"
	"shingocore/dispatch/plan"
	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// source_finder.go — the dispatch face of the planner's bin picker.
//
// THE PICKER MOVED INTO shingocore/dispatch/plan (B1): the tier cascade, the
// type derivation, the loader pool and the eligibility calls are the planner's
// "choice of source" part now, and plan.Finder.Pick is their body. What stays
// here is what only dispatch can say: the ORDER adapter (FindSource reads an
// order's fields, its sourcing intent and its dig asker, all dispatch
// vocabulary), and the translation of the planner's verdict into dispatch's
// queue vocabulary (QueueCause, QueueParams), whose names and releasers are
// dispatch's to keep (queue_cause.go). Every caller calls exactly what it called
// before; the answers are byte-identical (the planner golden).

// The finder's vocabulary, re-exported so every caller keeps its spelling.
type (
	Intent     = plan.Intent
	SourceNeed = plan.Need
	Outcome    = plan.Outcome
	FinderDB   = plan.FinderDB
)

const (
	IntentFull  = plan.IntentFull
	IntentEmpty = plan.IntentEmpty

	OutcomeFound      = plan.OutcomeFound
	OutcomeWait       = plan.OutcomeWait
	OutcomeReshuffle  = plan.OutcomeReshuffle
	OutcomeStructural = plan.OutcomeStructural
)

var _ FinderDB = (*store.DB)(nil)

// SourceResult is the closed result of FindSource. Bin and Node are returned
// together on OutcomeFound so the caller never re-resolves the node (which
// deleted two of the scanner's three ad-hoc rollbacks).
//
// It is the planner's plan.Plan with the verdict spelled in dispatch's queue
// vocabulary; sourceResultOf is the one translation.
type SourceResult struct {
	Outcome Outcome

	// OutcomeFound.
	Bin  *bins.Bin
	Node *nodes.Node

	// OutcomeWait: the structured category the order is parked under + the
	// params the operator sentence is generated from. Cause is the
	// engineer-only scope tag (which tier waited); the sentence is built by the
	// caller from QueueCode + QueueParams.
	QueueCode   protocol.QueueCode
	QueueCause  QueueCause
	QueueParams QueueParams

	// OutcomeReshuffle: the buried bin + its slot/lane for reshuffle planning.
	Buried *BuriedError

	// OutcomeStructural: TermCode is the planningError code the intake caller
	// re-raises verbatim (the queue_reason/skip-reason strings are a persisted,
	// compared contract); Err is the underlying error. The scanner maps any
	// structural outcome to its "structural" fail path.
	TermCode string
	Err      error

	// ForeignCarrier is plan.Plan's: a full taken into storage on a carrier its
	// part may not ride. The door stores it held (holdFullOnForeignCarrier).
	ForeignCarrier bool

	// PassedOver is plan.Plan's: FIFO's bin, when Need.LiftFrom took another so
	// a swap's returning carrier can land (noteSwapThroughBuffer).
	PassedOver *bins.Bin
}

// sourceResultOf translates a plan into dispatch's vocabulary. The cause is the
// same string under dispatch's name (TestPlanCausesAreDeclaredQueueCauses holds
// every planner cause to a declared QueueCause), and the params copy field for
// field.
func sourceResultOf(p plan.Plan) SourceResult {
	return SourceResult{
		Outcome:     p.Outcome,
		Bin:         p.Bin,
		Node:        p.Node,
		QueueCode:   p.QueueCode,
		QueueCause:  queueCauseOf(p.Cause),
		QueueParams: queueParamsOf(p.Params),
		Buried:      p.Buried,
		TermCode:    p.TermCode,
		Err:         p.Err,

		ForeignCarrier: p.ForeignCarrier,
		PassedOver:     p.PassedOver,
	}
}

// SourceFinder is the shared source-finding engine: the planner's picker, as
// dispatch calls it.
type SourceFinder struct {
	picker *plan.Finder
	db     FinderDB
}

// NewSourceFinder constructs a SourceFinder. resolver may be nil — the NGRP tier
// self-guards on it, exactly as the intake planners do (s.resolver != nil).
func NewSourceFinder(db FinderDB, resolver NodeResolver, dbg func(string, ...any)) *SourceFinder {
	return &SourceFinder{picker: plan.NewFinder(db, resolver, dbg), db: db}
}

// FindSourceForNeed runs the tier cascade for one NEED (plan.Finder.Pick) and
// answers in dispatch's vocabulary.
func (f *SourceFinder) FindSourceForNeed(need SourceNeed) SourceResult {
	return sourceResultOf(f.picker.Pick(need))
}

// FindSource runs the tier cascade for one order and one intent and returns a
// closed outcome. It never claims, transitions, or writes.
//
// Tier cascade (intake's order — reproduced faithfully per intent/shape so
// replay cannot drift from intake):
//
//  1. NGRP/LANE source        → resolver.Resolve, classified   (full intent)
//  2. dedicated-loader pool  → sourceFromDedicatedLoader        (Drain/Fill)
//  3. group/lane-scoped empty → FindEmptyCompatibleBinInGroup   (empty intent)
//  4. concrete-node candidate → ListBinsByNode    (move-shaped; a full naming a node)
//  5. plant-wide fallback    → FindSourceBinFIFO / FindEmptyCompatibleBin
//  6. post-find buried check → IsSlotAccessible                 (empty intent)
//
// A move sources node-locally (tiers 1,2,4) and never falls through to the
// plant-wide scan; a retrieve_empty scoped to a synthetic source queues rather
// than widening; an NGRP capacity/buried error queues (or reshuffles) scoped —
// none of these fall through to tier 5. Those "no fall-through" edges are the
// bugs the collapse fixes; keep them exact.
//
// A TOPS & BOTTOMS PULL STATES ITS WANT (Need.Want): the part's tote carrier at
// tops & bottoms only, for the fed press whose outside it lands on, which is
// the need's ProcessNode (so a strict group that supports the press serves it).
// Asked here on every find, the scanner's replay included, since no column
// carries the type; a pull that no longer resolves fails with the sentence
// intake would have refused it with (toteCarrierFor).
func (f *SourceFinder) FindSource(order *orders.Order, intent Intent) SourceResult {
	need := SourceNeed{
		SourceNode:   order.SourceNode,
		PayloadCode:  order.PayloadCode,
		DeliveryNode: order.DeliveryNode,
		Intent:       intent,
		// Move-shaped: a node-local source relocates the bin AT a concrete
		// source node and never scans plant-wide. Keyed on the sourcing intent
		// data (SourceIntentLocal), stamped at intake, never on OrderType.
		NodeLocal: order.SourceIntent == SourceIntentLocal,
		// Both read straight off the order, never re-derived: the values are in
		// hand here, and a per-step lookup would put a database round trip inside
		// the tier cascade for something the caller was already holding.
		OriginID:    order.OriginID,
		ProcessNode: order.ProcessNode,
		// Where the carrier is set down: what the empty type decider reads.
		SetDownNode: order.DeliveryNode,
		// THE ASKER, ACTUALLY FILLED. Need.Asker's doc has claimed
		// "FindSource fills it" since the field existed and this line is the
		// first time it was true. Until the empty tiers read it the omission
		// cost nothing; MG3-1b makes all four read it, so an unfilled asker
		// would mean a compound parent excluded from its OWN dig — unable to see
		// the carrier that dig uncovered for it.
		Asker: digAskerFor(order),
	}
	if order.SourceIntent == SourceIntentTopsAndBottoms {
		tc, refusal, err := toteCarrierFor(f.db, order.PayloadCode, order.DeliveryNode)
		switch {
		case err != nil:
			return sourceResultOf(plan.Plan{
				Outcome:   plan.OutcomeWait,
				QueueCode: protocol.QueueWaitingForMaterial,
				Cause:     plan.CauseFinderSourceUnreadable,
				Params:    plan.Params{Kind: plan.KindTopsAndBottoms, Payload: order.PayloadCode, Group: order.SourceNode},
			})
		case refusal != "":
			return sourceResultOf(plan.Plan{Outcome: plan.OutcomeStructural, TermCode: codeStructural,
				Err: errors.New(refusal)})
		}
		need.Want = plan.Want{Type: tc.Type, Height: domain.TopsAndBottomsOnly}
		need.ProcessNode = tc.Core
	}
	return f.FindSourceForNeed(need)
}

// toteCarrierFor answers a tops & bottoms pull of part to outside in one read
// (store ToteCarrierFor): the fed press and the tote carrier, or the sentence
// it is refused with when the delivery is no fed press's outside carrying the
// part, or the part names no tote carrier, or more than one. err is a read
// that did not answer, which is no refusal. Its askers: the intake guard
// (decideTransport) and every find (FindSource).
func toteCarrierFor(db FinderDB, part, outside string) (tc bins.ToteCarrier, refusal string, err error) {
	if tc, err = db.ToteCarrierFor(part, outside); err != nil {
		return tc, "", err
	}
	switch {
	case tc.Core == "":
		return tc, fmt.Sprintf("%s is not the outside position of a press fed with %s", outside, part), nil
	case tc.Types == 0:
		return tc, fmt.Sprintf("%s names no tote carrier", part), nil
	case tc.Types > 1:
		return tc, fmt.Sprintf("%s names more than one tote carrier", part), nil
	}
	return tc, "", nil
}

// queueCauseOf names a planner cause in dispatch's queue vocabulary. The value is
// the same string (TestPlanCausesAreDeclaredQueueCauses), so this is a change of
// type, never of meaning.
func queueCauseOf(c plan.Cause) QueueCause { return QueueCause(c) }

// queueParamsOf copies a planner verdict's params into the queue params the
// operator sentence is built from.
func queueParamsOf(p plan.Params) QueueParams {
	return QueueParams{
		Payload:        p.Payload,
		Kind:           p.Kind,
		Group:          p.Group,
		Reserved:       p.Reserved,
		SourceMissing:  p.SourceMissing,
		CarrierRefused: p.CarrierRefused,
		Destination:    p.Destination,
		BlockingBins:   p.BlockingBins,
		InboundOrders:  p.InboundOrders,
		AtLevel:        p.AtLevel,
	}
}

// ForecastPick is the pick a door would make for need, made for a place with
// no order (the forecaster, D2; the level keeper before it mints an ask,
// Maintainer.nothingToSource): the same finder, Asker domain.Anyone (the zero
// value, which every dig excludes), nothing held. The planner writes nothing;
// the zero-write test covers it.
func (d *Dispatcher) ForecastPick(need SourceNeed) SourceResult {
	return d.finder.FindSourceForNeed(need)
}
