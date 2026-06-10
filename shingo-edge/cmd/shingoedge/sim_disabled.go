//go:build !sim

package main

import (
	"log"

	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/plc"
)

// simGuard in a non-sim build: sim.enabled is a misconfiguration — a production
// binary physically contains no sim code, so refuse loudly rather than silently
// ignoring the flag (brief D6).
func simGuard() {
	log.Fatal("[sim] sim.enabled=true but this binary was built without -tags sim; rebuild with -tags sim to use simulation mode")
}

// startSimSubsystems is never reached in a !sim build (simGuard fatals first
// when cfg.Sim.Enabled). It exists only so main.go compiles without the tag.
func startSimSubsystems(eng *engine.Engine, cfg *config.Config, _ plc.WarlinkClient) {}

// simWarlinkClient returns nil in a !sim build, so engine.Config.Warlink stays
// nil and the PLC manager builds the real HTTP client. Defined here only so
// main.go compiles without the sim tag.
func simWarlinkClient(cfg *config.Config) plc.WarlinkClient { return nil }
