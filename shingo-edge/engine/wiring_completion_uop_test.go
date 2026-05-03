package engine

import (
	"testing"

	"shingo/protocol"
)

// TestResolveReplenishUOP pins the post-Item-8 decision matrix for
// the completion-handler UOP reset. Item 8 retired the
// BinUOPRemaining snapshot — the function now collapses to "produce
// → 0, anything else → claim capacity" and the reconciler heals the
// runtime cache to Core's authoritative bin value within ~60s.
func TestResolveReplenishUOP(t *testing.T) {
	cases := []struct {
		name     string
		role     protocol.ClaimRole
		capacity int
		want     int
	}{
		{
			name:     "produce_always_zero",
			role:     protocol.ClaimRoleProduce,
			capacity: 200,
			want:     0,
		},
		{
			name:     "consume_resets_to_capacity",
			role:     protocol.ClaimRoleConsume,
			capacity: 200,
			want:     200,
		},
		{
			name:     "changeover_resets_to_capacity",
			role:     protocol.ClaimRoleChangeover,
			capacity: 150,
			want:     150,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveReplenishUOP(tc.role, tc.capacity)
			if got != tc.want {
				t.Errorf("resolveReplenishUOP(%v, %d) = %d, want %d",
					tc.role, tc.capacity, got, tc.want)
			}
		})
	}
}
