// Package changeover holds the pure data shapes for the changeover order
// plan. The engine package builds a Plan from a set of node diffs and then
// applies it (creating orders, linking node tasks). Keeping the shapes in
// their own package lets the planner be exercised in tests without spinning
// up an Engine.
package changeover

import (
	"shingo/protocol"
)

// ComplexOrderSpec describes a single complex order to be created.
type ComplexOrderSpec struct {
	DeliveryNode string
	Steps        []protocol.ComplexOrderStep
	AutoConfirm  bool
}

// RetrieveOrderSpec describes a fallback retrieve order.
type RetrieveOrderSpec struct {
	RetrieveEmpty bool
	DeliveryNode  string
	StagingNode   string
	LoadType      string
	PayloadCode   string
	AutoConfirm   bool
}

// OrderSpec is one of Complex / Retrieve. Exactly one field is set.
type OrderSpec struct {
	Complex  *ComplexOrderSpec
	Retrieve *RetrieveOrderSpec
}

// NodeAction is the per-node plan: zero, one, or two orders to create plus
// the post-creation node-task state. If Err is non-nil the applier records
// the failure (and sets the node task to "error") without creating orders.
type NodeAction struct {
	NodeID    int64
	NodeName  string
	Situation string
	OrderA    *OrderSpec // "next material" — typically the staging order
	OrderB    *OrderSpec // "old material release" — swap/evac/release
	NextState string     // node-task state to set on success
	LogTag    string     // short tag used in success log line
	Err       error      // pre-flight validation failure (planning-time)
}

// Plan is the full set of per-node actions for a changeover.
type Plan struct {
	Actions []NodeAction
}
