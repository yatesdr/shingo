package engine

import (
	"sync"
	"testing"

	"shingocore/fleet"
)

// fakeReconnectFleet is the smallest fleet that implements both
// fleet.Backend (for the engine.fleet field type) and fleet.RobotLister
// (so autoResumeAfterFleetReconnect's type assertion succeeds).
// Backend methods are stubs — only RobotLister methods are exercised.
type fakeReconnectFleet struct {
	mu sync.Mutex

	// statusAfterReconnect is what GetRobotsStatus returns. Drives the
	// "current" half of the diff inside autoResumeAfterFleetReconnect.
	statusAfterReconnect []fleet.RobotStatus

	// setAvailabilityCalls captures every SetAvailability(vehicleID, true)
	// the handler issues. Order preserved for ordering assertions.
	setAvailabilityCalls []string

	// setAvailabilityAttempts counts every SetAvailability call
	// regardless of error — used by the per-robot-error test to assert
	// the handler iterated the full set rather than aborting on the
	// first failure.
	setAvailabilityAttempts int

	// getStatusErr / setAvailabilityErr exercise the failure-mode log-
	// and-continue branches.
	getStatusErr       error
	setAvailabilityErr error
}

var _ fleet.Backend = (*fakeReconnectFleet)(nil)
var _ fleet.RobotLister = (*fakeReconnectFleet)(nil)

// --- fleet.RobotLister ---

func (f *fakeReconnectFleet) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getStatusErr != nil {
		return nil, f.getStatusErr
	}
	out := make([]fleet.RobotStatus, len(f.statusAfterReconnect))
	copy(out, f.statusAfterReconnect)
	return out, nil
}

func (f *fakeReconnectFleet) SetAvailability(vehicleID string, available bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.setAvailabilityAttempts++
	if f.setAvailabilityErr != nil {
		return f.setAvailabilityErr
	}
	if available {
		f.setAvailabilityCalls = append(f.setAvailabilityCalls, vehicleID)
	}
	return nil
}

func (f *fakeReconnectFleet) RetryFailed(string) error   { return nil }
func (f *fakeReconnectFleet) ForceComplete(string) error { return nil }

// --- fleet.Backend stubs (unused by the handler under test) ---

func (f *fakeReconnectFleet) CreateOrder(fleet.CreateOrderRequest) (fleet.TransportOrderResult, error) {
	return fleet.TransportOrderResult{}, nil
}
func (f *fakeReconnectFleet) CancelOrder(string) error                            { return nil }
func (f *fakeReconnectFleet) SetOrderPriority(string, int) error                  { return nil }
func (f *fakeReconnectFleet) Ping() error                                         { return nil }
func (f *fakeReconnectFleet) Name() string                                        { return "fakeReconnect" }
func (f *fakeReconnectFleet) MapState(string) string                              { return "" }
func (f *fakeReconnectFleet) IsTerminalState(string) bool                         { return false }
func (f *fakeReconnectFleet) ReleaseOrder(string, []fleet.OrderBlock, bool) error { return nil }
func (f *fakeReconnectFleet) Reconfigure(fleet.ReconfigureParams)                 {}

// newReconnectEngine wires the minimum Engine surface
// autoResumeAfterFleetReconnect / capture / take touch: fleet,
// robotsCache, robotsMu, logFn. Bypasses engine.New so the test
// doesn't need a DB / messaging / config.
func newReconnectEngine(t *testing.T, f *fakeReconnectFleet) *Engine {
	t.Helper()
	return &Engine{
		fleet:       f,
		robotsCache: make(map[string]fleet.RobotStatus),
		logFn:       t.Logf,
	}
}

// --- captureDisconnectBaseline / takeReconnectBaseline -----------------

// TestCaptureDisconnectBaseline_CopiesAvailabilityFromRobotsCache pins
// the contract that captureDisconnectBaseline snapshots availability
// from robotsCache without holding a reference. Mutating the cache
// afterward must not affect the captured baseline.
func TestCaptureDisconnectBaseline_CopiesAvailabilityFromRobotsCache(t *testing.T) {
	eng := newReconnectEngine(t, &fakeReconnectFleet{})
	eng.robotsCache["R1"] = fleet.RobotStatus{VehicleID: "R1", Available: true}
	eng.robotsCache["R2"] = fleet.RobotStatus{VehicleID: "R2", Available: false}

	eng.captureDisconnectBaseline()

	// Mutate the source — captured baseline must not change.
	eng.robotsCache["R1"] = fleet.RobotStatus{VehicleID: "R1", Available: false}
	eng.robotsCache["R3"] = fleet.RobotStatus{VehicleID: "R3", Available: true}

	got := eng.preDisconnectAvailability
	if v, ok := got["R1"]; !ok || !v {
		t.Errorf("baseline[R1] = (%v, %v), want (true, true)", v, ok)
	}
	if v, ok := got["R2"]; !ok || v {
		t.Errorf("baseline[R2] = (%v, %v), want (false, true)", v, ok)
	}
	if _, ok := got["R3"]; ok {
		t.Errorf("baseline[R3] present; captured snapshot must not see post-capture additions")
	}
}

// TestTakeReconnectBaseline_ReturnsAndClears pins the single-shot
// consume semantic: the first take returns the captured map, the
// second take returns nil. Prevents accidental re-use across reconnect
// cycles without a fresh capture.
func TestTakeReconnectBaseline_ReturnsAndClears(t *testing.T) {
	eng := newReconnectEngine(t, &fakeReconnectFleet{})
	eng.robotsCache["R1"] = fleet.RobotStatus{VehicleID: "R1", Available: true}
	eng.captureDisconnectBaseline()

	first := eng.takeReconnectBaseline()
	if first == nil || first["R1"] != true {
		t.Fatalf("first take = %v, want non-nil with R1=true", first)
	}

	second := eng.takeReconnectBaseline()
	if second != nil {
		t.Errorf("second take = %v, want nil (single-shot)", second)
	}
}

// TestTakeReconnectBaseline_NoCapture_ReturnsNil covers the startup
// case: engine fresh, no disconnect ever observed, take returns nil
// so autoResumeAfterFleetReconnect's len()==0 guard fires and no
// blanket-wake happens.
func TestTakeReconnectBaseline_NoCapture_ReturnsNil(t *testing.T) {
	eng := newReconnectEngine(t, &fakeReconnectFleet{})
	if got := eng.takeReconnectBaseline(); got != nil {
		t.Errorf("take before capture = %v, want nil", got)
	}
}

// --- autoResumeAfterFleetReconnect -------------------------------------

// TestAutoResumeAfterFleetReconnect_ResumesOnlyPreviouslyAvailable pins
// the scope rule: only robots that were Available=true in the baseline
// AND are now Available=false get resumed. Operator-paused robots
// (Available=false in the baseline) stay paused.
func TestAutoResumeAfterFleetReconnect_ResumesOnlyPreviouslyAvailable(t *testing.T) {
	f := &fakeReconnectFleet{
		statusAfterReconnect: []fleet.RobotStatus{
			{VehicleID: "R1", Connected: true, Available: false},
			{VehicleID: "R2", Connected: true, Available: false},
		},
	}
	eng := newReconnectEngine(t, f)

	// Pre-disconnect: R1 was running, R2 was operator-paused.
	baseline := map[string]bool{"R1": true, "R2": false}

	eng.autoResumeAfterFleetReconnect(baseline)

	if len(f.setAvailabilityCalls) != 1 || f.setAvailabilityCalls[0] != "R1" {
		t.Errorf("SetAvailability calls = %v, want exactly [R1] (R2 was operator-paused before the blip)",
			f.setAvailabilityCalls)
	}
}

// TestAutoResumeAfterFleetReconnect_SkipsAlreadyAvailable covers the
// "RDS didn't park this one" branch: a robot Available=true in the
// baseline and still Available=true after reconnect doesn't need to
// be touched.
func TestAutoResumeAfterFleetReconnect_SkipsAlreadyAvailable(t *testing.T) {
	f := &fakeReconnectFleet{
		statusAfterReconnect: []fleet.RobotStatus{
			{VehicleID: "R1", Connected: true, Available: true},
		},
	}
	eng := newReconnectEngine(t, f)
	baseline := map[string]bool{"R1": true}

	eng.autoResumeAfterFleetReconnect(baseline)

	if len(f.setAvailabilityCalls) != 0 {
		t.Errorf("SetAvailability calls = %v, want none (already available)",
			f.setAvailabilityCalls)
	}
}

// TestAutoResumeAfterFleetReconnect_NilBaselineNoOps covers startup:
// no prior disconnect → nil baseline → no work. Refuses to blanket-
// wake on cold boot.
func TestAutoResumeAfterFleetReconnect_NilBaselineNoOps(t *testing.T) {
	f := &fakeReconnectFleet{
		statusAfterReconnect: []fleet.RobotStatus{
			{VehicleID: "R-NEW", Connected: true, Available: false},
		},
	}
	eng := newReconnectEngine(t, f)

	eng.autoResumeAfterFleetReconnect(nil)

	if f.setAvailabilityAttempts != 0 {
		t.Errorf("SetAvailability attempts = %d, want 0 (nil baseline → no work)",
			f.setAvailabilityAttempts)
	}
}

// TestAutoResumeAfterFleetReconnect_EmptyBaselineNoOps covers the
// disconnect-with-empty-cache case: captureDisconnectBaseline ran but
// robotsCache was empty (refresh loop never populated, or all robots
// dropped from the fleet config). Same no-op contract as nil.
func TestAutoResumeAfterFleetReconnect_EmptyBaselineNoOps(t *testing.T) {
	f := &fakeReconnectFleet{
		statusAfterReconnect: []fleet.RobotStatus{
			{VehicleID: "R-NEW", Connected: true, Available: false},
		},
	}
	eng := newReconnectEngine(t, f)

	eng.autoResumeAfterFleetReconnect(map[string]bool{})

	if f.setAvailabilityAttempts != 0 {
		t.Errorf("SetAvailability attempts = %d, want 0 (empty baseline → no work)",
			f.setAvailabilityAttempts)
	}
}

// TestAutoResumeAfterFleetReconnect_SurvivesFetchError pins log-and-
// continue on the GetRobotsStatus failure path. A transient RDS error
// at reconnect must not panic; the operator UI remains the manual
// fallback.
func TestAutoResumeAfterFleetReconnect_SurvivesFetchError(t *testing.T) {
	f := &fakeReconnectFleet{
		getStatusErr: errFakeFleetTransient,
	}
	eng := newReconnectEngine(t, f)
	baseline := map[string]bool{"R1": true}

	// Must not panic.
	eng.autoResumeAfterFleetReconnect(baseline)

	if f.setAvailabilityAttempts != 0 {
		t.Errorf("SetAvailability attempts = %d, want 0 (fetch failed)",
			f.setAvailabilityAttempts)
	}
}

// TestAutoResumeAfterFleetReconnect_ContinuesOnSetAvailabilityError
// pins per-robot log-and-continue: a SetAvailability failure on one
// robot must not abort the resume for the others. Asserts via the
// attempts counter that both candidates were tried.
func TestAutoResumeAfterFleetReconnect_ContinuesOnSetAvailabilityError(t *testing.T) {
	f := &fakeReconnectFleet{
		statusAfterReconnect: []fleet.RobotStatus{
			{VehicleID: "R1", Connected: true, Available: false},
			{VehicleID: "R2", Connected: true, Available: false},
		},
		setAvailabilityErr: errFakeFleetTransient,
	}
	eng := newReconnectEngine(t, f)
	baseline := map[string]bool{"R1": true, "R2": true}

	eng.autoResumeAfterFleetReconnect(baseline)

	if f.setAvailabilityAttempts != 2 {
		t.Errorf("SetAvailability attempts = %d, want 2 (per-robot error must not abort the loop)",
			f.setAvailabilityAttempts)
	}
}

// fakeFleetTransientError is sentinel for the error-path tests; declared
// here to avoid an extra import path in the test file.
type fakeFleetTransientError struct{}

func (fakeFleetTransientError) Error() string { return "fake RDS transient" }

var errFakeFleetTransient = fakeFleetTransientError{}
