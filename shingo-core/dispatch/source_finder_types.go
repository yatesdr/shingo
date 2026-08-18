package dispatch

import (
	"shingo/protocol"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// source_finder_types.go — the source-finding seam's vocabulary: what a caller
// ASKS for (Intent, SourceNeed), what it GETS BACK (Outcome, SourceResult), and
// the store surface the finder is allowed to reach (FinderDB).
//
// MOVED OUT OF source_finder.go UNCHANGED. Nothing here is new and nothing was
// edited in the move: the file was 923 lines with the tier cascade, the type
// derivation, and these declarations sharing one screen. The cascade is the part
// people read; the vocabulary is the part they look up. See source_finder.go for
// what the seam is FOR.

// Intent distinguishes what kind of bin the caller needs. It is keyed on the
// order's data, never on OrderType==Complex or StepsJSON.
//
// FindSource has exactly two callers — PlanningService (simple retrieve /
// retrieve_empty / move intake) and the fulfillment scanner's replay. The
// Allocator is NOT one of them: it sources complex orders through its own
// findAvailableForNeed, which reads a single node. Do not assume complex
// pickups are covered by anything in this file.
type Intent int

const (
	// IntentFull needs a bin holding the order's payload (retrieve, move).
	IntentFull Intent = iota
	// IntentEmpty needs an empty compatible carrier (retrieve_empty).
	IntentEmpty
)

// SourceNeed is the need-shaped input to the finder: WHAT is required and
// WHERE it may come from, with no *orders.Order attached. It exists because
// the finder used to derive its scoping from order fields, and a complex
// order's fields describe the ORDER, not the individual source need -- a
// complex pickup fed through the order-shaped entry point read as
// SourceIntentFull and fell through to the tier-5 plant-wide scan while
// steps_json still drove the robot to the step node.
//
// NodeLocal makes tier 5 UNREACHABLE BY TYPE: a node-local need queues at its
// node instead of widening plant-wide. That is a parameter now, not a comment.
type SourceNeed struct {
	SourceNode   string
	PayloadCode  string
	DeliveryNode string
	Intent       Intent
	NodeLocal    bool

	// Asker is the order this need belongs to, for the dig-lock question on the
	// NGRP tier. The zero value is reservations.Anyone, which every dig
	// excludes — the same answer this path gave before the field existed, so a
	// construction site that does not set it is unchanged rather than wrong.
	// FindSource fills it; callers building a need by hand should too when they
	// have an order, or a resuming complex parent cannot see the bin its own
	// dig uncovered.
	Asker reservations.DigAsker

	// OriginID is the demand episode this need serves, when it has one.
	//
	// IT IS HOW A TYPE TRAVELS FROM A DECISION TO A SOURCE. The maintained-group
	// level keeper decides it is short of one carrier TYPE, records that in the
	// episode key, and stamps the episode on the ask; wantedBinType's first arm
	// reads it back. Nothing else in the plant needs it — every other need
	// derives its type, or has none.
	//
	// FILLED FROM THE ORDER IN FindSource, never looked up per step. The field is
	// already in the caller's hand, and a per-step read would put a database
	// round trip inside the tier cascade for a value the caller was holding.
	// Blank for every non-keeper order, which is nearly all of them.
	OriginID string

	// ProcessNode is the equipment position this need is being sourced FOR.
	//
	// Unused by the cascade today and carried now because strict sourcing is
	// what consumes it: "may this asker take an empty out of that maintained
	// group" is a question about the PROCESS, and by the time the plant-wide
	// scan runs there is nothing left to derive it from. Filled from the order
	// alongside OriginID, for the same reason and in the same place — one
	// change, both fields, no per-step reads.
	//
	// BLANK MEANS OUTSIDER once the fence exists, which is the correct default
	// for everyone except the keeper — and the keeper is exempted on its ORIGIN
	// rather than on this field, precisely because its own needs carry no
	// process (SYNTH round 2 §2).
	ProcessNode string
}

// Outcome is the closed disposition set FindSource returns.
type Outcome int

const (
	// OutcomeFound — a bin was located; Bin and Node are both set.
	OutcomeFound Outcome = iota
	// OutcomeWait — no bin available now; the caller queues with QueueReason.
	OutcomeWait
	// OutcomeReshuffle — the only candidate is buried; Buried carries the plan input.
	OutcomeReshuffle
	// OutcomeStructural — a permanent/terminal failure; TermCode + Err describe it.
	OutcomeStructural
)

// SourceResult is the closed result of FindSource. Bin and Node are returned
// together on OutcomeFound so the caller never re-resolves the node (which
// deleted two of the scanner's three ad-hoc rollbacks).
type SourceResult struct {
	Outcome Outcome

	// OutcomeFound.
	Bin  *bins.Bin
	Node *nodes.Node

	// OutcomeWait: the structured category the order is parked under + the
	// params the operator sentence is generated from. Replaces a pre-formatted
	// reason string so the caller parks through the formatter door (the same
	// code surfaces from every finder tier). Cause is the engineer-only scope
	// tag (which tier waited); the sentence is built by the caller from
	// QueueCode + QueueParams.
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
}

// FinderDB is the narrow store surface the finder needs. *store.DB satisfies it
// structurally; the assertion below catches a drift in the store method set, and
// finder tests drop a fake in to prove tier scoping (e.g. "FindSourceBinFIFO is
// never called while the loader pool is empty").
type FinderDB interface {
	GetNodeByDotName(name string) (*nodes.Node, error)
	GetNode(id int64) (*nodes.Node, error)
	ListBinsByNode(nodeID int64) ([]*bins.Bin, error)
	ListBinsByNodes(nodeIDs []int64) ([]*bins.Bin, error)
	FindSourceBinFIFO(payloadCode string, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyCompatibleBin(payloadCode, preferZone string, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyCompatibleBinInGroup(payloadCode string, groupNodeID, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyBinOfType(binTypeCode, preferZone string, excludeNodeID int64) (*bins.Bin, error)
	FindEmptyBinOfTypeInGroup(binTypeCode string, groupNodeID, excludeNodeID int64) (*bins.Bin, error)
	// MaintainedTypeForOrigin resolves an ask's origin to the carrier type its
	// maintained-group episode is short of, or "" when the origin is not one.
	// The finder's whole view of the typed ask; see wantedBinType's first arm.
	MaintainedTypeForOrigin(originID string) (string, error)
	IsSlotAccessible(slotNodeID int64) (bool, error)
	GetLoaderHomeByPositionNode(positionNodeID int64) (*loaders.Home, error)
	GetLoader(id int64) (*loaders.Loader, error)
	ListLoaderHomes(loaderID int64) ([]loaders.Home, error)
	ListLoaderQuotas(loaderID int64) ([]loaders.Quota, error)
	ListLoaderHomeBinTypes(loaderID int64) (map[int64][]string, error)
}

var _ FinderDB = (*store.DB)(nil)
