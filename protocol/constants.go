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
