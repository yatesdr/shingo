package store

// Delegate for the plant-footprint cross-aggregate read (plan §15.D). Keeps
// the *store.DB method surface consistent with the other store sub-packages.

import (
	"time"

	"shingocore/store/audit"
	"shingocore/store/footprint"
)

// GetFootprint returns the plant-footprint summary (cells/bins managed +
// load/unload velocity series). loc is the plant timezone used to key the
// daily velocity buckets. The audit op sets are supplied here (the outer store
// layer owns cross-aggregate orchestration) so the footprint sub-package
// doesn't import store/audit directly (store-sub-pkg-isolation).
func (db *DB) GetFootprint(loc *time.Location) (*footprint.Footprint, error) {
	return footprint.Get(db.DB, loc, audit.OpSetForProduction, audit.ReleaseFamilyOps)
}
