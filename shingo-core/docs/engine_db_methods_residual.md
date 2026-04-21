# engine_db_methods.go — Residual passthroughs

**Count:** 0

Phase 3a (PR 3a.6) drove the `func (e *Engine)` passthrough surface on
`engine/engine_db_methods.go` all the way to zero. Every www-facing DB
operation now reaches `*store.DB` exclusively through a dedicated
service under `service/`:

| Service                              | Responsibility                                             |
|--------------------------------------|------------------------------------------------------------|
| `service.BinService`                 | Bin CRUD, movement, lock / unlock, manifest loading, notes |
| `service.BinManifestService`         | Manifest edits (items, overrides, confirm / unconfirm)     |
| `service.OrderService`               | Order CRUD / queries, vendor + priority mutations, claims  |
| `service.NodeService`                | Node CRUD, group / lane / slot layout, scene + state reads |
| `service.DemandService`              | Demand registry CRUD + produced counters + production log  |
| `service.PayloadService`             | Payload templates, manifest items, bin-type + node links   |
| `service.MissionService`             | Mission telemetry / events / stats                         |
| `service.TestCommandService`         | Test-order command workflow                                 |
| `service.CMSTransactionService`      | CMS transaction listings                                    |
| `service.AuditService`               | Generic audit append + lookup                               |
| `service.InventoryService`           | Aggregated inventory view                                   |
| `service.AdminService`               | Admin-user login lookup                                     |
| `service.HealthService`              | Database ping                                               |

Handlers reach each service via the accessor methods on `*engine.Engine`
declared in `engine/engine_accessors.go` and surfaced through the
`EngineAccess` interface in `www/engine_iface.go`.

## Retained passthroughs

There are none. The body of `engine/engine_db_methods.go` is now empty
(only its header comment remains), and the freeze test
`engine/engine_db_methods_freeze_test.go` enforces
`const frozenPassthroughCount = 0` with strict equality.

The internal engine and dispatch code that still needs direct store
access does so by holding its own `*store.DB` reference (or reaching
`e.db` inside `engine/`), not via an `Engine.X(...)` accessor. That is
why methods like `GetNodeByDotName`, `UpdateOrderStatus`, and
`FailOrderAtomic` remain on `*store.DB` while their Engine-level
wrappers have been removed: the internal call paths were never part of
the `engine_db_methods.go` passthrough surface in the first place.

## Follow-up

When the Phase 4 dispatch refactor lands, both
`engine/engine_db_methods.go` and
`engine/engine_db_methods_freeze_test.go` can be deleted outright —
there is no surviving surface for either file to guard.
