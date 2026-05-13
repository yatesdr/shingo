// Phase 4 delegate: InventoryDeltaService moved to shingocore/uop.
// This file preserves the service package's existing surface via
// type aliases + a thin constructor wrapper so all existing callers
// (engine, messaging, www) continue to build unchanged.
//
// The canonical implementation is now at shingocore/uop/applier.go.

package service

import (
	"shingocore/store"
	"shingocore/uop"
)

// InventoryDeltaService is an alias for the canonical type in
// shingocore/uop. Re-exported so existing callers continue to work
// without import churn; new code should reference uop.InventoryDeltaService
// directly.
type InventoryDeltaService = uop.InventoryDeltaService

// BinUOPRow / LinesideBucketRow / InventoryInvariant: same alias
// re-exports for the value types the service returns.
type (
	BinUOPRow          = uop.BinUOPRow
	LinesideBucketRow  = uop.LinesideBucketRow
	InventoryInvariant = uop.InventoryInvariant
)

// ErrInventoryDeltaSkipped re-exports the canonical sentinel error
// from uop. Callers comparing via errors.Is keep working unchanged.
var ErrInventoryDeltaSkipped = uop.ErrInventoryDeltaSkipped

// NewInventoryDeltaService wraps uop.NewInventoryDeltaService.
// binManifest may be nil; *BinManifestService satisfies
// uop.ManifestClearer via its ClearForReuseTx method.
func NewInventoryDeltaService(db *store.DB, binManifest *BinManifestService) *InventoryDeltaService {
	var clearer uop.ManifestClearer
	if binManifest != nil {
		clearer = binManifest
	}
	return uop.NewInventoryDeltaService(db, clearer)
}
