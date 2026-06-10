//go:build !sim

package main

import (
	"context"
	"fmt"
	"log"

	"shingocore/config"
	"shingocore/fleet"
)

// simGuard in a non-sim build: sim.enabled is a misconfiguration — a production
// binary physically contains no sim code, so refuse loudly rather than silently
// ignoring the flag (brief D6).
func simGuard() {
	log.Fatal("[sim] sim.enabled=true but this binary was built without -tags sim; rebuild with -tags sim to use simulation mode")
}

// newSimBackend exists only so main.go compiles without the sim tag. It is
// never reached at runtime: main calls simGuard() first when cfg.Sim.Enabled,
// and simGuard fatals in the !sim build. Returning an error keeps the seam
// honest in the impossible case that ordering ever changes.
func newSimBackend(ctx context.Context, cfg *config.Config) (fleet.TrackingBackend, error) {
	return nil, fmt.Errorf("this binary was built without sim support; rebuild with -tags sim")
}
