package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// agedEnvelope builds a real data envelope for subject and then ages it: both
// timestamps move back by age, the way an envelope stamped at enqueue looks
// when it reaches Core `age` later. A zero expiry stays zero.
func agedEnvelope(t *testing.T, subject string, age time.Duration) []byte {
	t.Helper()
	env, err := NewDataEnvelope(subject,
		Address{Role: RoleEdge, Station: "stn-test"}, Address{Role: RoleCore},
		map[string]any{"edge_snapshot_id": 1})
	if err != nil {
		t.Fatalf("NewDataEnvelope(%s): %v", subject, err)
	}
	env.Timestamp = env.Timestamp.Add(-age)
	if !env.ExpiresAt.IsZero() {
		env.ExpiresAt = env.ExpiresAt.Add(-age)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func dispatchedAfter(t *testing.T, subject string, age time.Duration) bool {
	t.Helper()
	ing := NewIngestor(nil)
	got := false
	ing.Dispatch = func(*Envelope) { got = true }
	ing.HandleRaw(agedEnvelope(t, subject, age))
	return got
}

// TestProductionTickFeed_LateDelivery (P8) characterises what Core's ingestor
// does with a heartbeat tick that reaches it six minutes after the Edge stamped
// it — an outage the outbox survived.
//
// TODAY the feed's subject is production.tick, which has no subjectTTLs entry
// and takes the 5-minute data default: the tick is dropped at the header, after
// the Edge already marked it sent, and the gap becomes a fake stop in MTBF and
// Lost. "production.ticks" is not a declared subject yet and takes the same
// default.
//
// INVERTS when the feed moves to production.ticks as a NoExpiry subject: the
// late copy is dispatched, and Core's (cell_id, edge_snapshot_id, recorded_at)
// key makes it harmless. production.tick itself keeps the default — an old
// Edge stamps its own expiry, so nothing changes for it.
func TestProductionTickFeed_LateDelivery(t *testing.T) {
	// Not parallel: a drop bumps the process-wide ExpiredDrops counter that
	// TestIngestor_ExpiredDropIsCounted measures by difference.
	const late = 6 * time.Minute

	if dispatchedAfter(t, SubjectProductionTick, late) {
		t.Errorf("production.tick %s late was dispatched; want dropped at the header (5 min default)", late)
	}
	if !dispatchedAfter(t, SubjectProductionTick, time.Minute) {
		t.Errorf("production.tick 1m late was dropped; the control arm must pass")
	}
	if dispatchedAfter(t, "production.ticks", late) {
		t.Errorf(`"production.ticks" %s late was dispatched; at this base it is undeclared and takes the default`, late)
	}
}
