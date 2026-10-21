package plc

import (
	"shingoedge/config"
	"shingoedge/store"
)

// newTestManager builds a manager from a whole config the way the engine does
// at boot: the WarLink settings and the counter loop's boot-only values are
// taken by value, and the manager never sees the live *config.Config. A nil
// cfg means defaults, for tests that only want the plumbing.
func newTestManager(db *store.DB, cfg *config.Config, emitter EventEmitter, wl WarlinkClient) *Manager {
	if cfg == nil {
		cfg = config.Defaults()
	}
	return NewManager(db, cfg.WarLink, cfg.PollRate, cfg.Counter.JumpThreshold, emitter, wl)
}
