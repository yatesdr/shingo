package protocol

import (
	"testing"
	"time"
)

// expiry_feedspin_test.go — the heartbeat and ack lifetime, pinned before the
// versioned feeds lengthen it. TestDataTTLForSubjects keeps its own assertion
// until the label that moves it rewrites it; this file carries the prediction.

// TestFeedsPin_HeartbeatAndAckTTL pins the TTL table entry, the expiry the
// sender stamps on the envelope, and what the ingestor does with a copy two
// minutes late — inside the predicted 5-minute budget, outside today's 90 s.
//
// Not parallel: a drop bumps the process-wide ExpiredDrops counter that
// TestIngestor_ExpiredDropIsCounted measures by difference.
func TestFeedsPin_HeartbeatAndAckTTL(t *testing.T) {
	const late = 2 * time.Minute
	cases := []struct {
		subject string

		ttl, ttlAfter                     time.Duration
		dispatchedLate, dispatchedLateAft bool
		label                             string
	}{
		{SubjectEdgeHeartbeat, 90 * time.Second, 5 * time.Minute, false, true, "F1"},
		{SubjectEdgeHeartbeatAck, 90 * time.Second, 5 * time.Minute, false, true, "F1"},
		// The register pair already carries 5 minutes and does not move.
		{SubjectEdgeRegister, 5 * time.Minute, 5 * time.Minute, true, true, "same"},
		{SubjectEdgeRegistered, 5 * time.Minute, 5 * time.Minute, true, true, "same"},
	}
	for _, tc := range cases {
		if got := DataTTLFor(tc.subject); got != tc.ttl {
			t.Errorf("DataTTLFor(%s) = %v, want %v (after %s: %v)", tc.subject, got, tc.ttl, tc.label, tc.ttlAfter)
		}
		env, err := NewDataEnvelope(tc.subject, Address{Role: RoleEdge, Station: "edge.test"}, Address{Role: RoleCore}, struct{}{})
		if err != nil {
			t.Fatalf("NewDataEnvelope(%s): %v", tc.subject, err)
		}
		if got := env.ExpiresAt.Sub(env.Timestamp); got != tc.ttl {
			t.Errorf("%s stamped lifetime = %v, want %v (after %s: %v)", tc.subject, got, tc.ttl, tc.label, tc.ttlAfter)
		}
		if got := dispatchedAfter(t, tc.subject, late); got != tc.dispatchedLate {
			t.Errorf("%s %v late dispatched = %v, want %v (after %s: %v)", tc.subject, late, got, tc.dispatchedLate, tc.label, tc.dispatchedLateAft)
		}
	}
}
