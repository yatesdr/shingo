// Package changeover holds the pure data shapes for the changeover order
// plan. The engine package builds a Plan from a set of node diffs and then
// applies it (creating orders, linking node tasks). Keeping the shapes in
// their own package lets the planner be exercised in tests without spinning
// up an Engine.
package changeover

import (
	"shingo/protocol"
	"shingoedge/domain"
)

// ComplexOrderSpec describes a single complex order to be created.
//
// ProcessNode is the line node the order belongs to (the node whose claim
// drove the plan). Distinct from DeliveryNode for swap orders that drop at
// a supermarket but conceptually "live" at the line. Threaded through to
// ComplexOrderRequest.ProcessNode so Core can pick the line bin for
// order.BinID and target the right bin at release-time fallback.
type ComplexOrderSpec struct {
	DeliveryNode string
	ProcessNode  string
	Steps        []protocol.ComplexOrderStep
	AutoConfirm  bool
	PayloadCode  string // when non-empty, overrides lookupPayloadMeta
}

// RetrieveOrderSpec describes a fallback retrieve order.
type RetrieveOrderSpec struct {
	RetrieveEmpty bool
	DeliveryNode  string
	// SourceNode names the supermarket node group Core should pull from.
	// Empty falls back to Core's global FIFO scan (legacy behaviour).
	// Changeover specs populate this from diff.ToClaim.InboundSource so
	// fallback retrieves honour the configured supermarket — same fix as
	// the bin_loader retrieve plumbing in orders/manager.go.
	SourceNode  string
	StagingNode string
	LoadType    string
	PayloadCode string
	AutoConfirm bool
}

// OrderSpec is one of Complex / Retrieve. Exactly one field is set.
type OrderSpec struct {
	Complex  *ComplexOrderSpec
	Retrieve *RetrieveOrderSpec
}

// NodeAction is the per-node plan: zero, one, or two orders to create plus
// the post-creation node-task state. If Err is non-nil the applier records
// the failure (and sets the node task to "error") without creating orders.
//
// SupplyOrder and EvacOrder are positional, not robot identities. A drop
// fills only EvacOrder (single robot, evac steps with a staged-wait gate).
// A swap fills both (resupply + removal). The names describe what the
// order does, not which "side" of a two-robot pair it belongs to.
type NodeAction struct {
	NodeID       int64
	NodeName     string
	Situation    string
	SupplyOrder  *OrderSpec // "next material" — staging / delivery order. nil for drops.
	EvacOrder    *OrderSpec // "old material release" — swap/evac/release order. nil for adds.
	NextState    domain.NodeTaskState // node-task state to set on success
	LogTag       string     // short tag used in success log line
	Err          error      // pre-flight validation failure (planning-time)
}

// Plan is the full set of per-node actions for a changeover.
type Plan struct {
	Actions []NodeAction
}
