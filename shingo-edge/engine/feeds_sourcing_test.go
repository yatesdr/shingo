package engine

import (
	"testing"

	"shingo/protocol"
)

// TestSourcingFeed_TheHeartbeatQuotesTheCache: FeedSourcing's digest is this
// Edge's own cache digested with Core's function. It is named before any ack
// (the boot burst's heartbeat carries it), it equals Core's digest of the same
// verdicts, an apply records when the copy came (CurrentAt reads it), and a
// cache that missed a delta quotes a different digest than the Core that sent
// it, so the next heartbeat compare sends the snapshot.
func TestSourcingFeed_TheHeartbeatQuotesTheCache(t *testing.T) {
	t.Parallel()
	eng := testEngine(t, testEngineDB(t))
	empty, err := protocol.SourcingDigest(nil)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if d, ok := eng.FeedDigests()[protocol.FeedSourcing]; !ok || d != empty {
		t.Fatalf("before any ack or copy: heartbeat sourcing = %q (named %v), want named, the empty cache's %q", d, ok, empty)
	}
	if received, _ := eng.FeedTimes(protocol.FeedSourcing); !received.IsZero() {
		t.Fatalf("nothing applied yet, but the copy has a received time %v", received)
	}

	snapshot := []protocol.SourcingState{
		{ProcessID: "P1", StyleID: "A", Status: "green", Reason: "Can change over."},
		{ProcessID: "P1", StyleID: "B", Status: "red", Missing: []string{"SYN-X"}, Reason: "Cannot change over — missing SYN-X."},
	}
	eng.HandleSourcingState(protocol.SourcingStateReport{Snapshot: true, States: snapshot})
	want, err := protocol.SourcingDigest(snapshot)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got := eng.FeedDigests()[protocol.FeedSourcing]; got != want {
		t.Errorf("after the snapshot: heartbeat sourcing = %q, want Core's digest of the same verdicts %q", got, want)
	}
	if received, _ := eng.FeedTimes(protocol.FeedSourcing); received.IsZero() {
		t.Error("the snapshot applied, but the copy has no received time")
	}

	// Core moves P1/B to green and pushes the delta; this Edge never gets it.
	moved := []protocol.SourcingState{snapshot[0], {ProcessID: "P1", StyleID: "B", Status: "green", Reason: "Can change over."}}
	coreNow, err := protocol.SourcingDigest(moved)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got := eng.FeedDigests()[protocol.FeedSourcing]; got == coreNow {
		t.Error("a cache that missed a delta quotes Core's current digest; the compare would never heal it")
	}
	eng.HandleSourcingState(protocol.SourcingStateReport{States: moved[1:]})
	if got := eng.FeedDigests()[protocol.FeedSourcing]; got != coreNow {
		t.Errorf("after the delta: heartbeat sourcing = %q, want %q", got, coreNow)
	}
}
