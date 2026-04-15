package countgroup

import (
	"errors"
	"sync"
	"testing"
	"time"

	"shingocore/config"
)

// fakePoller returns programmed responses in order; on exhaustion it
// returns the last response forever (or errForever if set).
type fakePoller struct {
	mu          sync.Mutex
	responses   [][]string
	errors      []error
	errForever  error
	calls       int
}

func (f *fakePoller) GetRobotsInCountGroup(group string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	i := f.calls
	f.calls++
	if f.errForever != nil && i >= len(f.responses) {
		return nil, f.errForever
	}
	if i >= len(f.responses) {
		if len(f.responses) == 0 {
			return []string{}, nil
		}
		return f.responses[len(f.responses)-1], nil
	}
	if i < len(f.errors) && f.errors[i] != nil {
		return nil, f.errors[i]
	}
	return f.responses[i], nil
}

// recordingEmitter captures all Transitions emitted by the loop.
type recordingEmitter struct {
	mu          sync.Mutex
	transitions []Transition
}

func (r *recordingEmitter) Emit(t Transition) {
	r.mu.Lock()
	r.transitions = append(r.transitions, t)
	r.mu.Unlock()
}

func (r *recordingEmitter) snapshot() []Transition {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Transition(nil), r.transitions...)
}

// waitForTransitions polls until emitter has >= n transitions or timeout.
func waitForTransitions(t *testing.T, em *recordingEmitter, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if len(em.snapshot()) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d transitions (saw %d)", n, len(em.snapshot()))
}

func fastConfig(groups []config.CountGroupConfig) config.CountGroupsConfig {
	return config.CountGroupsConfig{
		PollInterval:       20 * time.Millisecond,
		RDSTimeout:         50 * time.Millisecond,
		OnThreshold:        2,
		OffThreshold:       3,
		FailSafeTimeout:    100 * time.Millisecond,
		NeverOccupiedWarn:  24 * time.Hour, // disabled for this test
		NeverOccupiedError: 48 * time.Hour,
		Groups:             groups,
	}
}

func TestLoopEmitsOnThenOffAfterDebounce(t *testing.T) {
	p := &fakePoller{
		responses: [][]string{
			// Startup probe consumes the first response.
			{}, // probe: empty
			// Tick loop: on_threshold=2, off_threshold=3.
			{"AMR-01"}, // 1 occupied
			{"AMR-01"}, // 2 occupied → emit ON
			{"AMR-01"}, // stable (no emit — debouncer returns changed=false)
			{},         // 1 empty
			{},         // 2 empty
			{},         // 3 empty → emit OFF
		},
	}
	em := &recordingEmitter{}
	cfg := fastConfig([]config.CountGroupConfig{{Name: "Z1", Enabled: true}})
	r := NewRunner(cfg, p, em, nil)
	r.Start()
	defer r.Stop()

	waitForTransitions(t, em, 2, 2*time.Second)

	got := em.snapshot()
	if got[0].Group != "Z1" || got[0].Desired != "on" {
		t.Fatalf("first transition want Z1/on, got %+v", got[0])
	}
	if got[1].Desired != "off" {
		t.Fatalf("second transition want off, got %+v", got[1])
	}
	if got[0].FailSafeTriggered || got[1].FailSafeTriggered {
		t.Fatalf("no transitions should be fail-safe in clean run")
	}
}

func TestLoopFailSafeOnContinuousRDSError(t *testing.T) {
	p := &fakePoller{
		responses:  [][]string{{}}, // probe ok
		errForever: errors.New("rds down"),
	}
	em := &recordingEmitter{}
	cfg := fastConfig([]config.CountGroupConfig{{Name: "Z1", Enabled: true}})
	// FailSafeTimeout=100ms; at 20ms poll, ~5 ticks until we force on.
	r := NewRunner(cfg, p, em, nil)
	r.Start()
	defer r.Stop()

	waitForTransitions(t, em, 1, 2*time.Second)
	got := em.snapshot()[0]
	if got.Desired != "on" || !got.FailSafeTriggered {
		t.Fatalf("want fail-safe on, got %+v", got)
	}

	// No second emit while we're still in fail-safe.
	time.Sleep(200 * time.Millisecond)
	if len(em.snapshot()) != 1 {
		t.Fatalf("fail-safe should emit exactly once, got %d transitions", len(em.snapshot()))
	}
}

func TestLoopSkipsDisabledGroups(t *testing.T) {
	p := &fakePoller{}
	em := &recordingEmitter{}
	cfg := fastConfig([]config.CountGroupConfig{{Name: "Z1", Enabled: false}})
	r := NewRunner(cfg, p, em, nil)
	r.Start()
	defer r.Stop()

	time.Sleep(100 * time.Millisecond)
	if p.calls != 0 {
		t.Fatalf("disabled group should not poll, calls=%d", p.calls)
	}
}
