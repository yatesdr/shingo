package protocol

import (
	"encoding/json"
	"sort"
	"strings"
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

	for _, subject := range []string{SubjectBinUOPDelta, SubjectLinesideBucketLevel, SubjectHoldDropped} {
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

// TestNewDataEnvelope_SnapshotSubjectsStillExpire pins the other half: a
// snapshot subject that stopped expiring would publish an hour of superseded
// history after every outage. (It said only the two sequenced deltas are
// exempt; nine data subjects are, listed in noExpiryWant.)
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
		t.Fatal("plant.claims has no expiry — it is a snapshot and must keep the data channel's TTL")
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

// TestNewEnvelope_OrderIngestCarriesNoExpiry: a produce ingest is a full bin's
// count, and Core fences a late one by its bin_epoch, so an ingest drained
// minutes or hours late must still apply (S1b; it used to expire at 10 min).
func TestNewEnvelope_OrderIngestCarriesNoExpiry(t *testing.T) {
	t.Parallel()
	// The release is durable for the same reason (R4-8).
	if rel, err := NewEnvelope(TypeOrderRelease, Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore}, &OrderRelease{OrderUUID: "u"}); err != nil || !rel.ExpiresAt.IsZero() {
		t.Fatalf("order.release ExpiresAt = %v (err %v), want zero (never expires)", rel.ExpiresAt, err)
	}
	// And the cancel: a dropped one leaves Core's robots parked under an Edge
	// row already cancelled, which nothing re-asks about.
	if cxl, err := NewEnvelope(TypeOrderCancel, Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore}, &OrderCancel{OrderUUID: "u", Reason: CancelReasonLineIdle}); err != nil || !cxl.ExpiresAt.IsZero() {
		t.Fatalf("order.cancel ExpiresAt = %v (err %v), want zero (never expires)", cxl.ExpiresAt, err)
	}
	env, err := NewEnvelope(TypeOrderIngest, Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Address{Role: RoleCore}, &OrderIngestRequest{OrderUUID: "u", PayloadCode: "PART-X", Quantity: 47})
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	if !env.ExpiresAt.IsZero() {
		t.Fatalf("order.ingest ExpiresAt = %v, want zero (never expires)", env.ExpiresAt)
	}
	// The header's exp is all the reader checks: zero reads as never, at any age.
	hdr := RawHeader{Type: TypeOrderIngest, ExpiresAt: env.ExpiresAt}
	if IsExpiredHeader(&hdr) {
		t.Error("an order.ingest header reads expired")
	}
}

// TestNewDataEnvelope_BinPickedUpCarriesNoExpiry: a bin's lift is durable
// (R4-7): a copy dropped after an outage strands the slot's clear and a
// changeover supply deferred to it.
func TestNewDataEnvelope_BinPickedUpCarriesNoExpiry(t *testing.T) {
	t.Parallel()
	env, err := NewDataEnvelope(SubjectBinPickedUp, Address{Role: RoleCore}, Address{Role: RoleEdge, Station: "plant-a.line-1"},
		&BinPickedUp{OrderUUID: "u", BinID: 9, Location: "SYN-PRESS"})
	if err != nil {
		t.Fatalf("NewDataEnvelope: %v", err)
	}
	if !env.ExpiresAt.IsZero() || IsExpired(env) {
		t.Fatalf("transit.bin_picked_up ExpiresAt = %v, want zero (never expires)", env.ExpiresAt)
	}
}

// TestNewDataEnvelope_BinLandedCarriesNoExpiry: a carrier's drop is durable
// (Lane H, H.6): a copy dropped after an outage leaves a seat empty while a
// carrier stands on it. A late copy is refused by the seat's placement stamp.
func TestNewDataEnvelope_BinLandedCarriesNoExpiry(t *testing.T) {
	t.Parallel()
	env, err := NewDataEnvelope(SubjectBinLanded, Address{Role: RoleCore}, Address{Role: RoleEdge, Station: "plant-a.line-1"},
		&BinLanded{BinID: 9, CoreNodeName: "SYN-PRESS", Count: 40, Epoch: 2, PlacedSeq: 7})
	if err != nil {
		t.Fatalf("NewDataEnvelope: %v", err)
	}
	if !env.ExpiresAt.IsZero() || IsExpired(env) {
		t.Fatalf("transit.bin_landed ExpiresAt = %v, want zero (never expires)", env.ExpiresAt)
	}
}

// noExpiryWant is the never-expire set: the base's (v0.18.0, d2686a1d), then
// each message the must-arrive table has added since, marked where it is: the
// data subjects and the envelope types whose envelopes carry no `exp`. These
// keys are wire names, the same on both sides; the outbox's spellings are the
// drainer's pin's (outbox.TestNeverDeadLetter_ExactMembership). Literal
// strings, so a respelled subject fails here too.
var noExpiryWant = struct{ subjects, types []string }{
	subjects: []string{
		"demand.origin", // added by the must-arrive table: late-safe by its revision
		"edge.answer",
		"inventory.bin_epoch_refresh",
		"inventory.bin_uop_delta",
		"inventory.hold_dropped",
		"inventory.lineside_bucket_level",
		"inventory.uop_adjustment",
		"production.ticks",
		"transit.bin_landed",
		"transit.bin_picked_up",
	},
	types: []string{
		"order.cancel",
		"order.ingest",
		// Added by the must-arrive table (final brief 5i): a lost confirm left
		// Core reading delivered until its sweep, and for good on an order the
		// sweep skips; a late one confirms only a delivered order.
		"order.receipt",
		"order.release",
	},
}

// TestNoExpiry_ExactMembership pins which messages carry no `exp`, by building
// each one: every subject in AllSubjects through NewDataEnvelope, every type in
// AllTypes and EphemeralTypes through NewEnvelope. The ones stamped with no
// expiry must be exactly noExpiryWant, no more and no fewer.
//
// Pin 12 of the info-flow build's Step 0. The must-arrive table (lane B2)
// derives this set from one table and keeps this membership in its first
// commit; each later commit that changes membership changes noExpiryWant
// in the same commit and says why.
func TestNoExpiry_ExactMembership(t *testing.T) {
	t.Parallel()
	src := Address{Role: RoleEdge, Station: "plant-a.line-1"}
	dst := Address{Role: RoleCore}
	var subjects, types []string
	for _, s := range AllSubjects() {
		env, err := NewDataEnvelope(s, src, dst, map[string]any{"bin_id": 27})
		if err != nil {
			t.Fatalf("NewDataEnvelope(%s): %v", s, err)
		}
		if env.ExpiresAt.IsZero() {
			subjects = append(subjects, s)
		}
	}
	for _, ty := range append(AllTypes(), EphemeralTypes()...) {
		env, err := NewEnvelope(ty, src, dst, map[string]any{"order_uuid": "u"})
		if err != nil {
			t.Fatalf("NewEnvelope(%s): %v", ty, err)
		}
		if env.ExpiresAt.IsZero() {
			types = append(types, ty)
		}
	}
	if len(subjects) == len(AllSubjects()) || len(types) == len(AllTypes())+len(EphemeralTypes()) {
		t.Fatal("every message read as never expiring: the stamp is not being read, so this pin has no input")
	}
	sort.Strings(subjects)
	sort.Strings(types)
	if got, want := strings.Join(subjects, " "), strings.Join(noExpiryWant.subjects, " "); got != want {
		t.Errorf("never-expire data subjects = [%s], want [%s]. If the change is meant, change "+
			"noExpiryWant in the same commit and say why in the body", got, want)
	}
	if got, want := strings.Join(types, " "), strings.Join(noExpiryWant.types, " "); got != want {
		t.Errorf("never-expire envelope types = [%s], want [%s]. If the change is meant, change "+
			"noExpiryWant in the same commit and say why in the body", got, want)
	}
}

// TestMustArrive_EveryRowIsAMessageAndSaysWhy holds the must-arrive table's
// rows to what the two derived lists rely on: each names a real subject or
// type, once; it takes at least one of the two mechanisms; it says what a
// lost copy costs and what makes a late one safe; and a data subject's sender
// is the side its direction says (a subject Core handles is the Edge's, one the
// Edge handles is Core's), which is what makes its outbox key the spelling that
// side's row carries. Envelope types are keyed bare on either side, so their
// sender decides nothing and is not checked here.
func TestMustArrive_EveryRowIsAMessageAndSaysWhy(t *testing.T) {
	t.Parallel()
	in := func(list []string, s string) bool {
		for _, x := range list {
			if x == s {
				return true
			}
		}
		return false
	}
	rows := MustArriveTable()
	if len(rows) == 0 {
		t.Fatal("the must-arrive table is empty: nothing below was checked")
	}
	seen := map[string]bool{}
	for _, m := range rows {
		if seen[m.Name] {
			t.Errorf("%s has two rows", m.Name)
		}
		seen[m.Name] = true
		switch {
		case m.Type && !in(AllTypes(), m.Name):
			t.Errorf("%s is marked a type but is not in AllTypes", m.Name)
		case !m.Type && !in(AllSubjects(), m.Name):
			t.Errorf("%s is marked a data subject but is not in AllSubjects", m.Name)
		case !m.Type && in(CoreInboundSubjects(), m.Name) && m.Sender != SentByEdge:
			t.Errorf("%s is a subject Core handles, so the Edge sends it; its row says sender %d", m.Name, m.Sender)
		case !m.Type && in(EdgeInboundSubjects(), m.Name) && m.Sender == SentByEdge:
			t.Errorf("%s is a subject the Edge handles, so Core sends it; its row says the Edge", m.Name)
		}
		if !m.NeverExpires && !m.NeverDeadLetters {
			t.Errorf("%s takes neither mechanism: it is not must-arrive, so it has no row", m.Name)
		}
		if strings.TrimSpace(m.Lost) == "" || strings.TrimSpace(m.Late) == "" {
			t.Errorf("%s does not say what a lost copy costs (%q) and what makes a late one safe (%q)", m.Name, m.Lost, m.Late)
		}
	}
}
