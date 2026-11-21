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
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "pushed", at: t0.Add(time.Second)}, f.dropCount())
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "stale", at: t0}, f.dropCount())
	if e, ok := f.lookup("", protocol.FeedContainment, t0.Add(2*time.Second)); !ok || e.digest != "pushed" {
		t.Errorf("memo = %q (hit %v), want the later build", e.digest, ok)
	}
	f.store("", protocol.FeedContainment, feedMemoEntry{digest: "next", at: t0.Add(5 * time.Second)}, f.dropCount())
	if e, _ := f.lookup("", protocol.FeedContainment, t0.Add(6*time.Second)); e.digest != "next" {
		t.Errorf("memo = %q, want a newer build to replace it", e.digest)
	}
}

// TestFeedState_ABuildStartedBeforeADropIsNotKept: a heartbeat's build reads
// before a known write and finishes after the write's drop (a reply's, a
// payload write's). It is not stored: kept, the memo would hold the digest
// from before the write, and the next heartbeats would send that old value to
// an Edge holding the new one until the memo aged out. A build that started
// after the drop is kept. Each drop counts for its own key: one station's,
// the plant's, and every station's at once, a station that had no memo when
// it was dropped included.
func TestFeedState_ABuildStartedBeforeADropIsNotKept(t *testing.T) {
	t.Parallel()
	t0 := time.Date(2026, 11, 21, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name    string
		station string
		key     string
		drop    func(f *feedState)
	}{
		{"one station's node list", "edge.a", protocol.FeedNodes, func(f *feedState) { f.dropStation("edge.a", protocol.FeedNodes) }},
		{"the plant's catalog", "", protocol.FeedCatalog, func(f *feedState) { f.drop(protocol.FeedCatalog) }},
		{"every station's node list", "edge.b", protocol.FeedNodes, func(f *feedState) { f.dropEveryStation(protocol.FeedNodes) }},
	}
	for _, c := range cases {
		f := newFeedState()
		started := f.dropCount()
		c.drop(f)
		f.store(c.station, c.key, feedMemoEntry{digest: "read before the write", at: t0}, started)
		if e, ok := f.lookup(c.station, c.key, t0.Add(time.Second)); ok {
			t.Errorf("%s: the memo kept %q, a build that started before the drop", c.name, e.digest)
		}
		f.store(c.station, c.key, feedMemoEntry{digest: "read after the write", at: t0.Add(time.Second)}, f.dropCount())
		if e, ok := f.lookup(c.station, c.key, t0.Add(2*time.Second)); !ok || e.digest != "read after the write" {
			t.Errorf("%s: memo = %q (hit %v), want the build that started after the drop", c.name, e.digest, ok)
		}
	}

	// A drop of another key, or of another station's memo, leaves a build alone.
	f := newFeedState()
	started := f.dropCount()
	f.dropStation("edge.other", protocol.FeedNodes)
	f.drop(protocol.FeedCatalog)
	f.store("edge.a", protocol.FeedNodes, feedMemoEntry{digest: "n1", at: t0}, started)
	if e, ok := f.lookup("edge.a", protocol.FeedNodes, t0.Add(time.Second)); !ok || e.digest != "n1" {
		t.Errorf("another key's drop: memo = %q (hit %v), want the build kept", e.digest, ok)
	}
}
