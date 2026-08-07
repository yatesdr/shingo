package service

import "testing"

// Guard 1's threshold, and the reason it exists.
//
// Inside a reflector-declared zone a robot returns a clean reading or none at
// all. So a lane routed INTO a bad zone loses its worst readings entirely and
// its conditioned average goes UP while the floor got worse. When the miss rate
// moves materially the two averages are over different populations and the
// delta between them is an artifact of what survived.
func TestLaneChange_SuppressionThreshold(t *testing.T) {
	t.Parallel()
	if noEstimateMovedMaterially != 0.10 {
		t.Errorf("threshold = %v, want 0.10 — ten points is the line between "+
			"'comparable enough to show side by side' and 'measuring different "+
			"populations'", noEstimateMovedMaterially)
	}
}

// The delta is nil when either side is unknown, never zero.
//
// Zero means "it did not move". Unknown means "we cannot say". Rendering the
// second as the first is the no-data/zero rule failing on the one number the
// engineer is reading to decide whether their change worked.
func TestLaneChange_P50DeltaIsNilWhenEitherSideIsUnknown(t *testing.T) {
	t.Parallel()
	v := 0.8
	for _, tc := range []struct {
		name          string
		before, after *float64
		wantNil       bool
	}{
		{"both known", &v, &v, false},
		{"before unknown", nil, &v, true},
		{"after unknown", &v, nil, true},
		{"neither known", nil, nil, true},
	} {
		got := LaneChange{P50Before: tc.before, P50After: tc.after}.P50Delta()
		if (got == nil) != tc.wantNil {
			t.Errorf("%s: delta nil = %v, want %v", tc.name, got == nil, tc.wantNil)
		}
	}
}
