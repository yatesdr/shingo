//go:build lockcheck || sim

package dispatch

import (
	"strings"
	"testing"

	"shingo/protocol/lockcheck"
)

// TestLoaderLock_OnePerLoaderAndNoSecondUnderOne ports
// TestWithLoaderBudget_ASecondLoaderLockUnderOneIsRefused and the per-loader
// half of TestUnloaderSweep_LockHeldAtDecisionPerLoader: each loader has one
// decision lock, a held lock on one loader leaves another's free, and a second
// loader's lock taken under one is refused by the lock order (two loaders' locks
// in no fixed order are two goroutines away from waiting on each other).
func TestLoaderLock_OnePerLoaderAndNoSecondUnderOne(t *testing.T) {
	t.Parallel()
	d := &Dispatcher{}
	a := d.loaderLock(1)
	if d.loaderLock(1) != a {
		t.Fatal("two locks for one loader: two asks at it would not wait on each other")
	}
	a.Lock()
	defer a.Unlock()
	free := make(chan bool)
	go func() {
		b := d.loaderLock(2)
		ok := b.TryLock()
		if ok {
			b.Unlock()
		}
		free <- ok
	}()
	if !<-free {
		t.Error("loader 2's lock is held while loader 1 decides; a slow decision at one loader would stall another")
	}
	decided := false
	report := lockcheck.Catch(func() {
		b := d.loaderLock(2)
		b.Lock()
		decided = true
		b.Unlock()
	})
	if decided {
		t.Error("a decision ran under two loaders' locks")
	}
	if !strings.Contains(report, "order taken backwards") || !strings.Contains(report, "loaderLock") {
		t.Fatalf("a second loader's lock under one was not refused:\n%s", report)
	}
}
