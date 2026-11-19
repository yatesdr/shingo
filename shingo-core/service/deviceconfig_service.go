package service

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"shingocore/deviceconfig"
	"shingocore/store"
	deviceconfigstore "shingocore/store/deviceconfig"
)

// DeviceConfigService is the Device Config tab's persistence surface: a
// thin door over store/deviceconfig (the robothours/footprint pattern).
// Handlers reach it via the engine rather than importing store directly
// (www-no-direct-store depguard); the #1500 fetch itself goes through
// fleet.DeviceConfigReader, not here — the layers meet in the pull handler.
type DeviceConfigService struct {
	db *store.DB
}

func NewDeviceConfigService(db *store.DB) *DeviceConfigService {
	return &DeviceConfigService{db: db}
}

// SavePull appends one robot's pull: the flattened leaf rows (the matrix's
// read path) and the sanitized whole model (the v3 diff's artifact). Both
// arguments must already be sanitized — credential values fingerprinted —
// which the pull handler does before it calls this.
func (s *DeviceConfigService) SavePull(ctx context.Context, vehicle, modelMD5 string, pulledAt time.Time, leaves []deviceconfig.Leaf, config []byte) error {
	params, err := json.Marshal(leaves)
	if err != nil {
		return fmt.Errorf("deviceconfig service: marshal params: %w", err)
	}
	return s.db.SaveDeviceConfigPull(ctx, deviceconfigstore.Pull{
		Vehicle: vehicle, PulledAt: pulledAt, ModelMD5: modelMD5, Params: params,
	}, config)
}

// LatestPulls reads the newest pull per robot with its leaf rows decoded,
// keyed by vehicle — exactly the input deviceconfig.BuildMatrix takes.
func (s *DeviceConfigService) LatestPulls(ctx context.Context) (map[string]deviceconfig.Pull, error) {
	rows, err := s.db.LatestDeviceConfigPulls(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string]deviceconfig.Pull, len(rows))
	for _, r := range rows {
		p := deviceconfig.Pull{Vehicle: r.Vehicle, PulledAt: r.PulledAt}
		if len(r.Params) > 0 {
			if err := json.Unmarshal(r.Params, &p.Leaves); err != nil {
				return nil, fmt.Errorf("deviceconfig service: decode params for %s: %w", r.Vehicle, err)
			}
		}
		out[r.Vehicle] = p
	}
	return out, nil
}
