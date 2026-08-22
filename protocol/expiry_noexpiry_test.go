package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// expiry_noexpiry_test.go — subjects marked NoExpiry must carry no `exp`.
//
// The trap this guards is arithmetic, not policy: a zero TTL run through
// now.Add(ttl) stamps exp = now, and IsExpiredHeader then reports the envelope
// expired on the very next clock tick — the exact inverse of "never expires".
// Every assertion here fails if someone reintroduces the bare Add.

func TestNewDataEnvelope_DeltaSubjectsCarryNoExpiry(t *testing.T) {
	t.Parallel()
	src := Address{Role: RoleEdge, Station: "plant-a.line-1"}
	dst := Address{Role: RoleCore}

	for _, subject := range []string{SubjectBinUOPDelta, SubjectLinesideBucketDelta} {
		env, err := NewDataEnvelope(subject, src, dst, map[string]any{"bin_id": 27, "delta": -3})
		if err != nil {
			t.Fatalf("NewDataEnvelope(%s): %v", subject, err)
		}
		if !env.ExpiresAt.IsZero() {
			t.Fatalf("%s: ExpiresAt = %v, want zero — a NoExpiry subject must not be "+
				"stamped at all; now.Add(0) yields exp=now, which expires immediately",
				subject, env.ExpiresAt)
		}
		if IsExpired(env) {
			t.Errorf("%s: IsExpired = true on a freshly built envelope", subject)
		}

		// Round-trip: a zero time must survive JSON and still read as "never".
		raw, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("%s: marshal: %v", subject, err)
		}
		var hdr RawHeader
		if err := json.Unmarshal(raw, &hdr); err != nil {
			t.Fatalf("%s: unmarshal header: %v", subject, err)
		}
		if !hdr.ExpiresAt.IsZero() {
			t.Errorf("%s: ExpiresAt did not survive the wire as zero: %v", subject, hdr.ExpiresAt)
		}
		if IsExpiredHeader(&hdr) {
			t.Errorf("%s: IsExpiredHeader = true straight off the wire", subject)
		}
	}
}

// TestNoExpiry_SurvivesAnyAge is the one that would have caught the Springfield
// loss: those deltas sat in the outbox for a mean of 142 minutes and a peak of
// 23 hours before publishing, and Core's ingestor discarded every one.
//
// Age is expressed on the header rather than by moving a clock, because the
// SimClock advances with real time and cannot be stepped. The control arm is the
// point: an identical header carrying a real 24h-old exp DOES read expired, so a
// zero ExpiresAt is genuinely special-cased and not just passing vacuously.
func TestNoExpiry_SurvivesAnyAge(t *testing.T) {
	t.Parallel()

	env, err := NewDataEnvelope(SubjectBinUOPDelta,
		Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore},
		map[string]any{"bin_id": 27})
	if err != nil {
		t.Fatalf("NewDataEnvelope: %v", err)
	}

	aged := RawHeader{
		Version:   env.Version,
		Type:      env.Type,
		ID:        env.ID,
		Src:       env.Src,
		Dst:       env.Dst,
		ExpiresAt: env.ExpiresAt, // zero
	}
	if IsExpiredHeader(&aged) {
		t.Fatal("a NoExpiry delta reported expired — the whole point is that a late " +
			"delta is deduped or audited at Core, never discarded at the ingestor")
	}

	// Control: the same header with a real 24h-old expiry must be rejected,
	// proving the assertion above is not passing for the wrong reason.
	aged.ExpiresAt = time.Now().UTC().Add(-24 * time.Hour)
	if !IsExpiredHeader(&aged) {
		t.Fatal("a header expired 24h ago did not read as expired — IsExpiredHeader " +
			"is not doing the work this test assumes")
	}
}

// TestNewDataEnvelope_SnapshotSubjectsStillExpire pins the other half. Only the
// two sequenced deltas are exempt; a snapshot subject that stopped expiring
// would publish an hour of superseded history after every outage.
func TestNewDataEnvelope_SnapshotSubjectsStillExpire(t *testing.T) {
	t.Parallel()
	before := time.Now().UTC()
	env, err := NewDataEnvelope(SubjectPlantClaims,
		Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore},
		map[string]any{"process_id": "SNF2"})
	if err != nil {
		t.Fatalf("NewDataEnvelope: %v", err)
	}
	if env.ExpiresAt.IsZero() {
		t.Fatal("plant.claims has no expiry — only the sequenced deltas are exempt")
	}
	want := 5 * time.Minute
	if got := env.ExpiresAt.Sub(before); got < want-time.Minute || got > want+time.Minute {
		t.Errorf("plant.claims expiry window = %v, want ~%v", got, want)
	}
}

// TestZeroOrDeadline covers the helper directly, including the negative case —
// a scaled TTL should never go negative, but if it did, "already expired" is the
// worst possible reading of it.
func TestZeroOrDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

	if got := zeroOrDeadline(now, 0); !got.IsZero() {
		t.Errorf("zeroOrDeadline(now, 0) = %v, want zero", got)
	}
	if got := zeroOrDeadline(now, -time.Second); !got.IsZero() {
		t.Errorf("zeroOrDeadline(now, -1s) = %v, want zero", got)
	}
	if got := zeroOrDeadline(now, 5*time.Minute); !got.Equal(now.Add(5 * time.Minute)) {
		t.Errorf("zeroOrDeadline(now, 5m) = %v, want %v", got, now.Add(5*time.Minute))
	}
}
