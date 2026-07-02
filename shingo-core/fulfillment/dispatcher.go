package fulfillment

import (
	"shingo/protocol"
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Dispatcher is the narrow dispatch surface the scanner depends on.
//
// Declared consumer-side so *dispatch.Dispatcher satisfies it for
// free (structural). The scanner only exercises DispatchDirect on
// the happy path; narrowing the interface lets scanner_test.go
// stub dispatch with a one-method fake, which closes the coverage
// gap the old lines-14–31 scope note called out.
type Dispatcher interface {
	DispatchDirect(order *orders.Order, sourceNode, destNode *nodes.Node) (string, error)

	// DispatchPreparedComplex is the scanner-replay entrypoint for
	// complex orders queued via HandleComplexOrderRequest. The dispatcher
	// already has the resolved steps stored on the order (StepsJSON);
	// this call claims bins, transitions queued → sourcing → dispatched,
	// and ships blocks to the fleet. Phase 4b of bin-transit-state.
	//
	// On failure, the dispatcher transitions the order to terminal
	// `failed` (via lifecycle.Fail) and emits EventOrderFailed —
	// scanner doesn't need to do recovery here. The error return lets
	// the scanner log + skip; it's not actionable beyond that.
	DispatchPreparedComplex(order *orders.Order) error
}

// Lifecycle is the narrow lifecycle surface the scanner depends on.
//
// Declared consumer-side so *dispatch.LifecycleService satisfies it for
// free (structural). The scanner only invokes MoveToSourcing and Queue;
// declaring the interface at the call site lets scanner_test.go stub
// lifecycle with a two-method fake and keeps the dependency surface
// minimal.
type Lifecycle interface {
	MoveToSourcing(ord *orders.Order, actor, reason string) error
	Queue(ord *orders.Order, actor, reason string) error
}

// Resolver is the narrow resolver surface the scanner depends on.
//
// Signature mirrors binresolver.NodeResolver (exported via the
// dispatch.NodeResolver alias). Declared here rather than reusing
// the alias so scanner_test.go does not have to pull in dispatch
// to stub a fake resolver.
type Resolver interface {
	Resolve(syntheticNode *nodes.Node, orderType protocol.OrderType, payloadCode string, binTypeID *int64) (*binresolver.ResolveResult, error)
}

// Claimer is the reserve-then-claim primitive the scanner uses to claim a source
// bin (Acquire -> claim -> Confirm). A one-method consumer interface (matching
// Dispatcher/Lifecycle/Resolver) so scanner_test.go can stub it without pulling
// in service; *service.BinManifestService satisfies it structurally.
type Claimer interface {
	ClaimForDispatch(binID, orderID int64, remainingUOP *int) error
}

// Compile-time checks that the concrete dispatch types satisfy the
// consumer-side interfaces. If dispatch drops or renames either
// method, the assertion catches it before a build failure elsewhere.
var (
	_ Dispatcher = (*dispatch.Dispatcher)(nil)
	_ Resolver   = (*dispatch.DefaultResolver)(nil)
	_ Lifecycle  = (*dispatch.LifecycleService)(nil)
)
