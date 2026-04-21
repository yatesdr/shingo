// engine_db_methods.go — thin delegating methods that forward to *store.DB.
//
// Phase 3a of the shingo-core/www refactor absorbed every handler-reachable
// DB passthrough into a dedicated service under service/ (see
// engine_accessors.go for the full list). No www/ handler calls this
// surface anymore; the internal engine/dispatch code that still needs
// these stores reaches through *store.DB directly, not via the Engine
// accessor.
//
// This file now holds zero passthroughs. It is kept in place so the
// `frozenPassthroughCount` freeze test and `docs/engine_db_methods_residual.md`
// stay discoverable by future greps; when the Phase 4 cleanup lands, both
// this file and the freeze test will be deleted outright.

package engine
