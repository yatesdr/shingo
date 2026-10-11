//go:build lockcheck || sim

package lockcheck

import (
	"strings"
	"testing"
)

type low struct{}

func (low) Rank() int    { return 10 }
func (low) Name() string { return "low" }

type high struct{}

func (high) Rank() int    { return 20 }
func (high) Name() string { return "high" }

// catch runs fn under Catch.
func catch(t *testing.T, fn func()) string {
	t.Helper()
	return Catch(fn)
}

func TestGuard_InOrderPasses(t *testing.T) {
	var a Mutex[low]
	var b RWMutex[high]
	rep := catch(t, func() {
		a.Lock()
		b.RLock()
		b.RUnlock()
		b.Lock()
		b.Unlock()
		a.Unlock()
	})
	if rep != "" {
		t.Fatalf("an in-order take failed:\n%s", rep)
	}
}

func TestGuard_ReentryFailsWithBothStacks(t *testing.T) {
	var a Mutex[low]
	a.Lock()
	defer a.Unlock()
	rep := catch(t, func() { a.Lock() })
	for _, want := range []string{"re-entry", "taking low (rank 10) while holding low (rank 10)",
		"--- low was taken here:", "--- low is being taken here:", "TestGuard_ReentryFailsWithBothStacks"} {
		if !strings.Contains(rep, want) {
			t.Errorf("report lacks %q:\n%s", want, rep)
		}
	}
}

func TestGuard_ReadReentryFails(t *testing.T) {
	var a RWMutex[low]
	a.RLock()
	defer a.RUnlock()
	if rep := catch(t, func() { a.RLock() }); !strings.Contains(rep, "re-entry") {
		t.Fatalf("a second read lock on one goroutine passed:\n%s", rep)
	}
}

func TestGuard_BackwardsFails(t *testing.T) {
	var a Mutex[low]
	var b Mutex[high]
	b.Lock()
	defer b.Unlock()
	rep := catch(t, func() { a.Lock() })
	if !strings.Contains(rep, "order taken backwards") || !strings.Contains(rep, "taking low (rank 10) while holding high (rank 20)") {
		t.Fatalf("a backwards take was not named:\n%s", rep)
	}
}

func TestGuard_TwoOfOneRankFail(t *testing.T) {
	var a, b Mutex[low]
	a.Lock()
	defer a.Unlock()
	if rep := catch(t, func() { b.Lock() }); !strings.Contains(rep, "order taken backwards") {
		t.Fatalf("two locks of one rank held at once passed:\n%s", rep)
	}
}

func TestGuard_OtherGoroutinesDoNotCount(t *testing.T) {
	var a Mutex[low]
	var b Mutex[high]
	b.Lock()
	done := make(chan string)
	go func() { done <- catch(t, func() { a.Lock(); a.Unlock() }) }()
	if rep := <-done; rep != "" {
		t.Fatalf("a lock held on another goroutine counted:\n%s", rep)
	}
	b.Unlock()
}

func TestGuard_UnlockOnAnotherGoroutineForgetsTheHold(t *testing.T) {
	var a Mutex[low]
	a.Lock()
	done := make(chan struct{})
	go func() { a.Unlock(); close(done) }()
	<-done
	if rep := catch(t, func() { a.Lock(); a.Unlock() }); rep != "" {
		t.Fatalf("a lock released elsewhere still counted as held:\n%s", rep)
	}
}

func TestGuard_TryLockIsHeld(t *testing.T) {
	var a Mutex[high]
	var b Mutex[low]
	if !a.TryLock() {
		t.Fatal("TryLock on a free lock failed")
	}
	defer a.Unlock()
	if rep := catch(t, func() { b.Lock() }); !strings.Contains(rep, "order taken backwards") {
		t.Fatalf("a lock taken by TryLock did not count:\n%s", rep)
	}
}

type keyed struct{}

func (keyed) Rank() int    { return 15 }
func (keyed) Name() string { return "keyed" }
func (keyed) Nests() bool  { return true }

func TestGuard_ANestingClassHoldsInstancesTogetherButNotOneTwice(t *testing.T) {
	var a, b Mutex[keyed]
	a.Lock()
	defer a.Unlock()
	if rep := catch(t, func() { b.Lock(); b.Unlock() }); rep != "" {
		t.Fatalf("two instances of a nesting class failed:\n%s", rep)
	}
	if rep := catch(t, func() { a.Lock() }); !strings.Contains(rep, "re-entry") {
		t.Fatalf("a second take of one instance passed:\n%s", rep)
	}
}
