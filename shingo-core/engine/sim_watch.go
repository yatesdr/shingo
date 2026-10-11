//go:build sim

package engine

import (
	"os"
	"path/filepath"
	"runtime/pprof"
	"time"
)

// sim_watch.go — what the rig's watchdog reads to tell a frozen Core
// (STANDARD rev 3), in sim builds only.
//
// A scan pass, an acquire and a door all hold the scan lock, and a frozen Core
// is one whose scan lock is never let go. Every 5 s a probe takes the lock with
// an empty body and says when it got it; a pass that logs nothing (no order is
// acquiring) still lets the probe in. No line for 30 wall-seconds: nothing has
// been able to scan.

// StartSimWatch starts the scan-lock probe. Its lines go to the engine log.
func (e *Engine) StartSimWatch() {
	if e.fulfillment == nil {
		return
	}
	go func() {
		t := time.NewTicker(5 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-e.stopChan:
				return
			case <-t.C:
				asked := time.Now()
				e.fulfillment.Door(func() {})
				e.logFn("[sim-watch] scan lock free (waited %s)", time.Since(asked).Round(time.Millisecond))
			}
		}
	}()
}

// DumpGoroutines writes every goroutine's stack to dir/core-goroutines.txt and
// returns the path.
func DumpGoroutines(dir string) (string, error) {
	path := filepath.Join(dir, "core-goroutines.txt")
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	return path, pprof.Lookup("goroutine").WriteTo(f, 2)
}
