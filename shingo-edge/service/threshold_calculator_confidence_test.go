package service

import "testing"

// scoreConfidence gates HIGH/MEDIUM on L2 (store) coverage alongside L1
// and retrieves, because L2 load/transit timings feed the threshold
// formula. These cases pin that a window starved of L2 stores cannot be
// stamped HIGH/MEDIUM no matter how rich its L1/retrieve coverage is.
func TestScoreConfidence_L2Gates(t *testing.T) {
	tests := []struct {
		name                   string
		days, l1, l2, retrieve int
		want                   string
	}{
		{"all high", 14, 20, 20, 20, "HIGH"},
		{"high but L2 starved", 14, 20, 0, 20, "LOW"}, // L2=0 fails both HIGH and MEDIUM L2 gate
		{"high but L2 short of 20", 14, 20, 19, 20, "MEDIUM"},
		{"all medium", 7, 10, 10, 10, "MEDIUM"},
		{"medium but L2 starved", 7, 10, 0, 10, "LOW"},
		{"medium but L2 short of 10", 7, 10, 9, 10, "LOW"},
		{"L1 short", 14, 19, 20, 20, "MEDIUM"},
		{"retrieve short", 14, 20, 20, 19, "MEDIUM"},
		{"days short of 14", 13, 20, 20, 20, "MEDIUM"},
		{"days short of 7", 6, 10, 10, 10, "LOW"},
		{"nothing", 0, 0, 0, 0, "LOW"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := scoreConfidence(tt.days, tt.l1, tt.l2, tt.retrieve); got != tt.want {
				t.Errorf("scoreConfidence(%d,%d,%d,%d) = %q, want %q",
					tt.days, tt.l1, tt.l2, tt.retrieve, got, tt.want)
			}
		})
	}
}
