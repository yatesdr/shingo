//go:build lockcheck || sim

package lockcheck

import (
	"bytes"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
		through(b.RLock, b.RUnlock)
		through(b.Lock, b.Unlock)
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
	go func() { done <- catch(t, func() { through(a.Lock, a.Unlock) }) }()
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
	if rep := catch(t, func() { through(a.Lock, a.Unlock) }); rep != "" {
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
	if rep := catch(t, func() { through(b.Lock, b.Unlock) }); rep != "" {
		t.Fatalf("two instances of a nesting class failed:\n%s", rep)
	}
	if rep := catch(t, func() { a.Lock() }); !strings.Contains(rep, "re-entry") {
		t.Fatalf("a second take of one instance passed:\n%s", rep)
	}
}

// A hold a goroutine leaves in the book when it exits (a lock it took and
// another goroutine releases later, or never) stays there under its id. The
// runtime reuses an exited goroutine's g for later goroutines, so an id taken
// from the g would hand that hold to whichever goroutine runs on it next. On
// one P the runtime hands out the g freed last first, and each later goroutine
// here starts after the one before it exited, so they run on the holder's g.
// Serial: it sets GOMAXPROCS, which every goroutine in the process runs under.
func TestGuard_AHoldLeftByAnExitedGoroutineBindsNoLaterOne(t *testing.T) {
	defer runtime.GOMAXPROCS(runtime.GOMAXPROCS(1))
	var a Mutex[high]
	var b Mutex[low]
	took := make(chan struct{})
	go func() { a.Lock(); close(took) }()
	<-took
	for i := range 256 {
		done := make(chan string)
		go func() { done <- Catch(func() { through(b.Lock, b.Unlock) }) }()
		if rep := <-done; rep != "" {
			t.Fatalf("goroutine %d started after the holder exited inherited its hold:\n%s", i, rep)
		}
	}
	a.Unlock()
	if rep := Catch(func() { through(a.Lock, a.Unlock) }); rep != "" {
		t.Fatalf("the exited goroutine's hold outlived its release:\n%s", rep)
	}
}

// TestGoid_IsTheRuntimesGoroutineID pins what every caller of goid relies on
// (guard.go: the book of held locks, fail, Catch). It names the calling
// goroutine; one goroutine gets the same id for its whole life; no two
// goroutines alive at once share one; and it is the id runtime.Stack prints,
// which the runtime never gives a later goroutine, so a hold a goroutine leaves
// in the book when it exits binds no other. Hundreds of goroutines are alive at
// once: a read that mixed them up, or that two of them share, fails here. Run
// it with -race as well.
func TestGoid_IsTheRuntimesGoroutineID(t *testing.T) {
	t.Parallel()
	const n = 512
	type seen struct {
		want uint64
		err  error
		got  [4]uint64
	}
	all := make([]seen, n)
	var started, done sync.WaitGroup
	release := make(chan struct{})
	started.Add(n)
	done.Add(n)
	for i := range all {
		go func(s *seen) {
			defer done.Done()
			s.got[0] = goid()
			started.Done()
			// None of the n exits before every one has started, so all n are
			// alive at once from here on.
			<-release
			s.got[1] = goid()
			runtime.Gosched()
			s.got[2] = goid()
			s.want, s.err = stackID()
			s.got[3] = goid()
		}(&all[i])
	}
	started.Wait()
	close(release)
	done.Wait()

	owner := map[uint64]int{}
	for i, s := range all {
		if s.err != nil {
			t.Fatalf("goroutine %d: %v", i, s.err)
		}
		for k, g := range s.got {
			if g != s.want {
				t.Errorf("goroutine %d: read %d of goid was %d, runtime.Stack says %d", i, k, g, s.want)
			}
		}
		if j, dup := owner[s.got[0]]; dup {
			t.Errorf("goroutines %d and %d, alive at once, both read id %d", j, i, s.got[0])
		}
		owner[s.got[0]] = i
	}
	want, err := stackID()
	if err != nil {
		t.Fatal(err)
	}
	if g := goid(); g != want {
		t.Errorf("the test's own goroutine read %d, runtime.Stack says %d", g, want)
	}
}

// stackID is the calling goroutine's id as runtime.Stack prints it in the
// header of its own trace ("goroutine 18 [running]:"), parsed here
// independently of goid.
func stackID() (uint64, error) {
	buf := make([]byte, 128)
	buf = buf[:runtime.Stack(buf, false)]
	rest, ok := bytes.CutPrefix(buf, []byte("goroutine "))
	if !ok {
		return 0, fmt.Errorf("runtime.Stack header %q does not start with \"goroutine \"", buf)
	}
	num, _, ok := bytes.Cut(rest, []byte(" "))
	if !ok {
		return 0, fmt.Errorf("runtime.Stack header %q has no space after the id", buf)
	}
	id, err := strconv.ParseUint(string(num), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("runtime.Stack header %q: %w", buf, err)
	}
	return id, nil
}

// held counts critical sections entered, so a take-and-release the guard is
// asked about has a body (staticcheck SA2001) and the tests say what they do.
var held atomic.Int64

// through takes a lock, holds it for one statement, and releases it.
func through(lock, unlock func()) {
	lock()
	held.Add(1)
	unlock()
}
