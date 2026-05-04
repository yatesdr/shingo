package engine

import (
	"testing"

	"shingo/protocol"
)

// TestResolveReplenishUOP pins the decision matrix:
// produce role → 0 (line ticks UP); other roles → claim capacity.
// The binID parameter is currently a no-op (kept for symmetry).
func TestResolveReplenishUOP(t *testing.T) {
	bin := int64(42)
	cases := []struct {
		name     string
		role     protocol.ClaimRole
		capacity int
		binID    *int64
		want     int
	}{
		{
			name:     "produce_starts_at_zero",
			role:     protocol.ClaimRoleProduce,
			capacity: 200,
			binID:    &bin,
			want:     0,
		},
		{
			name:     "consume_resets_to_capacity",
			role:     protocol.ClaimRoleConsume,
			capacity: 200,
			binID:    &bin,
			want:     200,
		},
		{
			name:     "changeover_resets_to_capacity",
			role:     protocol.ClaimRoleChangeover,
			capacity: 150,
			binID:    &bin,
			want:     150,
		},
		{
			name:     "nil_bin_consume_still_capacity",
			role:     protocol.ClaimRoleConsume,
			capacity: 200,
			binID:    nil,
			want:     200,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveReplenishUOP(tc.role, tc.capacity, tc.binID)
			if got != tc.want {
				t.Errorf("resolveReplenishUOP(%v, %d, %v) = %d, want %d",
					tc.role, tc.capacity, tc.binID, got, tc.want)
			}
		})
	}
}
