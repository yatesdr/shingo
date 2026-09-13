# engine_db_methods.go — Residual passthroughs

**Count: 0**, and it is pinned — `frozenPassthroughCount = 0` in
`engine/engine_db_methods_freeze_test.go`, with equality the only passing
state. `engine/engine_db_methods.go` holds nothing but its header comment.

Every www-facing DB operation reaches `*store.DB` through a dedicated service
under `service/`. **The list of those services is `engine/engine_accessors.go`,
not this file** — each is one `func (e *Engine) XService() *service.X` accessor.
A table here would be a second copy of that list with nothing to keep it honest,
which is how the version of this section that named a `service.DemandService`
survived the rename to `DemandEpisodeService`.

Handlers take those accessors through `ServiceAccess` (CRUD) or
`EngineOrchestration` (orchestration verbs) in `www/engine_iface.go`. The single
`EngineAccess` interface this document was written against was split into those
two on 2026-04-25 (Phase 6.5), and their widths are pinned by
`www/engine_iface_width_test.go`.

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
