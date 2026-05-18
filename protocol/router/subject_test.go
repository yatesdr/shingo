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
