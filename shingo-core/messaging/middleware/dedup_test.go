//go:build docker

package middleware_test

import (
	"testing"

	"shingo/protocol"
	"shingocore/internal/testdb"
	"shingocore/messaging/middleware"
)

// TestInboxDedup_DuplicateReplay_DroppedSecondCall mirrors the legacy
// TestInboxDedup_HandleOrderRequest_GatedByDedup: when the same
// envelope ID hits the middleware twice, the second call is dropped
// and next() is not invoked. RecordInboundMessage's UNIQUE-on-id
// constraint is the gate.
func TestInboxDedup_DuplicateReplay_DroppedSecondCall(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	mw := middleware.NewInboxDedup(db, nil)

	env := &protocol.Envelope{
		ID:   "test-dedup-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore, Station: "core"},
	}

	calls := 0
	next := func() { calls++ }

	mw(env, protocol.TypeOrderRequest, next)
	mw(env, protocol.TypeOrderRequest, next)

	if calls != 1 {
		t.Errorf("middleware forwarded %d times for duplicate envelope; want 1", calls)
	}
}

// TestInboxDedup_EmptyEnvelopeID_AlwaysForwards preserves the pre-
// middleware quirk: an envelope with no ID skips the inbox write and
// always forwards. This lets internally-synthesized envelopes (tests,
// CLI harnesses) through without pretending they have uniqueness.
// db is nil because the empty-ID path doesn't touch it.
func TestInboxDedup_EmptyEnvelopeID_AlwaysForwards(t *testing.T) {
	t.Parallel()
	mw := middleware.NewInboxDedup(nil, nil)

	env := &protocol.Envelope{
		ID:   "",
		Type: protocol.TypeOrderCancel,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
	}

	calls := 0
	next := func() { calls++ }

	mw(env, protocol.TypeOrderCancel, next)
	mw(env, protocol.TypeOrderCancel, next)
	mw(env, protocol.TypeOrderCancel, next)

	if calls != 3 {
		t.Errorf("middleware forwarded %d times for empty-ID envelope; want 3", calls)
	}
}

// TestInboxDedup_NilEnvelope_AlwaysForwards covers the defensive nil
// branch. The pre-middleware decorator handled it the same way; this
// pins parity.
func TestInboxDedup_NilEnvelope_AlwaysForwards(t *testing.T) {
	t.Parallel()
	mw := middleware.NewInboxDedup(nil, nil)

	calls := 0
	next := func() { calls++ }

	mw(nil, protocol.TypeOrderCancel, next)
	if calls != 1 {
		t.Errorf("middleware did not forward for nil envelope; want 1 call, got %d", calls)
	}
}

// TestInboxDedup_DistinctIDs_BothForward verifies the dedup keys on
// envelope ID specifically, not on Type or Src — two envelopes with
// different IDs but same Type both forward.
func TestInboxDedup_DistinctIDs_BothForward(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	mw := middleware.NewInboxDedup(db, nil)

	calls := 0
	next := func() { calls++ }

	env1 := &protocol.Envelope{
		ID: "dedup-distinct-1", Type: protocol.TypeOrderRequest,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
	}
	env2 := &protocol.Envelope{
		ID: "dedup-distinct-2", Type: protocol.TypeOrderRequest,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
	}

	mw(env1, protocol.TypeOrderRequest, next)
	mw(env2, protocol.TypeOrderRequest, next)

	if calls != 2 {
		t.Errorf("middleware forwarded %d times for distinct IDs; want 2", calls)
	}
}

// TestInboxDedup_DuplicateLogged exercises the dbg parameter — when
// supplied, a duplicate replay logs the dropped envelope so ops can
// see it. Captured via a closure rather than the global log writer.
func TestInboxDedup_DuplicateLogged(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	var logs []string
	mw := middleware.NewInboxDedup(db, func(format string, args ...any) {
		logs = append(logs, format)
	})

	env := &protocol.Envelope{
		ID: "dedup-log-1", Type: protocol.TypeOrderRequest,
		Src: protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
	}

	mw(env, protocol.TypeOrderRequest, func() {})
	mw(env, protocol.TypeOrderRequest, func() {})

	if len(logs) != 1 {
		t.Errorf("expected 1 dup-log line on second call; got %d (logs: %v)", len(logs), logs)
	}
}
