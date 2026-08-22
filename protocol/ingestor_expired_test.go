package protocol

import (
	"encoding/json"
	"io"
	"log"
	"strings"
	"testing"
	"time"
)

// captureLog redirects the standard logger for one test.
//
// Restores the PREVIOUS writer, never nil: log.SetOutput(nil) leaves the global
// logger writing to a nil io.Writer and the next log call in the process
// segfaults. That is not hypothetical here — it is how the plant.claims docker
// crash was caused.
func captureLog(t *testing.T, w io.Writer) func() {
	t.Helper()
	prev := log.Writer()
	flags := log.Flags()
	log.SetOutput(w)
	log.SetFlags(0)
	return func() {
		log.SetOutput(prev)
		log.SetFlags(flags)
	}
}

// ingestor_expired_test.go — the expiry drop is counted and names its subject.
//
// This was the larger of the two silent loss channels and nothing observed it:
// one log line carrying an envelope UUID and `type=data`, no counter anywhere.
// Springfield discarded 797 envelopes on 2026-08-20 and 544 on 2026-08-21 with
// no signal, while the sending edge recorded every one as published.

func expiredRawEnvelope(t *testing.T, subject string, age time.Duration) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{"bin_id": 27, "delta": -3})
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	payload, err := json.Marshal(&Data{Subject: subject, Body: body})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	now := time.Now().UTC()
	env := Envelope{
		Version:   Version,
		Type:      TypeData,
		ID:        "test-envelope-id",
		Src:       Address{Role: RoleEdge, Station: "plant-a.line-1"},
		Dst:       Address{Role: RoleCore},
		Timestamp: now.Add(-age),
		ExpiresAt: now.Add(-age), // already lapsed
		Payload:   payload,
	}
	raw, err := json.Marshal(&env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	return raw
}

func TestIngestor_ExpiredDropIsCounted(t *testing.T) {
	before := ExpiredDrops()

	ing := NewIngestor(nil)
	dispatched := false
	ing.Dispatch = func(*Envelope) { dispatched = true }

	ing.HandleRaw(expiredRawEnvelope(t, SubjectPlantClaims, time.Hour))

	if dispatched {
		t.Fatal("an expired envelope reached Dispatch — the header gate must drop it " +
			"before any handler runs")
	}
	if got := ExpiredDrops() - before; got != 1 {
		t.Errorf("ExpiredDrops delta = %d, want 1 — without a counter this loss "+
			"channel is invisible on /status", got)
	}
}

// TestSubjectOf pins the part that makes the log line useful. The subject is
// the only field that says whether a dropped message mattered — a lineside
// snapshot is superseded within 60s, a bin_uop_delta is a permanently wrong
// count — and it is NOT on RawHeader, so it takes a second decode.
func TestSubjectOf(t *testing.T) {
	t.Parallel()

	raw := expiredRawEnvelope(t, SubjectBinUOPDelta, time.Minute)
	if got := subjectOf(raw); got != SubjectBinUOPDelta {
		t.Errorf("subjectOf = %q, want %q", got, SubjectBinUOPDelta)
	}

	// Undecodable and non-data envelopes degrade to "" rather than failing the
	// drop path — the message is being discarded either way, and a panic or an
	// early return here would lose the log line as well as the message.
	if got := subjectOf([]byte("{not json")); got != "" {
		t.Errorf("subjectOf(garbage) = %q, want empty", got)
	}
	if got := subjectOf([]byte(`{"v":1,"type":"order.cancel","p":{"order_id":7}}`)); got != "" {
		t.Errorf("subjectOf(non-data) = %q, want empty", got)
	}
}

// TestIngestor_ExpiredDropLogsSubject is the regression guard for the log line
// itself: it used to carry only the UUID and type, which is why the Springfield
// investigation could count the drops but never say what was lost.
func TestIngestor_ExpiredDropLogsSubject(t *testing.T) {
	var buf strings.Builder
	defer captureLog(t, &buf)()

	ing := NewIngestor(nil)
	ing.HandleRaw(expiredRawEnvelope(t, SubjectBinUOPDelta, 90*time.Minute))

	line := buf.String()
	if !strings.Contains(line, "subject="+SubjectBinUOPDelta) {
		t.Errorf("drop line does not name the subject: %q", line)
	}
	if !strings.Contains(line, "expired") || !strings.Contains(line, "ago") {
		t.Errorf("drop line does not report how late the message was: %q", line)
	}
}
