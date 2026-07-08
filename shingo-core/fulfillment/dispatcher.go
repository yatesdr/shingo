package fulfillment

import (
	"shingocore/dispatch"
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

	// SecureStoreSlot atomically claims a queued store order's destination slot
	// (reserve → confirm under the NOT-EXISTS-bins seatbelt) before the fleet
	// dispatch, so a store never drops into a slot another store already owns
	// (#115/#117). The winner claimed it at intake; a store that lost the intake
	// race (or whose slot filled) gets a non-nil error here — the scanner then
	// requeues it, keeping its bin, and re-attempts on the next tick.
	SecureStoreSlot(order *orders.Order) error
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

// BinFinder is the narrow source-finding surface the scanner depends on — the
// ONE seam both intake planning and scanner replay route through, so the scanner
// can no longer drift its own inline finder from intake's tier scoping (the bug
// the collapse fixed). *dispatch.SourceFinder satisfies it structurally; the
// finder is pure (no claims/transitions), so the scanner keeps its own claim +
// dispatch orchestration and only asks the finder "where is the bin."
type BinFinder interface {
	FindSource(order *orders.Order, intent dispatch.Intent) dispatch.SourceResult
}

// Claimer is the reserve-then-claim primitive the scanner uses to claim a source
// bin (Acquire -> claim -> Confirm). A one-method consumer interface (matching
// Dispatcher/Lifecycle/BinFinder) so scanner_test.go can stub it without pulling
// in service; *service.BinManifestService satisfies it structurally.
type Claimer interface {
	ClaimForDispatch(binID, orderID int64, remainingUOP *int) error
}

// Compile-time checks that the concrete dispatch types satisfy the
// consumer-side interfaces. If dispatch drops or renames either
// method, the assertion catches it before a build failure elsewhere.
var (
	_ Dispatcher = (*dispatch.Dispatcher)(nil)
	_ BinFinder  = (*dispatch.SourceFinder)(nil)
	_ Lifecycle  = (*dispatch.LifecycleService)(nil)
)
