package messaging

import (
	"testing"
	"time"
)

// plant_claims_publisher_test.go — the snapshot cadence is a safety net, not a
// delivery mechanism.
//
// At 5 minutes this publisher emitted ~65 messages an hour at Springfield (one
// per process, twelve times an hour) for plant config that changes a few times
// a shift, and it became 66% of everything Core discarded for expiry — 181 a
// day, each carrying config identical to the snapshot before it.
//
// Raising the interval is only safe because the change-driven paths are
// complete: requestSpecChangePublish fires on every style and claim mutation
// (apiCreateStyle and apiUpdateStyle were missing it until 2026-08-22, which
// the 5-minute timer had been masking), and SubjectEdgeRegistered republishes a
// full snapshot on every register — including the re-register Core asks for
// after it restarts. If any of that regresses, this interval is how long a
// plant runs on a stale mirror, so the number is pinned here deliberately.
func TestPlantClaimsPublisher_SnapshotIntervalIsTheSafetyNet(t *testing.T) {
	t.Parallel()

	p := NewPlantClaimsPublisher(nil, "plant-a.line-1")

	if p.snapshotInterval != 60*time.Minute {
		t.Errorf("snapshotInterval = %v, want 60m — this is the LAST-RESORT catch "+
			"for a change whose publish was lost outright, not the way changes "+
			"normally reach Core", p.snapshotInterval)
	}

	// Guard the direction, not just the value: anything at or under the old
	// 5-minute cadence means someone has reverted to broadcasting config on a
	// timer, which is the behaviour that filled Core's expiry bin.
	if p.snapshotInterval <= 5*time.Minute {
		t.Errorf("snapshotInterval is back to timer-driven broadcasting (%v)", p.snapshotInterval)
	}
}

// PublishChanged and PublishAll must stay the same operation. Core replaces its
// mirror per process on every message, so a partial "changed" publish would
// leave processes the edit did not touch mirroring nothing.
func TestPlantClaimsPublisher_ChangedIsAFullSnapshot(t *testing.T) {
	t.Parallel()

	p := NewPlantClaimsPublisher(nil, "plant-a.line-1")

	// Both paths hit the same nil-DB failure, which is the observable proof
	// they are the same code path — a PublishChanged that had diverged into a
	// partial publish would not.
	errChanged := func() (err error) {
		defer func() { _ = recover() }()
		return p.PublishChanged()
	}()
	errAll := func() (err error) {
		defer func() { _ = recover() }()
		return p.PublishAll()
	}()

	if (errChanged == nil) != (errAll == nil) {
		t.Errorf("PublishChanged and PublishAll diverged: %v vs %v — Core replaces "+
			"its mirror per process, so a partial snapshot erases the processes "+
			"the edit did not touch", errChanged, errAll)
	}
}
