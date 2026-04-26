// Package domain holds the pure data types that describe the core
// entities flowing through shingo-core: bins, bin types, nodes, node
// types, payloads, orders, and their immediate children (manifests,
// node properties, order-bin junctions).
//
// Stage 2A of the architecture plan lifted these definitions out of the
// store/ aggregate sub-packages (bins, nodes, payloads, orders) so that
// higher layers — dispatch, engine, service, www — can depend on a
// persistence-free package. The store sub-packages retain the names via
// type aliases (e.g. `type Bin = domain.Bin`), so the full
// bins.Bin / nodes.Node / orders.Order public API is unchanged and every
// existing call site compiles without edits.
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
//
// If a new struct is persistence-specific (scan helpers, SELECT column
// lists, query filters), it belongs in the relevant store/<aggregate>
// sub-package, not here.
package domain
