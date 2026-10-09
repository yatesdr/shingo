package messaging

import (
	"testing"
	"time"

	"shingo/protocol"
)

// A heartbeat's build that began before a known write finishes after the
// write's push stored its build. The later-started build stays in the memo;
// otherwise the next heartbeats would compare Edges holding the pushed digest
// against the old one and resend the old value.
func TestFeedState_StoreKeepsTheLaterBuild(t *testing.T) {
	t.Parallel()
	f := newFeedState()
	t0 := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "pushed", at: t0.Add(time.Second)})
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "stale", at: t0})
	if e, ok := f.lookup("", protocol.FeedContainment, t0.Add(2*time.Second)); !ok || e.digest != "pushed" {
		t.Errorf("memo = %q (hit %v), want the later build", e.digest, ok)
	}
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "next", at: t0.Add(5 * time.Second)})
	if e, _ := f.lookup("", protocol.FeedContainment, t0.Add(6*time.Second)); e.digest != "next" {
		t.Errorf("memo = %q, want a newer build to replace it", e.digest)
	}
}
