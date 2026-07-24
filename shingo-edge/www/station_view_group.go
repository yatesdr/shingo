package www

import (
	"context"
	"sync"

	"shingoedge/domain"
)

// stationViewGroup coalesces concurrent builds of the SAME operator-station
// view into one.
//
// Why this exists: Edge serialises every DB operation on a single connection
// (store.Open sets SetMaxOpenConns(1)), and one station view costs roughly 8
// queries per tile. Nothing stopped N clients from each starting their own
// build of the same station — a second kiosk tab, a rejoining browser, a
// postAction refresh, or (worst) a client that timed out and immediately
// retried while the abandoned build kept running. Those builds queue behind
// each other and behind the write stream, so each one gets slower and the
// queue never drains on its own. Springfield's 22-home bin-loader board
// measured 3.1s per build on a freshly restarted edge and 25-116s after a day
// of uptime, essentially all of it queueing.
//
// The browser-side guard (loadView in operator.js) caps one build per BOARD.
// This caps one build per STATION no matter how many clients ask, and it holds
// even for a client running a stale cached copy of that JS — which is the
// reason to enforce it server-side as well rather than trusting the page.
//
// Cancellation: the shared build runs on its own context, deliberately
// detached from whichever request happened to start it and cancelled only when
// the LAST waiter goes away. So a client that disconnects stops waiting
// immediately, a build still wanted by others survives, and a build nobody
// wants any more is abandoned — the point being that it is holding the one DB
// connection while it runs.
type stationViewGroup struct {
	mu    sync.Mutex
	calls map[int64]*stationViewCall
}

type stationViewCall struct {
	done    chan struct{}
	cancel  context.CancelFunc
	waiters int

	// Written once by the build goroutine before done is closed, so a reader
	// that observes the close sees them via that happens-before edge.
	view *domain.OperatorStationView
	err  error
}

func newStationViewGroup() *stationViewGroup {
	return &stationViewGroup{calls: make(map[int64]*stationViewCall)}
}

// do returns the view for stationID, running build at most once concurrently
// per station. Callers arriving while a build is in flight receive that
// build's result rather than starting another.
//
// build runs on the shared context described on the type — NOT on ctx. ctx
// governs only how long THIS caller is prepared to wait.
func (g *stationViewGroup) do(
	ctx context.Context,
	stationID int64,
	build func(context.Context) (*domain.OperatorStationView, error),
) (*domain.OperatorStationView, error) {
	g.mu.Lock()
	call := g.calls[stationID]
	if call == nil {
		// WithoutCancel: the build must outlive the request that started it, or
		// the first client to disconnect would kill a build the others are still
		// waiting on. Its lifetime is governed by the waiter count instead.
		buildCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
		call = &stationViewCall{done: make(chan struct{}), cancel: cancel}
		g.calls[stationID] = call
		go func() {
			defer close(call.done)
			defer cancel()
			view, err := build(buildCtx)
			g.mu.Lock()
			call.view, call.err = view, err
			// Drop it before signalling so the next request starts a fresh build
			// rather than joining a finished one.
			if g.calls[stationID] == call {
				delete(g.calls, stationID)
			}
			g.mu.Unlock()
		}()
	}
	call.waiters++
	g.mu.Unlock()

	defer func() {
		g.mu.Lock()
		call.waiters--
		abandoned := call.waiters == 0
		// Unpublish an abandoned call under the same lock that cancels it, so a
		// request arriving in the gap can never join a build that is about to be
		// torn down and receive a spurious context.Canceled.
		if abandoned && g.calls[stationID] == call {
			delete(g.calls, stationID)
		}
		g.mu.Unlock()
		if abandoned {
			call.cancel()
		}
	}()

	select {
	case <-call.done:
		return call.view, call.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
