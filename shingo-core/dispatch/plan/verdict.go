package plan

import (
	"errors"
	"strings"

	"shingo/protocol"
	"shingocore/dispatch/binresolver"
)

// verdict.go — the words a plan's "missing X" is said in.
//
// A plan that cannot be completed says why: a Cause (the engineer-only tag for
// which tier or gate waited) and the Params the operator sentence is built from.
// These are the planner's own words, and each Cause is spelled with exactly the
// string dispatch parks an order under: dispatch owns the queue vocabulary (the
// names, the releasers, the board), and it translates a plan's verdict into it
// at the one door where a plan becomes an order's wait (dispatch.sourceResultOf).
// TestPlanCausesAreDeclaredQueueCauses holds every Cause here to a declared
// dispatch QueueCause of the same value, so the two cannot drift.

// Cause is the engineer-only tag on a waiting verdict.
type Cause string

// The picker's causes. Each is the value of the dispatch QueueCause of the same
// name; its meaning and its releaser are documented there (queue_cause.go,
// queue_releasers.go).
const (
	CauseReadFailed                    Cause = "read-failed"
	CauseLoaderSourceUnreadable        Cause = "loader-source-unreadable"
	CauseFinderNodeEmpty               Cause = "finder-node-empty"
	CauseFinderGroupEmpty              Cause = "finder-group-empty"
	CauseFinderPoolEmpty               Cause = "finder-pool-empty"
	CauseFinderPlantEmpty              Cause = "finder-plant-empty"
	CauseFinderNoFullCarrier           Cause = "finder-no-full-carrier"
	CauseFinderAccessibilityUnreadable Cause = "finder-accessibility-unreadable"
	CauseFinderGroupFenced             Cause = "finder-group-fenced"
	CauseFinderSourceUnreadable        Cause = "finder-source-unreadable"
	CauseFinderSourceMissing           Cause = "finder-source-missing"
	CauseFinderNoEmptyOfType           Cause = "finder-no-empty-of-type"
	CauseFinderCarrierRefused          Cause = "finder-carrier-refused"
	CauseFinderNoToteCarrier           Cause = "finder-no-tote-carrier"
)

// The dropoff-capacity gate's causes (capacity.go), under the same rule: each is
// the value of the dispatch QueueCause of the same name.
const (
	CauseDropoffOccupied     Cause = "dropoff-occupied"
	CauseDropoffInflight     Cause = "dropoff-inflight"
	CauseNGRPFull            Cause = "ngrp-full"
	CauseNGRPAtLevel         Cause = "ngrp-at-level"
	CauseCapacityCheckFailed Cause = "capacity-check-failed"
)

// Causes enumerates every Cause the planner can return, for the drift test.
var Causes = []Cause{
	CauseReadFailed, CauseLoaderSourceUnreadable,
	CauseFinderNodeEmpty, CauseFinderGroupEmpty, CauseFinderPoolEmpty, CauseFinderPlantEmpty,
	CauseFinderNoFullCarrier, CauseFinderAccessibilityUnreadable, CauseFinderGroupFenced,
	CauseFinderSourceUnreadable, CauseFinderSourceMissing, CauseFinderNoEmptyOfType,
	CauseFinderCarrierRefused, CauseFinderNoToteCarrier,
	CauseDropoffOccupied, CauseDropoffInflight, CauseNGRPFull, CauseNGRPAtLevel, CauseCapacityCheckFailed,
}

// Params are the facts a waiting verdict's operator sentence is built from: the
// subset of dispatch.QueueParams the planner fills, field for field.
type Params struct {
	Payload string
	// Kind is what the need waits for when it is not a full (parkKind):
	// KindEmpty, KindTopsAndBottoms, or "" for a full.
	Kind          string
	Group         string
	Reserved      bool
	SourceMissing bool
	// CarrierRefused: the named node holds the part only on carriers the part
	// may not ride (CauseFinderCarrierRefused). Group carries the node.
	CarrierRefused bool
	// NoToteCarrier: tops & bottoms is wanted for a part whose carrier rule
	// names no tote carrier (CauseFinderNoToteCarrier). Payload carries the part.
	NoToteCarrier bool

	// The capacity gate's: the destination it gated, the bins standing there,
	// the orders already inbound, and whether a maintained group is at its level.
	Destination   string
	BlockingBins  int
	InboundOrders int
	AtLevel       bool
}

// The terminal codes a structural verdict carries. The same values as the
// planning service's codes (protocol's Term* codes); dispatch re-raises them
// verbatim.
var (
	termStructural  = string(protocol.TermStructural)
	termNode        = string(protocol.TermNodeError)
	termInvalidNode = string(protocol.TermInvalidNode)
)

type resolutionClass int

const (
	resolutionOther resolutionClass = iota
	resolutionBuried
	resolutionStructural
)

// classifyForPick is the half of dispatch's classifyResolutionError the picker
// branches on: a buried answer (a dig can reach the bin), a structural one (the
// group cannot serve this at all), or anything else (wait). Dispatch's
// classifier asks these two first, through BuriedOrStructural, so the two
// cannot disagree about them.
func classifyForPick(err error) (resolutionClass, any) {
	b, s := BuriedOrStructural(err)
	switch {
	case b != nil:
		return resolutionBuried, b
	case s != nil:
		return resolutionStructural, s
	}
	return resolutionOther, nil
}

// BuriedOrStructural reads a resolver error as the two answers a plan acts on: a
// buried bin (ErrBuried carrying its *BuriedError), or a structural refusal. An
// ErrBuried with no BuriedError attached is neither; both nil is every other
// error.
func BuriedOrStructural(err error) (*binresolver.BuriedError, *binresolver.StructuralError) {
	if err == nil {
		return nil, nil
	}
	var buried *binresolver.BuriedError
	if errors.Is(err, binresolver.ErrBuried) {
		if errors.As(err, &buried) {
			return buried, nil
		}
		return nil, nil
	}
	var structural *binresolver.StructuralError
	if errors.As(err, &structural) {
		return nil, structural
	}
	return nil, nil
}

// The capacity shapes a group resolution can refuse with: the resolver
// returns each as a plain fmt.Errorf, so they are named by the substring that
// survives every wrap. dispatch's classifier and the planner's vacated-slot arm
// read them through CapacityShape, so the two cannot disagree.
const (
	CapacityEmptiesOnly = "empties-only" // a labelled store aimed at an empties bank
	CapacitySlot        = "slot"         // the group has no free slot
	CapacityPayload     = "payload"      // the group holds none of the payload
	CapacityBin         = "bin"          // the empty-fetch pool is dry
)

// CapacityShape names the capacity shape of a resolution error, or "" when it
// is not one — a buried or structural refusal (BuriedOrStructural), an
// ErrBuried with nothing attached, or any other error. Checked most specific
// first: the empties-only statement is about the same destination as the slot
// shape, and the group may well have free slots.
func CapacityShape(err error) string {
	if err == nil {
		return ""
	}
	if b, st := BuriedOrStructural(err); b != nil || st != nil || errors.Is(err, binresolver.ErrBuried) {
		return ""
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "empties-only node group"):
		return CapacityEmptiesOnly
	case strings.Contains(msg, "no available slot in node group"):
		return CapacitySlot
	case strings.Contains(msg, "no bin of requested payload in node group"):
		return CapacityPayload
	case strings.Contains(msg, "cannot resolve empty in group"), strings.Contains(msg, "no empty carrier in group"):
		return CapacityBin
	}
	return ""
}

// The Kind values a wait carries (Params.Kind). The operator sentence reads
// them (dispatch materialSentence).
const (
	KindEmpty          = "empty"
	KindTopsAndBottoms = "tops_bottoms"
)

// parkKind is the Kind a need's wait carries, at every park the finder writes:
// tops & bottoms for a need stating that height, an empty for any other empty,
// "" for a full.
func parkKind(need Need) string {
	switch {
	case need.Intent != IntentEmpty:
		return ""
	case need.Height == protocol.TopsAndBottomsOnly:
		return KindTopsAndBottoms
	}
	return KindEmpty
}
