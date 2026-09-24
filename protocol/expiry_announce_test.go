package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// expiry_announce_test.go — what the Edge's ingestor does with a Core→Edge
// count announcement (UOPAdjustment, BinEpochRefresh) that arrives late.

// announcementAged builds the envelope Core builds for subject, then moves it
// age into the past as if it had sat in Core's outbox or the broker that long.
// Only a stamp that exists moves: an envelope with no exp has nothing to age.
func announcementAged(t *testing.T, subject string, age time.Duration) []byte {
	t.Helper()
	env, err := NewDataEnvelope(subject,
		Address{Role: RoleCore, Station: "core.test"},
		Address{Role: RoleEdge, Station: StationBroadcast},
		map[string]any{"bin_id": 27, "core_node_name": "NODE-A", "epoch": 3})
	if err != nil {
		t.Fatalf("NewDataEnvelope(%s): %v", subject, err)
	}
	env.Timestamp = env.Timestamp.Add(-age)
	if !env.ExpiresAt.IsZero() {
		env.ExpiresAt = env.ExpiresAt.Add(-age)
	}
	raw, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal %s: %v", subject, err)
	}
	return raw
}

// TestLateAnnouncementReachesTheEdge is S5a. UOPAdjustment and BinEpochRefresh
// carry no exp, so one delivered after an outage of any length reaches the
// Edge's handler. Safe to arrive late because every epoch write they reach
// refuses to move a bound carrier's stamp backward
// (shingo-edge/engine/epoch_monotonic_test.go).
//
// Inverted pin: at base (TestPin_P0i_LateAnnouncementIsDroppedAtTheEdge) they
// took TypeData's 5 minutes and the ingestor dropped one delivered 6 minutes
// late.
func TestLateAnnouncementReachesTheEdge(t *testing.T) {
	for _, subject := range []string{SubjectUOPAdjustment, SubjectBinEpochRefresh} {
		if got := DataTTLFor(subject); got != NoExpiry {
			t.Errorf("DataTTLFor(%s) = %v, want NoExpiry", subject, got)
		}
		for _, age := range []time.Duration{6 * time.Minute, 24 * time.Hour} {
			ing := NewIngestor(nil)
			dispatched := false
			ing.Dispatch = func(*Envelope) { dispatched = true }
			ing.HandleRaw(announcementAged(t, subject, age))
			if !dispatched {
				t.Errorf("%s delivered %v late was dropped at the ingestor; the station keeps "+
					"counting under a generation that has ended", subject, age)
			}
		}
	}
}
