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

// TestPin_P0i_LateAnnouncementIsDroppedAtTheEdge pins V9 at base: the two
// announcements carry no subjectTTLs entry, so they take TypeData's 5 minutes,
// and one delivered 6 minutes late is dropped by the ingestor before any
// handler runs. A load, clear or count announced during an outage longer than
// that never reaches the Edge.
//
// Verify-red: S5a (UOPAdjustment and BinEpochRefresh become NoExpiry) inverts
// it — the envelope carries no exp and reaches Dispatch at any age.
func TestPin_P0i_LateAnnouncementIsDroppedAtTheEdge(t *testing.T) {
	for _, subject := range []string{SubjectUOPAdjustment, SubjectBinEpochRefresh} {
		if got := DataTTLFor(subject); got != 5*time.Minute {
			t.Errorf("DataTTLFor(%s) = %v, want 5m (TypeData's default)", subject, got)
		}
		ing := NewIngestor(nil)
		dispatched := false
		ing.Dispatch = func(*Envelope) { dispatched = true }
		ing.HandleRaw(announcementAged(t, subject, 6*time.Minute))
		if dispatched {
			t.Errorf("%s delivered 6 minutes late reached Dispatch; at base the ingestor drops it", subject)
		}
	}
}
