package engine

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
)

// startLog starts a changeover on the add-node scenario with the sourcing feed
// as set up by hold (nil holds nothing) and returns the changeover and every
// line the engine logged.
func startLog(t *testing.T, hold func(eng *Engine)) (int64, string) {
	t.Helper()
	db := testEngineDB(t)
	processID, _, _, toStyleID := seedAddNodeScenario(t, db)
	eng := testEngine(t, db)
	eng.wireEventHandlers()
	coreNeverAnswers(t, eng)
	var (
		mu    sync.Mutex
		lines []string
	)
	eng.logFn = func(format string, args ...any) {
		mu.Lock()
		lines = append(lines, fmt.Sprintf(format, args...))
		mu.Unlock()
	}
	if hold != nil {
		hold(eng)
	}
	co, err := eng.StartProcessChangeover(processID, toStyleID, "test", "stock advisory")
	if err != nil {
		t.Fatalf("the start must not refuse on stock, got: %v", err)
	}
	if co == nil || co.ID == 0 || co.State.IsTerminal() {
		t.Fatalf("start = %+v, want a live changeover", co)
	}
	mu.Lock()
	defer mu.Unlock()
	return co.ID, strings.Join(lines, "\n")
}

// verdict holds the feed's snapshot for the scenario's process: one verdict
// for the to-style.
func verdict(s protocol.SourcingState) func(eng *Engine) {
	return func(eng *Engine) {
		s.ProcessID, s.StyleID = "ADD-PROC", "Style-WithNode"
		eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: true, States: []protocol.SourcingState{s}})
	}
}

// TestStartChangeover_TheStockGateReadsTheFeed is the changeover start's stock
// gate on the sourcing feed (owner's ruling, cleanup brief Phase 3). It asks
// Core nothing and never refuses, where it refused when Core did not answer
// its preflight ask (the arm the previous commit pinned at the base):
//   - a red verdict naming the missing payload starts with that advisory, as
//     the 2026-06-04 change made it (Springfield NF SPOT 3: refusing dead-ended
//     the operator with idle robots); the supply legs queue as Awaiting Stock;
//   - no verdict for the style, and a verdict from a copy past CurrentAt, start
//     with "stock not checked";
//   - a green verdict from a current copy starts with neither.
func TestStartChangeover_TheStockGateReadsTheFeed(t *testing.T) {
	t.Parallel()
	const missing, unchecked = "not yet in stock", "stock not checked"
	cases := []struct {
		name   string
		hold   func(eng *Engine)
		want   string
		absent []string
	}{
		{"red, current: the advisory", verdict(protocol.SourcingState{Status: "red", Missing: []string{"PART-ADD"}}),
			missing, []string{unchecked}},
		{"no verdict held", nil, unchecked, []string{missing}},
		{"a copy past CurrentAt", func(eng *Engine) {
			verdict(protocol.SourcingState{Status: "green"})(eng)
			eng.feeds.now = func() time.Time { return time.Now().Add(FeedAsOfAfter + time.Minute) }
		}, unchecked, []string{missing}},
		{"green, current", verdict(protocol.SourcingState{Status: "green"}), "", []string{missing, unchecked}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			_, log := startLog(t, c.hold)
			if c.want != "" && !strings.Contains(log, c.want) {
				t.Errorf("log does not say %q:\n%s", c.want, log)
			}
			if c.want == missing && !strings.Contains(log, "PART-ADD") {
				t.Errorf("the advisory does not name PART-ADD:\n%s", log)
			}
			for _, a := range c.absent {
				if strings.Contains(log, a) {
					t.Errorf("log says %q, want it not to:\n%s", a, log)
				}
			}
		})
	}
}
