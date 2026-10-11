package plan

import (
	"shingo/protocol"
	"shingocore/dispatch/binresolver"
	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// need.go — the planner's vocabulary: what a caller ASKS for (Need, Intent,
// Given), what it GETS BACK (Plan, Outcome and the verdict), and the store surface
// the picker is allowed to read (FinderDB, part of Reader).
//
// Moved from dispatch/source_finder_types.go (B1). The need and the verdict were
// already need-shaped: SourceNeed carried no order, and SourceResult was the
// closed "satisfiable, or missing X" answer. They are the planner's types now,
// and dispatch keeps its names as aliases (SourceNeed) or as a translation of the
// verdict into its queue vocabulary (SourceResult).
//
// WHAT A PLAN DOES NOT CARRY YET, AND WHY (v4 §2.9 law 5: every new fact names
// its reader in the same change). Contention ("who else wants it") arrives with
// the friction view (D2), and a rank key with the comparator (C2b/D3); neither
// has a reader in B1, so neither is declared. A plan's destination and legs are
// answered by the planner's other parts (Planner.StoreDestination,
// Planner.ComplexSteps, TransportLegs) and are gathered into one Plan when the
// builder that reads them lands (B2).

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

// Need is the need-shaped input to the finder: WHAT is required and
// WHERE it may come from, with no *orders.Order attached. It exists because
// the finder used to derive its scoping from order fields, and a complex
// order's fields describe the ORDER, not the individual source need -- a
// complex pickup fed through the order-shaped entry point read as
// SourceIntentFull and fell through to the tier-5 plant-wide scan while
// steps_json still drove the robot to the step node.
//
// NodeLocal makes tier 5 UNREACHABLE BY TYPE: a node-local need queues at its
// node instead of widening plant-wide. That is a parameter now, not a comment.
type Need struct {
	SourceNode   string
	PayloadCode  string
	DeliveryNode string
	Intent       Intent
	NodeLocal    bool

	// Asker is the order this need belongs to, for the dig-lock question — on
	// the NGRP tier AND, as of MG3-1b, on all four empty finders.
	//
	// The zero value is domain.Anyone, which every dig excludes. That is
	// the owner-blind reading, and it is the SAFE direction for a need with no
	// order behind it: a caller with nothing to exempt should not walk into a
	// lane somebody is digging.
	//
	// FINDSOURCE ACTUALLY FILLS IT NOW, via digAskerFor. It did not until
	// MG3-1b — this comment promised it and FindSource omitted the field, so
	// every need built there carried Anyone. The consequence was nil while no
	// empty tier read Asker; it became a live trap the moment they did, which is
	// why MG3-0 pinned the omission as a red test and this commit deletes that
	// pin rather than quietly satisfying it.
	//
	// Callers building a need by hand should fill it too when they have an
	// order, or a resuming complex parent cannot see the bin its own dig
	// uncovered.
	Asker domain.DigAsker

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

	// Order is the order this need is planned FOR, when there is one and the
	// plan's own earlier steps count: the vacated-slot rule's case (a) asks what
	// THIS order's earlier pickups take (BinsAtStep). 0 at intake, where no row
	// exists yet, and that keeps intake's answers exactly as they were.
	Order int64
	// Pair is the partner's leg, for the leg that runs second in a pair's pass:
	// the vacated-slot rule's case (b). The pass's own output, handed over where
	// the pass forms the pair and never read back (vacated.go). nil for a solo
	// order, the first leg, and every need that is not a pair's.
	Pair *PairLeg
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

// Plan is the picker's answer to one Need: a bin and the node it stands on, or
// the reason there is none (v4 §2.3, "satisfiable, or missing X"). Bin and Node
// are returned together on OutcomeFound so the caller never re-resolves the node
// (which deleted two of the scanner's three ad-hoc rollbacks).
type Plan struct {
	Outcome Outcome

	// OutcomeFound.
	Bin  *bins.Bin
	Node *nodes.Node

	// OutcomeWait: the structured category the need waits under, and the params
	// the operator sentence is generated from. Cause is the engineer-only scope
	// tag (which tier waited). Dispatch parks the order under the same cause, by
	// the same string, in its own queue vocabulary.
	QueueCode protocol.QueueCode
	Cause     Cause
	Params    Params

	// OutcomeReshuffle: the buried bin + its slot/lane for reshuffle planning.
	Buried *binresolver.BuriedError

	// OutcomeStructural: TermCode is the planningError code the intake caller
	// re-raises verbatim (the queue_reason/skip-reason strings are a persisted,
	// compared contract); Err is the underlying error. The scanner maps any
	// structural outcome to its "structural" fail path.
	TermCode string
	Err      error

	// THE GATHERED PLAN (B2). A move's plan (move.go) carries its destination
	// and whether it takes a soft hold, and the builder reads both (dispatch.Build
	// writes Dest as the row's delivery node; the bin-move door takes the hold in
	// the birth's transaction). The picker leaves them zero.
	//
	// Dest is the node the carrier is put down on: the named node, or the child
	// the planner chose when the named node is a group.
	Dest *nodes.Node
	// Take is Q1's explicit take: the bin carries another order's PENDING
	// reservation, and a person's move takes it (v4 §2.7, Rule 4).
	Take bool
	// Refused is the move's "not satisfiable": which rule said no, for a door
	// with a person waiting. nil when the plan can be carried out.
	Refused *Refusal
	// ForeignCarrier: the found bin is a full on a carrier its part may not
	// ride, taken by a move into storage because nothing lawful stood at the
	// source (finder.go, the node-local arm). The door stores it held.
	ForeignCarrier bool
}

// Given is a fully specified manual move (v4 §2.5): the bin, where it stands
// and where it goes are all named, and the planner VALIDATES that plan instead of
// choosing one (Planner.ValidateMove). It carries the rows the door read for what
// the person named, so validating costs no second read of them.
type Given struct {
	Bin    *bins.Bin
	Source *nodes.Node
	Dest   *nodes.Node
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
	FindEmptyCompatibleBin(payloadCode, preferZone string, excludeNodeID int64, fence bins.EmptyFence, asker domain.DigAsker) (*bins.Bin, error)
	FindEmptyCompatibleBinInGroup(payloadCode string, groupNodeID, excludeNodeID int64, asker domain.DigAsker) (*bins.Bin, error)
	FindEmptyBinOfType(binTypeCode, preferZone string, excludeNodeID int64, fence bins.EmptyFence, asker domain.DigAsker) (*bins.Bin, error)
	FindEmptyBinOfTypeInGroup(binTypeCode string, groupNodeID, excludeNodeID int64, asker domain.DigAsker) (*bins.Bin, error)
	// The typed finders for a SET of types: a no-mix loader whose payloads may
	// ride more than one carrier type (Finder.wantedBinTypes).
	FindEmptyBinOfTypes(binTypeCodes []string, preferZone string, excludeNodeID int64, fence bins.EmptyFence, asker domain.DigAsker) (*bins.Bin, error)
	FindEmptyBinOfTypesInGroup(binTypeCodes []string, groupNodeID, excludeNodeID int64, asker domain.DigAsker) (*bins.Bin, error)
	// MaintainedEpisodeForOrigin is the same read with BOTH halves of the pair:
	// the group the episode belongs to as well as its carrier type. The group is
	// what keeps a top-off ask from sourcing out of the group it is filling.
	MaintainedEpisodeForOrigin(originID string) (groupNode, binType string, err error)
	// GroupFencesAsker answers the fence as a DISPOSITION, for a need that named
	// a group explicitly. The plant-wide fence is a filter inside the query; this
	// one has to produce a cause.
	GroupFencesAsker(groupNodeID int64, processNode string) (bool, error)
	// MaintainedGroupsSupporting and NodeIsUnderAny are the MG3-5 audit's two
	// reads: which groups serve this process, and did the carrier come from
	// inside one of them.
	// GetEffectiveBinTypes is MG3-4's read: what physically fits at a position,
	// resolved through the inherit chain. One type is a statement; zero or many
	// leave the choice to the cascade's ordering.
	GetEffectiveBinTypes(nodeID int64) ([]*bins.BinType, error)
	// LoadBinTypeRule is the other direction: which carrier types a PART may
	// travel in (payload_bin_types), as opposed to which fit at a node. Read
	// once per door call and judged against every candidate bin.
	LoadBinTypeRule(payloadCode string) (domain.BinTypeRule, error)
	MaintainedGroupsSupporting(processNode string) ([]int64, error)
	NodeIsUnderAny(nodeID int64, roots []int64) (bool, error)

	IsSlotAccessible(slotNodeID int64) (bool, error)
	GetLoaderHomeByPositionNode(positionNodeID int64) (*loaders.Home, error)
	// GetLoaderHomeCarriers is the home with the carrier types its payloads may
	// ride (nil = any), one read: the type derivation's home read.
	GetLoaderHomeCarriers(positionNodeID int64) (*loaders.Home, []string, error)
	// GetLoaderHomeAndLayout is the home with its loader's layout, one read: the
	// loader tier's gate.
	GetLoaderHomeAndLayout(positionNodeID int64) (*loaders.Home, string, error)
	GetLoader(id int64) (*loaders.Loader, error)
	ListLoaderHomes(loaderID int64) ([]loaders.Home, error)
	ListLoaderQuotas(loaderID int64) ([]loaders.Quota, error)
	ListLoaderHomeBinTypes(loaderID int64) (map[int64][]string, error)
}
