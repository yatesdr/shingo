// Package domain holds the pure data types that describe the entities
// and shared response shapes flowing through shingo-edge: processes,
// process nodes, claims, runtime state, station configuration, edge
// orders + history, shift definitions, counter snapshots, lineside
// buckets, and the operator-station view shapes that the HMI renders.
//
// The package mirrors shingocore/domain's role on the core side: lift
// pure data types out of the store/ aggregate sub-packages so higher
// layers (engine, service, www) can depend on a persistence-free
// package. The store sub-packages retain the unprefixed names via
// type aliases (e.g. `type Process = domain.Process`), so the full
// processes.Process / orders.Order / stations.Station public API is
// unchanged and every existing call site compiles without edits.
//
// Stage 2A.2 (2026-04) created this package to drain the
// www-no-direct-store depguard ratchet. Edge handlers under
// shingo-edge/www/ used to import shingoedge/store/<aggregate>
// purely for type names (Process, Station, Order, Style, NodeTask,
// etc.) — no DB calls. With the aliases in place those handlers now
// reach only for the domain package, and the depguard rule that
// forbids store imports inside www/ becomes a permanent guardrail
// instead of a ratcheted exception list.
//
// Guidelines for this package:
//
//   - No imports from shingoedge/store or any of its sub-packages.
//   - No database/sql, no network, no I/O — only the std lib's value
//     types (time, encoding/json for pure-data parsing, etc.).
//   - Methods are allowed only when they operate on the type's own
//     fields and require no external state.
//
// What still belongs in the store sub-packages:
//
//   - Persistence helpers: scan functions, SELECT column lists,
//     internal struct types used only inside store/ (e.g. an arrival
//     instruction passed between cross-aggregate composition methods).
//   - Anything that imports database/sql or that depends on a
//     specific schema layout to be useful.
package domain
