package protocol

// Typed step / node / actor constants. Every value here is DB- and/or
// wire-serialised: the string values must never change. These centralise the
// magic strings that used to be scattered across the dispatch decision paths
// (the ALN_002 → SMN_003 incident class branched on raw "pickup"/"retrieve"
// literals).

// StepType names a leg in a reshuffle plan (dispatch.ReshuffleStep.StepType).
// A named type so callers can build exhaustive switches over it — the typed
// domain that directly attacks the ALN_002 incident class. Values are stable
// identifiers, not display text.
type StepType string

const (
	StepUnbury   StepType = "unbury"   // lift a blocking bin out of a lane
	StepRetrieve StepType = "retrieve" // fetch the target bin
	StepRestock  StepType = "restock"  // return an unburied bin to its slot
)

// Step action constants name the coarse leg kind on a ComplexOrderStep /
// dispatch.resolvedStep (the "action" field): pickup, dropoff, or wait.
//
// These are deliberately UNTYPED string constants, not a named type. The
// action field is the edge↔core wire contract (ComplexOrderStep.Action,
// json:"action") and is read as a plain string in many sites; promoting it to
// a named type means retyping that wire field across edge and core, a larger
// change deferred to its own dedicated step. Untyped constants de-stringify
// every decision site today as a drop-in (no field retype, no conversions).
const (
	ActionPickup  = "pickup"
	ActionDropoff = "dropoff"
	ActionWait    = "wait"
)

// Node class codes — the NodeTypeCode field on a node. Untyped string
// constants: NodeTypeCode is compared as a plain string across the whole node
// model (core store, dispatch, www; edge style sync), so retyping it is out of
// scope here. DB-serialised; do not change the values.
const (
	NodeClassNGRP = "NGRP" // synthetic parent grouping lanes / direct nodes
	NodeClassLANE = "LANE" // depth-ordered slot lane
	NodeClassSTOR = "STOR" // standalone storage node (store-order destination type)
)

// AuditActorUI is the audit-trail actor recorded for web-UI-initiated actions
// (the "ui" source in AuditService.Append / audit rows).
const AuditActorUI = "ui"

// ActorCoreLifecycle is the actor on a UOPAdjustment that Core generated
// itself, from a carrier's lifecycle, rather than from a person declaring a
// number. Today that is exactly one producer: the generation announcement in
// BinManifestService.bumpEpoch, which fires on load, clear, release and
// produce-finalize.
//
// It is a wire value on an existing field — nothing new travels because of it.
// It exists so the Edge can tell the two apart, which it could not before.
const ActorCoreLifecycle = "core"

// IsLifecycleActor reports whether a UOPAdjustment came from Core's own
// bookkeeping rather than from a person.
//
// THE DISTINCTION IS LOAD-BEARING AND IT IS ABOUT AN EMPTY SLOT. Both kinds of
// message carry a bin id and a count, and the Edge binds an unbound slot from
// one of them so that a carrier delivered-but-never-bound can be reconnected —
// a repair built for a person acting deliberately. Produce finalize is a
// machine firing once per press cycle, and its announcement routinely arrives
// AFTER a robot has taken the finished carrier away. Binding there attaches a
// carrier that has physically left, and the next ticks are charged to it.
//
// UNATTRIBUTED COUNTS AS LIFECYCLE, deliberately. A caller that forgets to set
// an actor gets the safe answer — the slot is left alone and the arriving
// carrier binds itself on delivery, which costs nothing. The opposite default
// would silently reinstate the misattribution the moment somebody added a
// producer, with no compile error and no failing test. Every human door
// resolves through www.resolveActor, which substitutes AuditActorUI rather
// than returning empty, so no real operator declaration lands here blank.
//
// Any other non-empty actor is a person: the cycle-count door carries a
// free-form username, so humans cannot be enumerated, only machines can.
func IsLifecycleActor(actor string) bool {
	return actor == "" || actor == ActorCoreLifecycle
}
