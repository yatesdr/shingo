package www

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"shingoedge/domain"
)

// waitForWaiters blocks until the in-flight call for stationID has exactly want
// waiters registered, so the tests can sequence joiners against the leader
// without sleeping on a guess.
func waitForWaiters(t *testing.T, g *stationViewGroup, stationID int64, want int) {
	t.Helper()
	for i := 0; i < 400; i++ {
		g.mu.Lock()
		got := 0
		if c := g.calls[stationID]; c != nil {
			got = c.waiters
		}
		g.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("waiters for station %d never reached %d", stationID, want)
}

// Concurrent requests for the same station must share ONE build. This is the
// whole point of the group: a station view costs ~8 queries per tile and every
// DB read serialises on one connection, so N clients each building the same
// board is what wound Springfield's bin loader from 3.1s to 116s.
func TestStationViewGroup_CoalescesConcurrentBuilds(t *testing.T) {
	g := newStationViewGroup()
	var builds atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})

	build := func(ctx context.Context) (*domain.OperatorStationView, error) {
		if builds.Add(1) == 1 {
			close(started)
		}
		<-release
		return &domain.OperatorStationView{}, nil
	}

	const n = 5
	views := make([]*domain.OperatorStationView, n)
	errs := make([]error, n)
	var wg sync.WaitGroup

	wg.Add(1)
	go func() { defer wg.Done(); views[0], errs[0] = g.do(context.Background(), 7, build) }()
	<-started // leader has registered its call

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); views[i], errs[i] = g.do(context.Background(), 7, build) }(i)
	}
	waitForWaiters(t, g, 7, n)

	close(release)
	wg.Wait()

	if got := builds.Load(); got != 1 {
		t.Fatalf("build ran %d times, want exactly 1", got)
	}
	for i := range views {
		if errs[i] != nil {
			t.Fatalf("caller %d: unexpected error %v", i, errs[i])
		}
		if views[i] == nil {
			t.Fatalf("caller %d: nil view", i)
		}
		if views[i] != views[0] {
			t.Fatalf("caller %d got a different view pointer; callers should share one build", i)
		}
	}
	// The finished call must not linger, or the next request would join a
	// completed build instead of getting fresh data.
	g.mu.Lock()
	leftover := g.calls[7]
	g.mu.Unlock()
	if leftover != nil {
		t.Fatal("completed call was left registered")
	}
}

// One caller giving up must not kill a build the others are still waiting on.
// That is why the build runs on a context detached from any single request.
func TestStationViewGroup_CallerCancelDoesNotKillSharedBuild(t *testing.T) {
	g := newStationViewGroup()
	var builds atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	buildCtxErr := make(chan error, 1)

	build := func(ctx context.Context) (*domain.OperatorStationView, error) {
		builds.Add(1)
		close(started)
		<-release
		buildCtxErr <- ctx.Err()
		return &domain.OperatorStationView{}, nil
	}

	var leaderView *domain.OperatorStationView
	var leaderErr error
	var wg sync.WaitGroup
	wg.Add(1)
	go func() { defer wg.Done(); leaderView, leaderErr = g.do(context.Background(), 3, build) }()
	<-started

	quitCtx, quit := context.WithCancel(context.Background())
	joinerErr := make(chan error, 1)
	go func() {
		_, err := g.do(quitCtx, 3, build)
		joinerErr <- err
	}()
	waitForWaiters(t, g, 3, 2)

	quit() // the joiner's client disconnects
	select {
	case err := <-joinerErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("joiner error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("joiner did not return after its context was cancelled")
	}

	close(release)
	wg.Wait()

	if err := <-buildCtxErr; err != nil {
		t.Fatalf("shared build context was cancelled (%v) though the leader was still waiting", err)
	}
	if leaderErr != nil {
		t.Fatalf("leader error = %v, want nil", leaderErr)
	}
	if leaderView == nil {
		t.Fatal("leader got a nil view")
	}
	if got := builds.Load(); got != 1 {
		t.Fatalf("build ran %d times, want exactly 1", got)
	}
}

// When the last waiter goes away the build must be cancelled — it is holding
// the single DB connection, and nobody is left to receive the result.
func TestStationViewGroup_AbandonedBuildIsCancelled(t *testing.T) {
	g := newStationViewGroup()
	started := make(chan struct{})
	observed := make(chan error, 1)

	build := func(ctx context.Context) (*domain.OperatorStationView, error) {
		close(started)
		<-ctx.Done() // a real build notices this at its tile-loop boundary
		observed <- ctx.Err()
		return nil, ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := g.do(ctx, 11, build)
		done <- err
	}()
	<-started
	waitForWaiters(t, g, 11, 1)

	cancel() // the only client disconnects

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("do() error = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("do() did not return after the last waiter left")
	}
	select {
	case err := <-observed:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("build saw ctx.Err() = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("abandoned build was never cancelled — it would keep holding the DB connection")
	}
}
