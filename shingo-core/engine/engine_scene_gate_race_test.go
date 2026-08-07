package engine

import (
	"sync"
	"testing"
	"time"

	"shingocore/fleet"
)

// The scene gate's read-modify-write, under the concurrency the call graph
// actually permits.
//
// WHY THIS TEST EXISTS. engine.go used to assert that lastSceneSync was
// "touched only from the scene-sync path, which is serialised by
// e.sceneSyncing". Neither half held: SceneSync computes the gate BEFORE
// handing the atomic to scenesync.Sync, and SyncScenePoints reaches sceneGate
// without touching the atomic at all. Three goroutines can arrive together —
// handleNodeSyncFleet, handleSceneSync, and the reconnect goroutine in
// checkConnectionStatus.
//
// Named TestRace_ to match the convention scripts/gate.sh points its targeted
// race recipe at.

// fakeSceneGateFleet is the reconnect fake plus a scene envelope whose
// observation time ADVANCES on every read, so a lost update is visible as a
// duplicated predecessor rather than only as a race-detector report.
type fakeSceneGateFleet struct {
	*fakeReconnectFleet

	mu   sync.Mutex
	tick time.Time
}

var _ fleet.SceneStateProvider = (*fakeSceneGateFleet)(nil)

func (f *fakeSceneGateFleet) GetSceneState() (fleet.SceneState, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.tick = f.tick.Add(time.Millisecond)
	return fleet.SceneState{SceneMD5: "scene-hash", ObservedAt: f.tick}, true
}

// TestRace_SceneGate_EveryCallerGetsADistinctPredecessor pins the property the
// lock actually buys, which is stronger than race-freedom.
//
// previousSync is the LOWER BOUND of the window a diff row claims an edit
// happened in. If two callers read the same lastSceneSync before either writes,
// both diff rows claim the same lower bound and one of them is describing a
// window that another sync already consumed — a wrong "when" on a row whose
// entire purpose is to say when.
//
// Asserted by VALUE as well as under -race, deliberately: a value assertion
// fails on a plain `go test` too, so this does not depend on anyone remembering
// to run the race mode.
func TestRace_SceneGate_EveryCallerGetsADistinctPredecessor(t *testing.T) {
	e := &Engine{
		fleet: &fakeSceneGateFleet{
			fakeReconnectFleet: &fakeReconnectFleet{},
			tick:               time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC),
		},
		logFn: t.Logf,
	}

	const goroutines = 8
	const perGoroutine = 250

	var (
		mu    sync.Mutex
		prevs []*time.Time
		wg    sync.WaitGroup
	)
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			local := make([]*time.Time, 0, perGoroutine)
			for j := 0; j < perGoroutine; j++ {
				_, prev := e.sceneGate()
				local = append(local, prev)
			}
			mu.Lock()
			prevs = append(prevs, local...)
			mu.Unlock()
		}()
	}
	wg.Wait()

	if got, want := len(prevs), goroutines*perGoroutine; got != want {
		t.Fatalf("collected %d predecessors, want %d", got, want)
	}

	// Exactly one caller may see nil — the first one through. A second nil
	// means two callers both believed they were the first sync, and a first
	// diff row is the one row that legitimately has no lower bound.
	nils := 0
	seen := make(map[time.Time]int, len(prevs))
	for _, p := range prevs {
		if p == nil {
			nils++
			continue
		}
		seen[*p]++
	}
	if nils != 1 {
		t.Errorf("%d callers saw a nil predecessor, want exactly 1 — more than one "+
			"sync believed it was the first, so more than one diff row would claim "+
			"no lower bound", nils)
	}
	for at, n := range seen {
		if n > 1 {
			t.Errorf("predecessor %s was handed to %d callers, want 1 — two diff rows "+
				"would claim the same window lower bound, and one of them is wrong",
				at.Format(time.RFC3339Nano), n)
		}
	}
}
