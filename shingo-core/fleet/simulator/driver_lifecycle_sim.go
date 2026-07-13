//go:build sim

package simulator

import (
	"context"
	"fmt"
	"math/rand"

	"shingo/shared/clock"
	"shingocore/config"
	"shingocore/fleet"
)

// Compile-time check: DriverStarter is only implemented in sim builds.
var _ fleet.DriverStarter = (*SimulatorBackend)(nil)

// typedDriver returns the typed Driver pointer stored in the backend's driver
// field. The field is `any` in the base struct so the package compiles without
// the sim tag; this accessor gives sim-tagged callers the typed pointer.
func (s *SimulatorBackend) typedDriver() *Driver {
	d, _ := s.driver.(*Driver)
	return d
}

// NewDriverFromConfig creates the sim order-lifecycle driver and stores it on
// the backend. The goroutine does NOT start yet — call StartDriver after the
// engine's event handlers are wired (Item 3, sim startup race).
func (s *SimulatorBackend) NewDriverFromConfig(cfg config.SimConfig, clk clock.Clock, rng *rand.Rand) {
	s.driver = NewDriver(s, cfg, clk, rng)
}

// StartDriver launches the driver goroutine. Call AFTER engine event wiring so
// FINISHED events reach live handlers. Implements fleet.DriverStarter.
func (s *SimulatorBackend) StartDriver(ctx context.Context) error {
	d := s.typedDriver()
	if d == nil {
		return fmt.Errorf("simulator: driver not created — call NewDriverFromConfig first")
	}
	go d.run(ctx)
	return nil
}
