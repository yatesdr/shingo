package store

// Delegate file: the Core-owned bin-loader aggregate lives in store/loaders/.
// This preserves the *store.DB method surface so callers use store.Loader etc.
// and db.CreateLoader(...) without importing the sub-package.
//
// The read-cutover has happened: BuildLoaderInfos (the downward config sync) and
// BuildDemandRegistryFromAggregate (the threshold registry) both go through here.

import "shingocore/store/loaders"

// Type aliases so external callers use the store.* names.
type (
	Loader        = loaders.Loader
	LoaderHome    = loaders.Home
	LoaderPayload = loaders.Payload
	LoaderConfig  = loaders.Config
)

func (db *DB) CreateLoader(l loaders.Loader) (int64, error) { return loaders.CreateLoader(db.DB, l) }
func (db *DB) GetLoader(id int64) (*loaders.Loader, error)  { return loaders.GetLoader(db.DB, id) }
func (db *DB) GetLoaderByName(name, role string) (*loaders.Loader, error) {
	return loaders.GetLoaderByName(db.DB, name, role)
}
func (db *DB) ListLoaders() ([]loaders.Loader, error) { return loaders.ListLoaders(db.DB) }

// LoadersStagingAt names the active loaders that stage empties in this node.
// See loaders.LoadersStagingAt — it backs the one-owner-per-level refusal.
func (db *DB) LoadersStagingAt(nodeName string) ([]string, error) {
	return loaders.LoadersStagingAt(db.DB, nodeName)
}

func (db *DB) UpdateLoader(l loaders.Loader) error   { return loaders.UpdateLoader(db.DB, l) }
func (db *DB) DeleteLoader(id int64) error           { return loaders.DeleteLoader(db.DB, id) }
func (db *DB) UpsertLoaderHome(h loaders.Home) error { return loaders.UpsertHome(db.DB, h) }
func (db *DB) RemoveLoaderHome(loaderID, positionNodeID int64) error {
	return loaders.RemoveHome(db.DB, loaderID, positionNodeID)
}
func (db *DB) SetLoaderHomeOrder(loaderID int64, orderedNodeIDs []int64) error {
	return loaders.SetHomeOrder(db.DB, loaderID, orderedNodeIDs)
}
func (db *DB) ListLoaderHomes(loaderID int64) ([]loaders.Home, error) {
	return loaders.ListHomes(db.DB, loaderID)
}
func (db *DB) GetLoaderHomeByPositionNode(positionNodeID int64) (*loaders.Home, error) {
	return loaders.GetHomeByPositionNode(db.DB, positionNodeID)
}
func (db *DB) UpsertLoaderPayload(p loaders.Payload) error { return loaders.UpsertPayload(db.DB, p) }
func (db *DB) RemoveLoaderPayload(loaderID int64, payloadCode string) error {
	return loaders.RemovePayload(db.DB, loaderID, payloadCode)
}

// ListLoaderQuotas returns a loader's declared carrier mix.
func (db *DB) ListLoaderQuotas(loaderID int64) ([]loaders.Quota, error) {
	return loaders.ListQuotas(db.DB, loaderID)
}

// UpsertLoaderQuota sets how many carriers of one bin type a loader wants.
func (db *DB) UpsertLoaderQuota(q loaders.Quota) error { return loaders.UpsertQuota(db.DB, q) }

// RemoveLoaderQuota drops one line of a loader's carrier mix.
func (db *DB) RemoveLoaderQuota(loaderID, binTypeID int64) error {
	return loaders.RemoveQuota(db.DB, loaderID, binTypeID)
}

// ListLoaderHomeBinTypes returns each window's capability set, keyed by position
// node id. A window absent from the map takes anything.
func (db *DB) ListLoaderHomeBinTypes(loaderID int64) (map[int64][]string, error) {
	return loaders.ListHomeBinTypes(db.DB, loaderID)
}

// SetLoaderHomeBinTypes replaces what one window can physically take.
func (db *DB) SetLoaderHomeBinTypes(loaderID, positionNodeID int64, binTypeIDs []int64) error {
	return loaders.SetHomeBinTypes(db.DB, loaderID, positionNodeID, binTypeIDs)
}

func (db *DB) ListLoaderPayloads(loaderID int64) ([]loaders.Payload, error) {
	return loaders.ListPayloads(db.DB, loaderID)
}
func (db *DB) GetLoaderConfig(id int64) (*loaders.Config, error) { return loaders.GetConfig(db.DB, id) }

// LoaderMemberNodeNames maps a loader's member position node ids to node names.
func (db *DB) LoaderMemberNodeNames(loaderID int64) (map[int64]string, error) {
	return loaders.MemberNodeNames(db.DB, loaderID)
}
