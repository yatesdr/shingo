//go:build sim && unix

package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"shingocore/engine"
)

// simWatch starts the engine's scan-lock probe and dumps every goroutine to
// /tmp/core-goroutines.txt on SIGUSR1, without stopping: the rig's watchdog
// sends it when Core goes quiet (STANDARD rev 3), copies the file out, then
// reads Postgres's locks while Core is still in the state that froze it.
func simWatch(eng *engine.Engine) {
	eng.StartSimWatch()
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGUSR1)
	go func() {
		for range sig {
			path, err := engine.DumpGoroutines(os.TempDir())
			if err != nil {
				log.Printf("[sim-watch] goroutine dump: %v", err)
				continue
			}
			log.Printf("[sim-watch] goroutine dump written to %s", path)
		}
	}()
}
