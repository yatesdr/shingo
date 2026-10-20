package router_test

import (
	"encoding/json"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
)

// fakeSubjectPayload stands in for a concrete subject payload type like
// protocol.EdgeRegister or protocol.NodeListResponse.
type fakeSubjectPayload struct {
	StationID string
	Count     int
}

func makeDataEnvelope(t *testing.T, subject string, body fakeSubjectPayload) (*protocol.Envelope, *protocol.Data) {
	t.Helper()
	bodyBytes, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal subject body: %v", err)
	}
	data := &protocol.Data{Subject: subject, Body: bodyBytes}
	env := &protocol.Envelope{
		Type: protocol.TypeData,
		ID:   "test-envelope-id",
		Src:  protocol.Address{Role: "edge", Station: "test-station"},
	}
	return env, data
}

func TestSubjectRegister_DeliversTypedPayloadToHandler(t *testing.T) {
	r := router.NewSubject()

	var got fakeSubjectPayload
	router.RegisterSubject(r, protocol.SubjectEdgeRegister, func(_ *protocol.Envelope, p *fakeSubjectPayload) {
		got = *p
	})

	env, data := makeDataEnvelope(t, protocol.SubjectEdgeRegister, fakeSubjectPayload{StationID: "edge.1", Count: 7})
	r.Dispatch(env, data)

	if got.StationID != "edge.1" || got.Count != 7 {
		t.Errorf("payload not delivered: got %+v, want {StationID:edge.1, Count:7}", got)
	}
}

func TestSubjectRegisterBare_HandlerReceivesEnvelopeOnly(t *testing.T) {
	r := router.NewSubject()

	var seen string
	router.RegisterSubjectBare(r, protocol.SubjectNodeListRequest, func(env *protocol.Envelope) {
		seen = env.ID
	})

	env, data := makeDataEnvelope(t, protocol.SubjectNodeListRequest, fakeSubjectPayload{})
	r.Dispatch(env, data)

	if seen != "test-envelope-id" {
		t.Errorf("bare handler did not run; seen=%q", seen)
	}
}

func TestSubjectDispatch_UnknownSubjectLogsEnvelopeContext(t *testing.T) {
	buf := captureLog(t)
	r := router.NewSubject()
	env, data := makeDataEnvelope(t, "no.such.subject", fakeSubjectPayload{})
	r.Dispatch(env, data)
	got := buf.String()
	for _, want := range []string{"no handler registered for subject", "no.such.subject", "test-envelope-id", "edge/test-station"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected log to contain %q; got %q", want, got)
		}
	}
}

func TestSubjectUse_GlobalMiddlewareRunsAroundHandler(t *testing.T) {
	r := router.NewSubject()

	var order []string
	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		order = append(order, "before")
		next()
		order = append(order, "after")
	})
	router.RegisterSubject(r, protocol.SubjectEdgeRegister, func(_ *protocol.Envelope, _ *fakeSubjectPayload) {
		order = append(order, "handler")
	})

	env, data := makeDataEnvelope(t, protocol.SubjectEdgeRegister, fakeSubjectPayload{})
	r.Dispatch(env, data)

	want := []string{"before", "handler", "after"}
	if len(order) != len(want) {
		t.Fatalf("order = %v, want %v", order, want)
	}
	for i := range want {
		if order[i] != want[i] {
			t.Errorf("order[%d] = %q, want %q", i, order[i], want[i])
		}
	}
}

func TestSubjectUseFor_AppliesOnlyToMatchingSubjects(t *testing.T) {
	r := router.NewSubject()

	var mwCalls, otherHandlerCalls int
	r.UseFor(func(_ *protocol.Envelope, _ any, next func()) {
		mwCalls++
		next()
	}, protocol.SubjectEdgeRegister)

	router.RegisterSubject(r, protocol.SubjectEdgeRegister, func(*protocol.Envelope, *fakeSubjectPayload) {})
	router.RegisterSubject(r, protocol.SubjectEdgeHeartbeat, func(*protocol.Envelope, *fakeSubjectPayload) { otherHandlerCalls++ })

	env1, d1 := makeDataEnvelope(t, protocol.SubjectEdgeRegister, fakeSubjectPayload{})
	env2, d2 := makeDataEnvelope(t, protocol.SubjectEdgeHeartbeat, fakeSubjectPayload{})
	r.Dispatch(env1, d1)
	r.Dispatch(env2, d2)

	if mwCalls != 1 {
		t.Errorf("UseFor middleware ran %d times for matched subject; want 1", mwCalls)
	}
	if otherHandlerCalls != 1 {
		t.Errorf("unrelated handler ran %d times; want 1 (middleware should not block unrelated subjects)", otherHandlerCalls)
	}
}

func TestSubjectRegister_PayloadDecodeFailureSkipsHandler(t *testing.T) {
	r := router.NewSubject()

	handlerRan := false
	router.RegisterSubject(r, protocol.SubjectEdgeRegister, func(_ *protocol.Envelope, _ *fakeSubjectPayload) {
		handlerRan = true
	})

	env := &protocol.Envelope{Type: protocol.TypeData}
	data := &protocol.Data{Subject: protocol.SubjectEdgeRegister, Body: []byte("not valid json")}
	r.Dispatch(env, data)

	if handlerRan {
		t.Error("handler should not run when subject body decode fails")
	}
}

func TestSubjectHas_ReturnsTrueForRegisteredFalseOtherwise(t *testing.T) {
	r := router.NewSubject()
	router.RegisterSubject(r, protocol.SubjectEdgeRegister, func(*protocol.Envelope, *fakeSubjectPayload) {})

	if !r.Has(protocol.SubjectEdgeRegister) {
		t.Error("Has(registered) should be true")
	}
	if r.Has(protocol.SubjectEdgeHeartbeat) {
		t.Error("Has(unregistered) should be false")
	}
}

// TestUnknownSubjectIsDroppedAndTheNextIsHandled is what an OLDER Core does
// with a subject or an envelope type it has never heard of, end to end from
// the raw bytes: the ingestor decodes it, the routers log "no handler
// registered" and return, and the next message is handled. Nothing in that
// path returns an error or blocks, so Core's read loop (messaging.Client
// readLoop) commits the offset past it like any handled message, and the Edge
// outbox has already acked the row on the broker's publish ack
// (outbox.Drainer.publishOne), not on a Core reply. An unknown kind is lost,
// not stuck. The subject is spelled as a literal so the pin holds a Core built
// before protocol.SubjectHoldDropped existed.
func TestUnknownSubjectIsDroppedAndTheNextIsHandled(t *testing.T) {
	buf := captureLog(t)
	src := protocol.Address{Role: protocol.RoleEdge, Station: "plant-a.line-1"}
	dst := protocol.Address{Role: protocol.RoleCore}

	// An older Core's tables: the subject it knows, and none of the new ones.
	subjects := router.NewSubject()
	var handled []string
	router.RegisterSubject(subjects, protocol.SubjectLinesideLevelReport, func(env *protocol.Envelope, _ *protocol.LinesideLevelReport) {
		handled = append(handled, env.ID)
	})
	types := router.New[string]()
	router.Register(types, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { subjects.Dispatch(env, p) })
	ing := protocol.NewIngestor(nil)
	ing.Dispatch = func(env *protocol.Envelope) { types.Dispatch(env, env.Type) }

	raw := func(env *protocol.Envelope, err error) []byte {
		t.Helper()
		if err != nil {
			t.Fatalf("build envelope: %v", err)
		}
		b, err := json.Marshal(env)
		if err != nil {
			t.Fatalf("marshal envelope: %v", err)
		}
		return b
	}
	drop := raw(protocol.NewDataEnvelope("inventory.hold_dropped", src, dst,
		map[string]any{"node": "ALN_011", "payload_code": "SYN44-PARTA.06", "qty": 3, "reason": "sweep"}))
	unknownType := raw(protocol.NewEnvelope("order.not_yet_invented", src, dst, map[string]any{"x": 1}))
	report := protocol.NewDataEnvelope
	next, err := report(protocol.SubjectLinesideLevelReport, src, dst, &protocol.LinesideLevelReport{Station: "plant-a.line-1"})
	nextRaw := raw(next, err)

	ing.HandleRaw(drop)
	ing.HandleRaw(unknownType)
	ing.HandleRaw(nextRaw)

	if len(handled) != 1 || handled[0] != next.ID {
		t.Fatalf("handled = %v, want only the lineside report %s after the two unknown messages", handled, next.ID)
	}
	got := buf.String()
	for _, want := range []string{
		"no handler registered for subject inventory.hold_dropped",
		"no handler registered for key order.not_yet_invented",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("log = %q, want it to contain %q (the unknown message must be dropped visibly, not silently)", got, want)
		}
	}
}
