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

	// ReserveStorageDropoff node-drives the plain-path destination-slot reserve
	// (reserve-only) before the fleet dispatch: if the dropoff is a concrete
	// storage slot it is reserved so two plain orders never drop into the same
	// slot (#115/#117, generalized in Stage 3 from store to every plain family);
	// a no-op for lines/consume points. A non-nil error means the slot is not
	// (yet) ours — the scanner requeues, keeping the bin, and re-attempts next
	// tick. Owner-idempotent.
	ReserveStorageDropoff(order *orders.Order) error

	// PlanBuriedReshuffle plans the reshuffle compound for a source that resolved
	// BURIED on replay, making the order its own compound parent (→ reshuffling).
	//
	// This is on the scanner's surface because reshuffle planning cannot live at
	// intake alone: planTransport runs once, but a queued order's lane can be buried
	// by a later store while it waits, and the scanner is the only thing that looks
	// at the order again. The scanner must clear the dropoff gate first — the
	// compound carries the delivery leg, so planning one commits the delivery.
	//
	// A transient error (lane locked by another reshuffle) means requeue and retry;
	// anything else is structural and fails the order.
	PlanBuriedReshuffle(order *orders.Order, buried *dispatch.BuriedError) error

	// PostFindHook fires between the scanner's Find and Claim. A no-op in
	// production (nil hook); concurrency tests install one via
	// Dispatcher.SetPostFindHook to make a claim race deterministic. It lives on
	// this interface because the claim-move made the scanner the single claimer,
	// so the find→claim window it guards is here, not at intake.
	PostFindHook()
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
