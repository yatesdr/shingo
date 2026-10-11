//go:build sim && !unix

package main

import "shingocore/engine"

// simWatch on a platform with no SIGUSR1: the scan-lock probe only. The rig
// runs Core in a Linux container (sim_watch_unix.go).
func simWatch(eng *engine.Engine) { eng.StartSimWatch() }
