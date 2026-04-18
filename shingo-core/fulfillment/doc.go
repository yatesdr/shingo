// Package fulfillment contains the queued-order fulfillment scanner.
//
// The Scanner monitors orders sitting in the "queued" status and
// promotes them to "sourcing" when matching inventory becomes
// available. It runs in two modes:
//
//   - event-driven: callers invoke Trigger() from event handlers and
//     then RunOnce() from a goroutine — useful for reacting to
//     EventBinUpdated / EventOrderCompleted / EventOrderCancelled /
//     EventOrderFailed on the engine event bus;
//   - periodic sweep: StartPeriodicSweep kicks off a background
//     ticker as a safety net so orders are never stuck forever even
//     if a trigger is missed.
//
// Dependencies are supplied at construction:
//
//   - Store — the narrow DB surface declared in store.go, satisfied
//     by *store.DB (see compile-time assertion).
//   - *dispatch.Dispatcher and *dispatch.DefaultResolver — same
//     concrete types the engine layer uses; lane locking is shared.
//   - sendToEdge — callback for emitting protocol.OrderAck and
//     protocol.OrderWaybill back to edge stations.
//   - failFn — callback for the "structural error" path. This is
//     wired at construction to engine.failOrderAndEmit so the
//     standard EventOrderFailed handler chain (audit, return order,
//     edge notification) runs when an order is structurally
//     impossible to fulfill. If failFn is nil, the scanner falls back
//     to Store.FailOrderAtomic — older tests rely on that fallback
//     so the package preserves it.
//   - logFn / debugLog — pass-through loggers.
//
// The Scanner itself does not import the engine package; it depends
// only on a narrow Store interface, the dispatch package's concrete
// types, and the protocol package. Extracted from engine/ in Stage 7
// of the shingo-core refactor.
package fulfillment
