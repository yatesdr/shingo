// Package domain holds the pure data types that describe the entities
// and shared response shapes flowing through shingo-core: bins, bin
// types, nodes, node types, payloads, orders, and their immediate
// children (manifests, node properties, order-bin junctions), plus
// audit entries, order history, scene points, edge-registry rows,
// telemetry events / missions / filters / stats, diagnostic test
// commands, and node-tile derived state.
//
// Stage 2A of the architecture plan lifted the entity definitions out
// of the store/ aggregate sub-packages (bins, nodes, payloads, orders)
// so that higher layers — dispatch, engine, service, www — can depend
// on a persistence-free package. The store sub-packages retain the
// names via type aliases (e.g. `type Bin = domain.Bin`), so the full
// bins.Bin / nodes.Node / orders.Order public API is unchanged and
// every existing call site compiles without edits.
//
// Stage 2A.2 (2026-04) extended the same pattern to non-entity data
// types referenced by the www handlers: audit.Entry, orders.History,
// scene.Point, registry.Edge, diagnostics.TestCommand, bins.NodeTileState,
// and the telemetry Event/Mission/Filter/Stats group. The original
// store sub-packages keep their unprefixed type names via aliases
// (`type Entry = domain.AuditEntry`, …), and the handlers now reach
// only for the domain package — which lets the depguard rule that
// forbids store imports inside www/ become a permanent guardrail
// instead of a ratcheted exception list.
//
// Phase 6.4b (2026-04) removed the outer `store.X` package-level
// aliases. Callers now reference the sub-package types directly
// (bins.Bin, nodes.Node, orders.Order, …).
//
// Guidelines for this package:
//
//   - No imports from shingocore/store or any of its sub-packages.
//   - No database/sql, no network, no I/O — only the std lib's value
//     types (time, encoding/json for pure-data parsing, etc.).
//   - Methods are allowed only when they operate on the type's own
//     fields and require no external state (e.g. Bin.ParseManifest,
//     which unmarshals the bin's own manifest string).
//   - Query filters live here when they cross the handler/service
//     boundary (TelemetryFilter is the working example). The
//     persistence layer interprets the filter into SQL but does not
//     own its definition — handlers populate the filter from request
//     parameters and pass it through the service interface.
//
// What still belongs in the store sub-packages:
//
//   - Persistence helpers: scan functions, SELECT column lists,
//     internal struct types used only inside store/ (e.g. an arrival
//     instruction passed between cross-aggregate composition methods).
//   - Anything that imports database/sql or that depends on a
//     specific schema layout to be useful.
package domain
