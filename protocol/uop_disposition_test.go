package protocol

import (
	"encoding/json"
	"testing"
)

// TestOrderRelease_LegacyShapeRoundTrip pins the back-compat invariant:
// an OrderRelease that carries only the legacy RemainingUOP pointer (no
// Disposition) must round-trip through JSON without leaking a non-nil
// Disposition field. Required so a Phase 0c-aware Core does not see a
// "Disposition is set" signal from an old-Edge envelope.
func TestOrderRelease_LegacyShapeRoundTrip(t *testing.T) {
	zero := 0
	for _, tc := range []struct {
		name string
		uop  *int
	}{
		{"nil", nil},
		{"zero", &zero},
		{"positive", intPtr(42)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &OrderRelease{
				OrderUUID:    "uuid-legacy-" + tc.name,
				RemainingUOP: tc.uop,
				CalledBy:     "stephen-station-1",
			}
			b, err := json.Marshal(src)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got OrderRelease
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.OrderUUID != src.OrderUUID {
				t.Errorf("OrderUUID = %q, want %q", got.OrderUUID, src.OrderUUID)
			}
			if !intPtrEqual(got.RemainingUOP, src.RemainingUOP) {
				t.Errorf("RemainingUOP = %v, want %v", deref(got.RemainingUOP), deref(src.RemainingUOP))
			}
			if got.Disposition != nil {
				t.Errorf("Disposition = %+v, want nil (legacy shape carries no enum)", got.Disposition)
			}
		})
	}
}

// TestOrderRelease_NewShapeRoundTrip pins the new disposition wire shape:
// each of the three operator-intent kinds round-trips correctly with the
// fields they care about.
func TestOrderRelease_NewShapeRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name string
		disp UOPDisposition
	}{
		{
			name: "pull_parts_with_captures",
			disp: UOPDisposition{
				Kind:     DispositionPullParts,
				Captures: map[string]int{"PART-A": 12, "PART-B": 5},
			},
		},
		{
			name: "release_partial_with_count",
			disp: UOPDisposition{
				Kind:  DispositionReleasePartial,
				Count: 47,
			},
		},
		{
			name: "release_empty",
			disp: UOPDisposition{
				Kind: DispositionReleaseEmpty,
			},
		},
		{
			// Operator opened the modal at runtime=60, edited to 47.
			// CountSuggested preserves the system-suggested baseline so
			// Core can record the override.
			name: "release_partial_with_override",
			disp: UOPDisposition{
				Kind:           DispositionReleasePartial,
				Count:          47,
				CountSuggested: intPtr(60),
			},
		},
		{
			// Pull-parts override: PART-A label said 12 but operator
			// counted 9; PART-B was correct at 5.
			name: "pull_parts_with_capture_override",
			disp: UOPDisposition{
				Kind:              DispositionPullParts,
				Captures:          map[string]int{"PART-A": 9, "PART-B": 5},
				CapturesSuggested: map[string]int{"PART-A": 12, "PART-B": 5},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := &OrderRelease{
				OrderUUID:   "uuid-new-" + tc.name,
				Disposition: &tc.disp,
				CalledBy:    "stephen-station-2",
			}
			b, err := json.Marshal(src)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var got OrderRelease
			if err := json.Unmarshal(b, &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if got.Disposition == nil {
				t.Fatalf("Disposition = nil after round-trip; want %+v", tc.disp)
			}
			if got.Disposition.Kind != tc.disp.Kind {
				t.Errorf("Kind = %q, want %q", got.Disposition.Kind, tc.disp.Kind)
			}
			if got.Disposition.Count != tc.disp.Count {
				t.Errorf("Count = %d, want %d", got.Disposition.Count, tc.disp.Count)
			}
			if len(got.Disposition.Captures) != len(tc.disp.Captures) {
				t.Errorf("Captures len = %d, want %d", len(got.Disposition.Captures), len(tc.disp.Captures))
			}
			for k, v := range tc.disp.Captures {
				if got.Disposition.Captures[k] != v {
					t.Errorf("Captures[%q] = %d, want %d", k, got.Disposition.Captures[k], v)
				}
			}
			if !intPtrEqual(got.Disposition.CountSuggested, tc.disp.CountSuggested) {
				t.Errorf("CountSuggested = %v, want %v",
					deref(got.Disposition.CountSuggested), deref(tc.disp.CountSuggested))
			}
			if len(got.Disposition.CapturesSuggested) != len(tc.disp.CapturesSuggested) {
				t.Errorf("CapturesSuggested len = %d, want %d",
					len(got.Disposition.CapturesSuggested), len(tc.disp.CapturesSuggested))
			}
			for k, v := range tc.disp.CapturesSuggested {
				if got.Disposition.CapturesSuggested[k] != v {
					t.Errorf("CapturesSuggested[%q] = %d, want %d",
						k, got.Disposition.CapturesSuggested[k], v)
				}
			}
		})
	}
}

// TestOrderRelease_BothShapesCoexist verifies the transitional contract:
// an envelope can carry both RemainingUOP and Disposition simultaneously
// during the Phase 0c → Phase 1 ramp. Receivers prefer Disposition when
// both are present (per the OrderRelease doc comment); this test only
// pins that the two fields are independently preserved on the wire.
func TestOrderRelease_BothShapesCoexist(t *testing.T) {
	uop := 42
	src := &OrderRelease{
		OrderUUID:    "uuid-both",
		RemainingUOP: &uop,
		Disposition: &UOPDisposition{
			Kind:  DispositionReleasePartial,
			Count: 42,
		},
		CalledBy: "stephen-station-3",
	}
	b, err := json.Marshal(src)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got OrderRelease
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.RemainingUOP == nil || *got.RemainingUOP != 42 {
		t.Errorf("RemainingUOP = %v, want *42", deref(got.RemainingUOP))
	}
	if got.Disposition == nil || got.Disposition.Kind != DispositionReleasePartial {
		t.Errorf("Disposition = %+v, want kind=release_partial", got.Disposition)
	}
}

func intPtr(v int) *int { return &v }

func intPtrEqual(a, b *int) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func deref(p *int) any {
	if p == nil {
		return nil
	}
	return *p
}
