// Package service holds shingo-edge's service layer — the canonical
// caller surface for handlers and engine business-logic code that
// reaches the persistence layer. Each service wraps *store.DB and
// exposes a coherent per-domain API.
//
// Edge took named-method delegates on *engine.Engine for Phase 4
// rather than the service-extraction route core took, because edge's
// domain is narrower — one production line, one process, one queue.
// Phase 6.1 introduced services for the three cross-aggregate
// coordinators (StationService, ChangeoverService); Phase 6.2′
// completed the extraction by adding services for every remaining
// single-aggregate domain previously sitting as a passthrough method
// on *engine.Engine. After 6.2′ the engine_db_methods.go file is
// deleted and EngineAccess collapses from ~93 methods to ~30.
//
// Service file layout (one per file, matches core's pattern):
//
//   admin_service.go       — admin user CRUD + auth lookup
//   catalog_service.go     — payload catalog sync from core
//   changeover_service.go  — process changeover orchestration
//   counter_service.go     — reporting points + counter snapshots + hourly counts
//   order_service.go       — order queries (lifecycle stays on orders.Manager)
//   process_service.go     — process + process_node + runtime CRUD
//   shift_service.go       — production shift CRUD
//   station_service.go     — operator station CRUD + cross-aggregate nodes/views
//   style_service.go       — style + style_node_claim CRUD
//
// Construction: Each service takes *store.DB. The *store.DB shim
// methods stay in place to keep test fixtures compiling; Phase 6.4
// migrates the test fixtures off those shims and retires them.
package service
