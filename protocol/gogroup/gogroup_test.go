package gogroup

import (
	"testing"
	"time"
)

// rank is a lock rank of the test's own.
type rank struct{}

func (rank) Rank() int    { return 1 }
func (rank) Name() string { return "gogroup test" }

// stopped reports whether stop returns within a second.
func stopped(stop func()) bool {
	done := make(chan struct{})
	go func() {
		stop()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(time.Second):
		return false
	}
}

// TestGroup_StopWaitsForWhatRuns: Stop returns only once a spawned goroutine
// and a Run on the caller's goroutine have returned.
func TestGroup_StopWaitsForWhatRuns(t *testing.T) {
	t.Parallel()
	for _, how := range []string{"Spawn", "Run"} {
		var g Group[rank]
		in, out := make(chan struct{}), make(chan struct{})
		work := func() {
			close(in)
			<-out
		}
		if how == "Spawn" {
			g.Spawn(work)
		} else {
			go g.Run(work)
		}
		<-in
		done := make(chan struct{})
		go func() {
			g.Stop()
			close(done)
		}()
		select {
		case <-done:
			t.Fatalf("%s: Stop returned while the work was running", how)
		case <-time.After(50 * time.Millisecond):
		}
		close(out)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("%s: Stop did not return once the work had", how)
		}
	}
}

// TestGroup_NothingRunsOnceStopped: after Stop, Spawn starts nothing and Run
// runs nothing and says so.
func TestGroup_NothingRunsOnceStopped(t *testing.T) {
	t.Parallel()
	var g Group[rank]
	g.Stop()
	ran := make(chan string, 2)
	g.Spawn(func() { ran <- "Spawn" })
	if g.Run(func() { ran <- "Run" }) {
		t.Error("Run on a stopped group returned true")
	}
	time.Sleep(20 * time.Millisecond)
	select {
	case how := <-ran:
		t.Errorf("%s ran on a stopped group", how)
	default:
	}
	if !stopped(g.Stop) {
		t.Error("a second Stop did not return")
	}
}

// TestGroup_APanicInRunStillLeaves: a Run whose work panics is no longer
// waited for: the panic goes on up the caller's stack, and Stop returns.
func TestGroup_APanicInRunStillLeaves(t *testing.T) {
	t.Parallel()
	var g Group[rank]
	func() {
		defer func() { _ = recover() }()
		g.Run(func() { panic("the work") })
	}()
	if !stopped(g.Stop) {
		t.Error("Stop waited on a Run whose work panicked")
	}
}
