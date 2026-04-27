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

The following code paths legitimately do NOT route through
`lifecycle.transition()`. Each is enforced by a `forbidigo` carveout
in `.golangci.yml`. Adding a new bypass requires a PR-reviewed
carveout entry alongside an inline comment explaining the reason.

### Core (shingo-core)

1. **Driver implementation.** `shingo-core/dispatch/lifecycle.go` —
   `transition()` is the state machine; it must call the underlying
   `db.UpdateOrderStatus` / `FailOrderAtomic` / `CancelOrderAtomic`
   methods to do its job.

2. **Initial-write Pending in the order-intake methods.**
   `shingo-core/dispatch/lifecycle_service.go`'s `CreateInboundOrder`,
   `CreateStorageWaybillOrder`, and `CreateIngestStoreOrder` write
   `StatusPending` immediately after `db.CreateOrder`. No source
   status exists; the lifecycle has nothing to validate. Sites:
   `dispatch/lifecycle_service.go:98,117,174`.

   Other order-intake call sites (`dispatch/complex.go`,
   `engine/orders.go`'s `CreateDirectOrder`) now use
   `lifecycle.MarkPending(ord, reason)` instead — `MarkPending` is
   itself a bypass internal to `lifecycle.go` (no source status to
   validate), but it keeps the intake call sites greppable as
   lifecycle calls and removes them from the carveout list.

3. **Child compound failure intake.**
   `shingo-core/dispatch/compound.go:116,124,132` call
   `FailOrderAtomic` directly on a child order during the
   `AdvanceCompoundOrder` loop. These are intake-time validations
   (missing source/dest node) where the child has no prior state
   worth validating against — the failure is the intended initial
   behaviour.

4. **Recovery / reconciliation.**
   `shingo-core/engine/reconciliation_service.go:120` advances stuck
   orders from `delivered → confirmed` after a crash. It bypasses
   lifecycle because the lifecycle's `ConfirmReceipt` is idempotent
   on `CompletedAt` and the recovery scenario specifically handles
   the case where the previous run failed mid-way.

5. **Service-layer passthrough wrappers.**
   `shingo-core/service/order_service.go:43,169` are direct
   passthroughs around `db.UpdateOrderStatus` and
   `db.FailOrderAtomic`. Slated for removal — callers should go
   direct to lifecycle. Carveout removable once the file is deleted.

6. **Store implementation.** `shingo-core/store/orders.go` is where
   the methods themselves live.

### Edge (shingo-edge)

7. **Edge-side lifecycle driver.**
   `shingo-edge/orders/lifecycle_service.go` — edge's equivalent of
   the core driver. `TransitionOrder` validates against
   `protocol.IsValidTransition` and writes through to the edge
   store. Same role as `shingo-core/dispatch/lifecycle.go`. A
   parallel typed-method migration on the edge side is a follow-up
   RFC; for now this file is the single bypass.

8. **Edge store implementation.** `shingo-edge/store/orders.go` is
   where edge's `UpdateOrderStatus` lives.

### Test files

`*_test.go` files build fixtures by direct DB writes. Permanent
carveout, no per-file entry needed.

### `forbidigo` configuration notes

The rule lives in `.golangci.yml`. Two implementation gotchas worth
recording for future config edits:

- **Field name is `pattern:`, not `p:`.** `golangci-lint v2.x`'s
  `ForbidigoPattern` struct has `yaml:"p"` for output and
  `mapstructure:"pattern"` for input. Config parsing uses
  mapstructure, so a `p:` key is silently ignored — the rule
  defaults to "match everything." Always use `pattern:`.
- **Patterns match the Go selector expression, not source text.**
  forbidigo matches against the AST identifier (e.g.
  `db.UpdateOrderStatus`), not the raw call site including the open
  paren. A regex ending in `\(` will never match its intended
  target. Drop the trailing `\(`.

The current rule uses text-based matching (no `analyze-types: true`)
which is sufficient because only `*store.DB` defines these methods
in either module — false-positive risk is bounded.

### Enforced via `forbidigo`

The `.golangci.yml` `forbidigo` rule prohibits direct calls to
`db.UpdateOrderStatus`, `db.FailOrderAtomic`, and
`db.CancelOrderAtomic` outside the carveout files listed above plus
the driver itself (`dispatch/lifecycle.go`) and the store
implementation (`store/orders.go`). Test files are permanently
exempted.

### Edge-side follow-up

`shingo-edge/orders/lifecycle_service.go`'s `TransitionOrder` is the
edge-side state-machine entry point. Edge already validates via
`protocol.IsValidTransition` (see `shingo-edge/orders/types.go:42`),
but a parallel `forbidigo` rule for edge — preventing direct
`db.UpdateOrderStatus` calls outside `TransitionOrder` and the
edge-side intake methods — is a follow-up RFC. It would land when
edge gets typed lifecycle methods (currently it has just the one
generic `TransitionOrder`).

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
