package store

// Delegate file: the Device Config pull history lives in
// store/deviceconfig/ (package deviceconfigstore — the store-side name;
// the domain types are the top-level shingocore/deviceconfig package the
// matrix is computed in). The pull handler reaches this through
// service.DeviceConfigService.

import (
	"context"
	"encoding/json"

	deviceconfigstore "shingocore/store/deviceconfig"
)

// SaveDeviceConfigPull appends one robot's pull row. params and config must
// already be sanitized (credential values fingerprinted) — the store trusts
// its one caller, the pull handler, on that.
func (db *DB) SaveDeviceConfigPull(ctx context.Context, p deviceconfigstore.Pull, config json.RawMessage) error {
	return deviceconfigstore.SavePull(ctx, db.DB, p, config)
}

// LatestDeviceConfigPulls reads the newest pull row per robot, ordered by
// vehicle. The whole-model config column is deliberately not selected.
func (db *DB) LatestDeviceConfigPulls(ctx context.Context) ([]deviceconfigstore.Pull, error) {
	return deviceconfigstore.LatestPulls(ctx, db.DB)
}
