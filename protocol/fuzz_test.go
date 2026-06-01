package protocol

import (
	"encoding/json"
	"testing"
	"time"
)

// FuzzEnvelopeDecode verifies that arbitrary byte input does not panic
// or hang during two-phase envelope decode. The ingestor processes raw
// bytes from Kafka, so malformed or adversarial input must be rejected
// gracefully.
func FuzzEnvelopeDecode(f *testing.F) {
	// Seed with valid envelope.
	validEnv, _ := NewEnvelope(TypeOrderRequest,
		Address{Role: RoleEdge, Station: "line-1"},
		Address{Role: RoleCore},
		OrderRequest{PayloadCode: "PART-A", DeliveryNode: "LINE1-IN"},
	)
	validBytes, _ := validEnv.Encode()
	f.Add(validBytes)

	// Minimal valid envelope with empty payload.
	f.Add([]byte(`{"v":1,"type":"order.request","id":"test-id","src":{"role":"edge","station":"line-1"},"dst":{"role":"core"},"ts":"2026-01-01T00:00:00Z","exp":"2099-01-01T00:00:00Z","p":{}}`))

	// Edge cases.
	f.Add([]byte(``))
	f.Add([]byte(`not json`))
	f.Add([]byte(`{}`))
	f.Add([]byte(`{"v":0,"type":"","id":"","src":{},"dst":{},"p":null}`))
	f.Add([]byte(`{"v":1,"type":"\xff\xfe","p":{}}`))
	f.Add(make([]byte, 1<<20))

	f.Fuzz(func(t *testing.T, data []byte) {
		var hdr RawHeader
		_ = json.Unmarshal(data, &hdr)

		var env Envelope
		_ = json.Unmarshal(data, &env)

		var target map[string]any
		_ = env.DecodePayload(&target)
	})
}

// FuzzIngestorHandleRaw verifies the full ingestor pipeline does not
// panic on arbitrary input. Uses NoOpHandler so no side effects occur.
func FuzzIngestorHandleRaw(f *testing.F) {
	validEnv, _ := NewEnvelope(TypeOrderAck,
		Address{Role: RoleCore},
		Address{Role: RoleEdge, Station: "line-1"},
		OrderAck{OrderUUID: "test-uuid", ShingoOrderID: 1},
	)
	validBytes, _ := validEnv.Encode()
	f.Add(validBytes)

	f.Add([]byte(``))
	f.Add([]byte(`{}`))
	f.Add([]byte(`not json at all`))
	f.Add([]byte(`{"v":1,"type":"unknown.type","id":"x","src":{"role":"edge"},"dst":{"role":"core"},"ts":"2026-01-01T00:00:00Z","exp":"2099-01-01T00:00:00Z","p":{}}`))

	f.Fuzz(func(t *testing.T, data []byte) {
		ing := NewIngestor(nil)
		ing.HandleRaw(data)
	})
}

// FuzzStatusTransition verifies that IsValidTransition handles arbitrary
// string inputs without panicking. Status values come from JSON decode
// over the wire and could be any string.
func FuzzStatusTransition(f *testing.F) {
	for _, s := range AllStatuses() {
		f.Add(string(s), string(StatusPending))
		f.Add(string(StatusPending), string(s))
	}
	f.Add("garbage", "also_garbage")
	f.Add("", "")
	f.Add("pending", "delivered")
	f.Add("confirmed", "pending")

	f.Fuzz(func(t *testing.T, from, to string) {
		sFrom := Status(from)
		sTo := Status(to)
		_ = IsValidTransition(sFrom, sTo)
		_ = IsTerminal(sFrom)
		_ = sFrom.CanTransitionTo(sTo)
		_ = sFrom.IsTerminal()
	})
}

// FuzzExpiry verifies that IsExpired and IsExpiredHeader handle
// arbitrary timestamp values without panicking.
func FuzzExpiry(f *testing.F) {
	f.Add(int64(0))
	f.Add(int64(1))
	f.Add(int64(-1))
	f.Add(int64(253402300799))
	f.Add(int64(-62135596801))

	f.Fuzz(func(t *testing.T, expUnix int64) {
		expTime := time.Unix(expUnix, 0).UTC().Format(time.RFC3339)
		data := []byte(`{"v":1,"type":"order.request","id":"fuzz","src":{"role":"edge","station":"x"},"dst":{"role":"core"},"ts":"2026-01-01T00:00:00Z","exp":"` + expTime + `","p":{}}`)

		var hdr RawHeader
		_ = json.Unmarshal(data, &hdr)
		_ = IsExpiredHeader(&hdr)

		var env Envelope
		_ = json.Unmarshal(data, &env)
		_ = IsExpired(&env)

		env.ExpiresAt = time.Time{}
		_ = IsExpired(&env)
		hdr.ExpiresAt = time.Time{}
		_ = IsExpiredHeader(&hdr)
	})
}
