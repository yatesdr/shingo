package audit

import "testing"

// TestReleaseFamilyOps_CoversQ036Ops pins the unloaded op set the footprint
// velocity chart counts: the live ops the old hardcoded 3-op filter missed
// (released_capture_empty / released_underpack) must be present, alongside the
// originals and fallbacks. This is the drift guard the Q-036 fix promised.
func TestReleaseFamilyOps_CoversQ036Ops(t *testing.T) {
	have := map[string]bool{}
	for _, op := range ReleaseFamilyOps {
		have[op] = true
	}
	for _, want := range []string{
		OpClearForReuse,
		OpReleasedEmpty,
		OpReleasedPartial,
		OpReleasedEmptyFallback,
		OpReleasedPartialFallback,
		OpReleasedCaptureEmpty, // missed by the old filter — Q-036
		OpReleasedUnderpack,    // missed by the old filter — Q-036
	} {
		if !have[want] {
			t.Errorf("ReleaseFamilyOps missing %q", want)
		}
	}
	// clear_and_claim is a re-purpose, not an unload — must NOT be counted.
	if have[OpClearAndClaim] {
		t.Errorf("ReleaseFamilyOps should not include %q (re-purpose, not unload)", OpClearAndClaim)
	}
}
