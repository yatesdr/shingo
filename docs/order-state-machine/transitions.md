# Order State Machine — Transitions Reference

**Source of truth:** `protocol/types.go` `validTransitions` map.

This document is the human-readable rendering. The Go source is canonical;
if the two diverge, the source wins.

## Statuses

| Status | Terminal? | Description |
|--------|-----------|-------------|
| `pending` | no | Order received, not yet routed |
| `sourcing` | no | Locating a source bin or destination |
| `submitted` | no | Submitted to fleet (queued for fleet acknowledgement) |
| `queued` | no | Awaiting inventory or fleet capacity |
| `acknowledged` | no | Fleet has acknowledged the order |
| `dispatched` | no | Fleet order created, robot assignment pending |
| `in_transit` | no | Robot moving with the bin |
| `staged` | no | Robot dwelling at a staging node (complex orders) |
| `delivered` | no | Bin delivered to destination, awaiting confirmation |
| `reshuffling` | no | Compound parent — children executing reshuffle plan |
| `confirmed` | **yes** | Receipt confirmed by edge |
| `failed` | **yes** | Order failed (vendor error, structural error, etc.) |
| `cancelled` | **yes** | Order cancelled (operator, fleet, or system) |

Terminal status = no key in `validTransitions`. `IsTerminal(s)` derives
from this property — adding a new non-terminal status only requires
adding a row to `validTransitions` with at least one outgoing edge.

## Allowed transitions

| From | Allowed To | Notes |
|------|------------|-------|
| `pending` | `sourcing`, `submitted`, `queued`, `reshuffling`, `cancelled`, `failed` | `pending → queued` is a fast-path used by `fulfillment/scanner.go` when the bin is already known. `pending → reshuffling` is the compound parent entry. |
| `sourcing` | `queued`, `submitted`, `cancelled`, `failed` | |
| `submitted` | `acknowledged`, `queued`, `cancelled`, `failed` | |
| `queued` | `acknowledged`, `dispatched`, `in_transit`, `sourcing`, `cancelled`, `failed` | `queued → dispatched` is the immediate write after `fleet.CreateOrder` returns; `acknowledged` is reported asynchronously by the vendor. `queued → sourcing` supports the scanner's re-resolve path. |
| `acknowledged` | `dispatched`, `in_transit`, `sourcing`, `cancelled`, `failed` | `acknowledged → sourcing` supports `PrepareRedirect` (re-resolve to a new delivery node). |
| `dispatched` | `in_transit`, `delivered`, `sourcing`, `cancelled`, `failed` | `dispatched → sourcing` mirrors acknowledged for redirect. |
| `in_transit` | `delivered`, `staged`, `cancelled`, `failed` | |
| `staged` | `in_transit`, `delivered`, `cancelled`, `failed` | `staged → in_transit` is the multi-robot release path (complex orders). |
| `delivered` | `confirmed`, `cancelled`, `failed` | |
| `reshuffling` | `confirmed`, `cancelled`, `failed` | Compound parent terminal-only exits. The parent never enters in-flight states; children carry the bin claims. |
| `confirmed` | (none) | Terminal |
| `failed` | (none) | Terminal |
| `cancelled` | (none) | Terminal |

## Action map

For transitions with side effects, the action map (in
`shingo-core/dispatch/lifecycle.go`) registers actions that fire after
the status update is persisted.

Engine-side reactions (sending edge envelopes, creating return orders,
running completion logic) live in `engine/wiring*.go` as EventBus
subscribers. Actions in the lifecycle package emit the events those
subscribers consume.

| (From, To) | Actions | Engine reaction |
|------------|---------|-----------------|
| `(in_transit, delivered)` | `emitCompleted` | `handleOrderCompleted` applies bin arrival, sends edge update |
| `(staged, delivered)` | `emitCompleted` | (same) |
| `(dispatched, delivered)` | `emitCompleted` | (same) |
| `(delivered, confirmed)` | `emitCompleted` | (idempotent — completion already ran) |
| `(reshuffling, confirmed)` | `emitCompleted` | Compound parent unlock + cleanup |
| `(*, cancelled)` | `emitCancelled` | `engine/wiring.go` cancel subscriber sends cancel notification + maybe-creates return order |
| `(*, failed)` | `emitFailed` | `engine/wiring.go` fail subscriber sends error notification + maybe-creates return order |

`(*, cancelled)` and `(*, failed)` apply for every non-terminal `from`
status. The action map enumerates each pair explicitly so the table is
greppable.

## Public lifecycle API

`shingo-core/dispatch/lifecycle.go` exposes the typed-method facade:

| Method | Transition | Caller(s) |
|--------|------------|-----------|
| `CancelOrder(ord, stationID, reason)` | `* → cancelled` | Edge cancel, operator UI, dispatcher, engine |
| `ConfirmReceipt(ord, stationID, receiptType, finalCount)` | `delivered → confirmed` | Edge receipt processing |
| `Release(ord, actor)` | `staged → in_transit` | `Dispatcher.HandleOrderRelease` |
| `MarkInTransit(ord, robotID, actor)` | `* → in_transit` (via vendor mapping) | `engine/wiring_vendor_status.go` |
| `MarkStaged(ord, actor)` | `in_transit → staged` | `engine/wiring_vendor_status.go` |
| `MarkDelivered(ord, actor)` | `* → delivered` | `engine/wiring_vendor_status.go` |
| `Queue(ord, actor, reason)` | `pending|sourcing → queued` | `dispatch/dispatcher.go`, `fulfillment/scanner.go` |
| `MoveToSourcing(ord, actor, reason)` | `* → sourcing` | `dispatch/planning_service.go`, `lifecycle_service.go`'s `PrepareRedirect`, `compound.go`, `fulfillment/scanner.go` |
| `Dispatch(ord, vendorOrderID, actor)` | `queued → dispatched` | `dispatch/dispatcher.go`, `complex.go` |
| `Fail(ord, stationID, errorCode, detail)` | `* → failed` | Many paths (fleet error, dispatcher error, etc.) |
| `BeginReshuffle(ord, reason)` | `pending → reshuffling` | `Dispatcher.CreateCompoundOrder` |
| `MarkPending(ord, reason)` | (initial write) | Order intake — `Create*Order` methods only. Bypasses `transition()` validation since there's no source status. |

## Bypass paths

Three categories of code do NOT go through `lifecycle.transition()`:

1. **Initial-write Pending.** The `Create*Order` methods write
   `StatusPending` immediately after `db.CreateOrder` to record the
   "received" detail in the audit trail. This bypass is acceptable
   because no source status exists; the lifecycle has nothing to
   validate. Sites:
   `dispatch/lifecycle_service.go:98,117,174`,
   `dispatch/complex.go:63`,
   `engine/orders.go:59`.

2. **Recovery / reconciliation.** `engine/reconciliation_service.go:120`
   advances stuck orders from `delivered → confirmed` after a crash. It
   bypasses lifecycle because the lifecycle's `ConfirmReceipt` is
   idempotent on `CompletedAt` and the recovery scenario specifically
   handles the case where the previous run failed mid-way. Documented
   bypass — see the inline comment.

3. **Child compound failure intake.** `dispatch/compound.go:116,124,132`
   call `FailOrderAtomic` directly on a child order during the
   AdvanceCompoundOrder loop. These are intake-time validations
   (missing source/dest node) where the child has no prior state worth
   validating against — the failure is the intended initial behaviour.
   Documented bypass.

A future depguard or forbidigo rule should enforce that direct calls to
`db.UpdateOrderStatus`, `db.FailOrderAtomic`, and `db.CancelOrderAtomic`
are confined to:

- `shingo-core/dispatch/lifecycle.go` (driver implementation)
- `shingo-core/dispatch/lifecycle_service.go` (initial-write bypass)
- `shingo-core/engine/reconciliation_service.go` (recovery bypass)
- `shingo-core/dispatch/compound.go` (child-intake bypass)
- `shingo-core/store/orders.go` (the implementations themselves)

That ratchet rule is not yet wired into `.golangci.yml` because the
existing depguard config operates on imports, not function calls.
A `forbidigo` configuration is the next-step Phase 7 work.

## Test pattern

`protocol/protocol_test.go` covers the table-level invariants:

- `TestIsTerminal` — the three terminal statuses are reported terminal
- `TestIsTerminalDerivedFromTable` — every key is non-terminal
- `TestEveryKeyHasOutgoingEdge` — no empty edge slices
- `TestReshufflingTransitions` — compound parent edges
- `TestAllStatusesCovered` — `AllStatuses()` matches the table
- `TestValidForwardTransitions` / `TestInvalidBackwardTransitions` — spot-checks
- `TestTerminalStatesCannotTransition` — terminal states reject all targets
- `TestUnknownStatusNotValidTransition` — unknown statuses reject

Future work: an exhaustive (status × status) matrix test in
`shingo-core/dispatch/lifecycle_test.go` that calls every typed method
from every source status and asserts either success or
`IllegalTransition`. Not added in this branch.
