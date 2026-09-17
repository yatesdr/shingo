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
// number. One producer writes it: the generation announcement in
// BinManifestService.bumpEpoch, on the resets nobody is standing at — a press
// finishing a carrier, a release, a dispatch claim. bumpEpoch also announces
// the two an operator performs at the bin detail modal, and those carry
// AuditActorUI instead; which it sends is the Declarer its caller passed.
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

// Declarer is the SENDING half of the distinction IsLifecycleActor reads: who
// a carrier-reset announcement is from, chosen where the reset happens.
//
// IT IS NOT THE AUDIT ACTOR, and the release path is the proof. Releasing a
// bin is audited to a named person — an operator screen pressed a button — and
// its announcement must still say lifecycle, because a release is a carrier
// LEAVING a node and binding an empty slot to a departing carrier is the exact
// misattribution the Edge guard exists to stop. "Who did this" and "is this
// somebody declaring what is in the slot" are different questions with
// different answers on the same call, so they get different types and cannot
// be passed to each other by mistake.
//
// A TYPE RATHER THAN THE STRING so the mapping to the wire lives in one place
// and a caller cannot invent a third value. It flattens a person to
// AuditActorUI, which is what every human door already resolves to
// (www.resolveActor), so no named operator is lost that reached the wire
// before.
//
// The zero value is DeclaredByLifecycle: a struct literal that forgets this
// still gets the answer that leaves the slot alone, for the reason spelled out
// on IsLifecycleActor. That is a floor, not a default — the parameter is
// required at every call site so the compiler asks the question.
type Declarer uint8

const (
	// DeclaredByLifecycle: Core's own bookkeeping ended a carrier's generation
	// — a produce finalize, a release, a dispatch claim. The Edge does not bind
	// an empty slot from one of these.
	DeclaredByLifecycle Declarer = iota
	// DeclaredByPerson: somebody at an admin door declared what this carrier
	// now holds. The Edge binds an empty slot from one of these, through the
	// repair path and its mid-swap guards.
	DeclaredByPerson
)

// Actor is the wire value for this declarer — the string that goes in
// UOPAdjustment.Actor and that IsLifecycleActor reads on the far side.
func (d Declarer) Actor() string {
	if d == DeclaredByPerson {
		return AuditActorUI
	}
	return ActorCoreLifecycle
}
