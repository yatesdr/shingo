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

// TestProductionTickFeed_LateDelivery (P8) pins what Core's ingestor does with
// a heartbeat tick that reaches it six minutes after the Edge stamped it: an
// outage the Edge's counter_snapshots backlog survived.
//
// INVERTED by the move to counter_snapshots as the queue. It used to pin the
// drop: the feed's subject was production.tick, which takes the 5-minute data
// default, so the tick died at the header after the Edge had marked it sent and
// the gap became a fake stop in MTBF and Lost. The feed is now
// production.ticks, a NoExpiry subject: the late copy is dispatched, and Core's
// (cell_id, edge_snapshot_id, recorded_at) key makes a duplicate harmless.
// production.tick keeps the default: an old Edge stamps its own expiry, so
// nothing changes for it. The subject is spelled out rather than taken from the
// constant so the inversion runs red at the base that lacked it.
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
	if !dispatchedAfter(t, "production.ticks", late) {
		t.Errorf(`production.ticks %s late was dropped; the feed is NoExpiry`, late)
	}
}
