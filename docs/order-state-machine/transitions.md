# Order state machine — bypasses and the lint guard

**This document does not contain the state machine.** It used to, and that is why
it was wrong.

Where the machine actually lives:

| What | Where |
|---|---|
| the statuses | `protocol/status.go` — the const block, and `AllStatuses()` |
| the transitions | `protocol/types.go` — `validTransitions` |
| terminality | derived: a status is terminal iff it has no outgoing edges |
| the status set predicates | `protocol/status.go` — `IsTerminal`, `IsVendorActive`, `IsVendorTracked`, `IsAcquiring`, `IsPreDispatch`, `IsStuckSweepCandidate`, `IsRuntimeStuckCandidate`, `IsOperatorVisible`, `BlocksChangeoverStart` |
| the side effects per edge | `shingo-core/dispatch/lifecycle.go` — `actionMap` |
| the typed writer methods | `shingo-core/dispatch/lifecycle.go` |
| a readable diagram | [`docs/order-lifecycle.md`](../order-lifecycle.md) |

## Why this document lost its tables

It carried hand-copied renderings of the status list, the transition table, the
action map and the lifecycle API. All four drifted, and the drift was not small:
two whole statuses missing, thirteen transitions, seven action-map rows, seven
methods, and one terminal status. It also stated that a compound parent has
"terminal-only exits" when `reshuffling → queued` is a live, action-bearing
resume edge with its own method.

That mattered more than an ordinary stale doc, because the `forbidigo` failure
message points developers here (`.golangci.yml`). Someone arriving to add a
carveout was reading a machine two feature-cycles behind the one they were
editing.

A table copied out of code has a half-life. The fix is not to re-copy it — it is
to stop. What remains below is the part that is only here.

## Reservations run alongside, not inside

An order's **reservations** — the soft holds on its source bins and destination
slots — have their own small lifecycle. Coupled to the order's status, not part
of it. Full mechanism in [reservations.md](../reservations.md).

| Reservation state | When |
|---|---|
| `pending` | written at plan time (`Acquire` / `AcquireSlot`) — the order holds the resource but has not shipped |
| `confirmed` | flipped at dispatch (`Confirm` / `ConfirmSlot`), in the same transaction that writes the hard claim |
| *(deleted)* | released when the order no longer needs the resource — at delivery, at terminal, or on a re-resolution that moved the source |

Reservation states are **not** order statuses and do not go through
`transition()`. Note the name collision: `reservations.State` has its own
`pending` and `confirmed`, and `domain.BinStatus` has its own `staged`. Most SQL
matching `'pending'` under `store/` is the reservation enum, not the order one.

The couplings worth knowing:

- A `sourcing` or `queued` order holds its sources as `pending` reservations and
  **reconciles** them each scanner tick — keep held, release moved, acquire newly
  needed — rather than re-shopping from scratch. The `queued ↔ sourcing` edges
  are what make that retry possible.
- `ReleaseByOrder` fires from the terminal chokepoint, so no reservation outlives
  its owning order.
- `ReapOrphaned` is the owner-liveness backstop: a hold is reclaimed when its
  order is terminal or gone, **never on age**. A waiting order may legitimately
  hold for hours, which is why `queued` and `sourcing` are exempt from the
  stuck-order sweep.

## What bypasses `transition()`

Three things write an order's status without going through the state machine.
Described by what they are rather than by line number, because line numbers were
the other half of why this document rotted.

1. **The driver itself** — `dispatch/lifecycle.go`. `transition()` has to call the
   underlying store methods to do its job.
2. **`MarkPending`** — the initial write at intake. There is no source status to
   validate against. Its real product is the `order_history` row, since the INSERT
   has already set the column.
3. **The INSERT** — `orders.Create` binds the struct's `Status` field directly.

**Number three is the one to know about.** Movement is governed; *entry* is not.
There is no CHECK constraint on `orders.status` in either schema, `Scan`
deliberately does not validate on read, and the lint guard matches selector
expressions like `db.UpdateOrderStatus` — it cannot see `Status:` in a struct
literal. Three statuses are used at creation today. That is convention, not
enforcement.

Related: the two writes that bypass `transition()` are also the two with no
compare-and-swap. `UpdateStatus` writes by id alone. There is a recorded incident
— a stale scanner snapshot wrote `queued → sourcing` over a cancel and
resurrected a cancelled order — which is why the other paths CAS.

The authoritative carveout list is `.golangci.yml`, in the `exclusions.rules`
block. Read it there; it is maintained, and a copy here would not be.

## `forbidigo` configuration gotchas

Two implementation details worth keeping, because both fail *silently*:

- **The field is `pattern:`, not `p:`.** `golangci-lint v2.x`'s
  `ForbidigoPattern` struct uses `yaml:"p"` for output and
  `mapstructure:"pattern"` for input. Config parsing goes through mapstructure, so
  a `p:` key is ignored and the rule defaults to matching everything.
- **Patterns match the Go selector expression, not source text.** forbidigo matches
  the AST identifier (`db.UpdateOrderStatus`), not the call site including the open
  paren. A regex ending in `\(` never matches.

Text-based matching (no `analyze-types: true`) is sufficient only because
`*store.DB` is the sole definer of these methods in either module. It is blind to
raw SQL and to any receiver not named `db` — see the invariant sweep in
`dispatch/rule1_invariant_test.go`, which exists because of that blindness.
