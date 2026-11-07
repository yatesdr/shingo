package protocol

import (
	"testing"
	"time"
)

// expiry_feedspin_test.go — the heartbeat and ack lifetime on the wire. It was
// written as a prediction before the versioned feeds lengthened the heartbeat
// pair's TTL from 90 s to 5 minutes; that change has landed, and the TTL table
// itself is TestDataTTLForSubjects'. What this file still pins, and nothing else
// in protocol does, is the envelope end of it.

// TestFeedsPin_HeartbeatAndAckTTL pins, for the heartbeat and register pairs,
// the lifetime the sender stamps on the envelope and what the ingestor does with
// a copy two minutes late: inside the 5-minute budget, so it is dispatched. A
// copy that late was dropped while the heartbeat pair carried 90 s.
//
// Not parallel: a drop bumps the process-wide ExpiredDrops counter that
// TestIngestor_ExpiredDropIsCounted measures by difference.
func TestFeedsPin_HeartbeatAndAckTTL(t *testing.T) {
	const late = 2 * time.Minute
	for _, subject := range []string{
		SubjectEdgeHeartbeat, SubjectEdgeHeartbeatAck, SubjectEdgeRegister, SubjectEdgeRegistered,
	} {
		const ttl = 5 * time.Minute
		env, err := NewDataEnvelope(subject, Address{Role: RoleEdge, Station: "edge.test"}, Address{Role: RoleCore}, struct{}{})
		if err != nil {
			t.Fatalf("NewDataEnvelope(%s): %v", subject, err)
		}
		if got := env.ExpiresAt.Sub(env.Timestamp); got != ttl {
			t.Errorf("%s stamped lifetime = %v, want %v", subject, got, ttl)
		}
		if !dispatchedAfter(t, subject, late) {
			t.Errorf("%s %v late was not dispatched; its %v lifetime covers it", subject, late, ttl)
		}
	}
}
