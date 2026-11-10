# Order state machine — bypasses and the lint guard

Verified against: 51489c78

**The code is the state machine; this document is not.** It carries one table
copied out of the code — the transition table below — only because a test fails
when it disagrees with the code.

Where the machine actually lives:

| What | Where |
|---|---|
| the statuses | `protocol/status.go` — the const block, and `AllStatuses()` |
| the transitions | `protocol/types.go` — `validTransitions` |
| terminality | derived: a status is terminal iff it has no outgoing edges |
| the status set predicates | `protocol/status.go` — `IsTerminal`, `IsVendorActive`, `IsVendorTracked`, `IsAcquiring`, `IsWaiting`, `IsPreDispatch`, `IsStuckSweepCandidate`, `IsRuntimeStuckCandidate`, `IsFailureTerminal`, `BlocksChangeoverStart` |
| the side effects per edge | `shingo-core/dispatch/lifecycle.go` — `actionMap` |
| the typed writer methods | `shingo-core/dispatch/lifecycle.go` |
| a readable diagram | [`docs/order-lifecycle.md`](../order-lifecycle.md) |

## Why only one table

The `forbidigo` failure message points developers here (`.golangci.yml`), so a
hand copy of the machine that drifts sends them to a machine they are not
editing. The transition table below is the one copy, held to `validTransitions`
by `TestTransitionsDocTableMatchesTheCode` (an edge or a status added or removed
on either side fails the protocol module's tests); the action map and the method
list are read in `lifecycle.go`.

## The transition table

One row per status. `from` may move to each status in its row; a terminal
status has no outgoing edge. Pinned to `protocol/types.go` `validTransitions`
and `AllStatuses()`.

| from | to |
|---|---|
| `pending` | `submitted`, `dispatched`, `queued`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `sourcing` | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `queued` | `acknowledged`, `dispatched`, `in_transit`, `reshuffling`, `cancelled`, `failed`, `skipped` |
| `submitted` | `acknowledged`, `queued`, `cancelled`, `failed`, `skipped` |
| `dispatched` | `in_transit`, `staged`, `delivered`, `queued`, `cancelled`, `faulted`, `failed` |
| `acknowledged` | `dispatched`, `in_transit`, `staged`, `cancelled`, `faulted`, `failed` |
| `in_transit` | `delivered`, `staged`, `cancelled`, `faulted`, `failed` |
| `staged` | `in_transit`, `delivered`, `cancelled`, `faulted`, `failed` |
| `delivered` | `confirmed`, `cancelled`, `failed` |
| `faulted` | `in_transit`, `delivered`, `failed`, `cancelled` |
| `reshuffling` | `confirmed`, `pending`, `cancelled`, `failed` |
| `confirmed` | *(terminal)* |
| `failed` | *(terminal)* |
| `cancelled` | *(terminal)* |
| `skipped` | *(terminal)* |

`sourcing` is history: no edge enters it, and its row is `queued`'s, so an old
row that still says it is not terminal.

## Reservations run alongside, not inside

An order's **reservations** — the soft holds on its source bins and destination
slots — have their own lifecycle (`pending` → `confirmed` → deleted), coupled to
the order's status but not part of it, and they do not go through
`transition()`; the mechanism is [reservations.md](../reservations.md). Mind the
name collision: `reservations.State` has its own `pending` and `confirmed`, and
`domain.BinStatus` has its own `staged`. Most SQL matching `'pending'` under
`store/` is the reservation enum, not the order one.

## What bypasses `transition()`

Two things write an order's status without going through the state machine.

1. **The driver itself** — `dispatch/lifecycle.go`. `transition()` has to call the
   underlying store methods to do its job.
2. **The INSERT** — `orders.Create` binds the struct's `Status` field directly,
   and writes the creation `order_history` row in the same transaction
   (`IssueOrder` opens one for an order issued alone; a dig's legs and the
   home clear's ride their compound's).

**Number two is the one to know about.** Movement is governed; *entry* is not.
There is no CHECK constraint on `orders.status` in either schema, `Scan`
deliberately does not validate on read, and the lint guard matches selector
expressions like `db.UpdateOrderStatus` — it cannot see `Status:` in a struct
literal. One status is used at creation: `pending`, at every door. That is
convention, not enforcement.

Related: the plain `UpdateStatus` write has no compare-and-swap — it writes by id
alone, so a writer acting on a stale read can land over a concurrent cancel and
resurrect a cancelled order. The transition paths CAS (`UpdateStatusFrom`) for
that reason.

The authoritative carveout list is `.golangci.yml`, in the `exclusions.rules`
block. Read it there; it is maintained, and a copy here would not be.

## `forbidigo` configuration gotchas

Two implementation details worth keeping, because both fail *silently*:

- **The field is `pattern:`, not `p:`.** golangci-lint v2.x's forbidigo
  pattern struct uses `yaml:"p"` for output and `mapstructure:"pattern"` for
  input. Config parsing goes through mapstructure, so
  a `p:` key is ignored and the rule defaults to matching everything.
- **Patterns match the Go selector expression, not source text.** forbidigo matches
  the AST identifier (`db.UpdateOrderStatus`), not the call site including the open
  paren. A regex ending in `\(` never matches.

Text-based matching (no `analyze-types: true`) is sufficient only because
`*store.DB` is the sole definer of these methods in either module. It is blind to
raw SQL and to any receiver not named `db` — see the invariant sweep in
`engine/rule1_invariant_docker_test.go`, which exists because of that blindness.
