package router_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
)

// captureLog redirects the default logger into a buffer for assertion.
// Restores the previous writer and flags on test cleanup.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevW := log.Writer()
	prevFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(prevW)
		log.SetFlags(prevFlags)
	})
	return &buf
}

// fakePayload is the typed payload used by these tests. Stands in for a
// concrete protocol payload type like protocol.OrderRequest.
type fakePayload struct {
	ID   int64
	Name string
}

// makeEnvelope wraps fakePayload as JSON in a protocol.Envelope so the
// router's Unmarshal path can decode it.
func makeEnvelope(t *testing.T, typ string, p fakePayload) *protocol.Envelope {
	t.Helper()
	body, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return &protocol.Envelope{
		Type:    typ,
		Payload: body,
	}
}

// TestRegister_DeliversTypedPayloadToHandler pins the basic registration
// + dispatch contract: a Register'd handler receives the typed payload
// after the router decodes envelope.Payload.
func TestRegister_DeliversTypedPayloadToHandler(t *testing.T) {
	r := router.New[string]()

	var got fakePayload
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, p *fakePayload) {
		got = *p
	})

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{ID: 42, Name: "test"}), protocol.TypeOrderRequest)

	if got.ID != 42 || got.Name != "test" {
		t.Errorf("payload not delivered: got %+v, want {ID:42, Name:test}", got)
	}
}

// TestDispatch_UnknownKeyLogsEnvelopeContext verifies the no-handler-registered
// path: Dispatch logs (with enough envelope context for ops to correlate)
// and does not panic. Callers should detect missing registrations at boot
// via LogRegistration + a coverage check; the per-message log is the fallback.
func TestDispatch_UnknownKeyLogsEnvelopeContext(t *testing.T) {
	buf := captureLog(t)
	r := router.New[string]()
	env := &protocol.Envelope{
		Type: protocol.TypeOrderRequest,
		ID:   "test-envelope-id",
		Src:  protocol.Address{Role: "edge", Station: "test-station"},
	}
	r.Dispatch(env, protocol.TypeOrderRequest)
	got := buf.String()
	for _, want := range []string{"no handler registered", "test-envelope-id", protocol.TypeOrderRequest, "edge/test-station"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected log to contain %q; got %q", want, got)
		}
	}
}

// TestUse_GlobalMiddlewareRunsAroundHandler pins the global-middleware
// wrap-and-next contract.
func TestUse_GlobalMiddlewareRunsAroundHandler(t *testing.T) {
	r := router.New[string]()

	var order []string
	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		order = append(order, "before")
		next()
		order = append(order, "after")
	})
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {
		order = append(order, "handler")
	})

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{}), protocol.TypeOrderRequest)

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

// TestUse_MiddlewareCanShortCircuit pins that not calling next stops the
// chain before the handler runs. This is the inbox-dedup pattern.
func TestUse_MiddlewareCanShortCircuit(t *testing.T) {
	r := router.New[string]()

	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		// Don't call next.
	})
	handlerRan := false
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {
		handlerRan = true
	})

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{}), protocol.TypeOrderRequest)

	if handlerRan {
		t.Error("short-circuit middleware should prevent handler invocation")
	}
}

// TestUse_MultipleMiddlewareRunInRegistrationOrder pins the chain
// ordering: registration order is execution order, each wrapping the next.
func TestUse_MultipleMiddlewareRunInRegistrationOrder(t *testing.T) {
	r := router.New[string]()

	var order []string
	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		order = append(order, "mw1-before")
		next()
		order = append(order, "mw1-after")
	})
	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		order = append(order, "mw2-before")
		next()
		order = append(order, "mw2-after")
	})
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {
		order = append(order, "handler")
	})

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{}), protocol.TypeOrderRequest)

	want := []string{"mw1-before", "mw2-before", "handler", "mw2-after", "mw1-after"}
	for i, w := range want {
		if i >= len(order) || order[i] != w {
			t.Errorf("order[%d] = %q, want %q (full: %v)", i, safeIndex(order, i), w, order)
		}
	}
}

// TestUseFor_AppliesOnlyToMatchingKeys pins the scoped-middleware
// contract: UseFor middleware runs only when the dispatched key is in
// the listed set. Mirrors the inbox-dedup middleware's intended use —
// applies to order-channel envelopes but not to data subjects.
func TestUseFor_AppliesOnlyToMatchingKeys(t *testing.T) {
	r := router.New[string]()

	var mwCallsForOrder, dataHandlerCalls int
	r.UseFor(func(_ *protocol.Envelope, _ any, next func()) {
		mwCallsForOrder++
		next()
	}, protocol.TypeOrderRequest)

	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {})
	router.Register(r, protocol.TypeData, func(_ *protocol.Envelope, _ *fakePayload) { dataHandlerCalls++ })

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{}), protocol.TypeOrderRequest)
	r.Dispatch(makeEnvelope(t, protocol.TypeData, fakePayload{}), protocol.TypeData)

	if mwCallsForOrder != 1 {
		t.Errorf("UseFor middleware ran %d times for matched key; want 1", mwCallsForOrder)
	}
	if dataHandlerCalls != 1 {
		t.Errorf("data handler ran %d times; want 1 (middleware should not block unrelated keys)", dataHandlerCalls)
	}
}

// TestKeys_ReturnsRegisteredKeys pins the coverage-assertion shape:
// callers walk Keys() to verify their handler-registration is complete.
func TestKeys_ReturnsRegisteredKeys(t *testing.T) {
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, func(*protocol.Envelope, *fakePayload) {})
	router.Register(r, protocol.TypeOrderAck, func(*protocol.Envelope, *fakePayload) {})

	keys := r.Keys()
	if len(keys) != 2 {
		t.Errorf("Keys() len = %d, want 2", len(keys))
	}
	want := map[string]bool{protocol.TypeOrderRequest: true, protocol.TypeOrderAck: true}
	for _, k := range keys {
		if !want[k] {
			t.Errorf("unexpected key in Keys(): %v", k)
		}
	}
}

// TestHas_ReturnsTrueForRegisteredFalseOtherwise pins the alternate
// coverage-assertion shape.
func TestHas_ReturnsTrueForRegisteredFalseOtherwise(t *testing.T) {
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, func(*protocol.Envelope, *fakePayload) {})

	if !r.Has(protocol.TypeOrderRequest) {
		t.Error("Has(registered) should be true")
	}
	if r.Has(protocol.TypeOrderAck) {
		t.Error("Has(unregistered) should be false")
	}
}

// TestInvokeChain_DoubleNextIsGuardedAndLogged pins the safety property
// on the middleware chain: a buggy middleware that calls next() more
// than once gets the first call honored and subsequent calls dropped
// with a warning log. Without the guard, the handler would fire once
// per next() call, doubling side effects.
func TestInvokeChain_DoubleNextIsGuardedAndLogged(t *testing.T) {
	buf := captureLog(t)
	r := router.New[string]()

	r.Use(func(_ *protocol.Envelope, _ any, next func()) {
		next()
		next() // intentional double-call
		next() // and a third
	})
	handlerCalls := 0
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {
		handlerCalls++
	})

	r.Dispatch(makeEnvelope(t, protocol.TypeOrderRequest, fakePayload{}), protocol.TypeOrderRequest)

	if handlerCalls != 1 {
		t.Errorf("handler called %d times, want 1 (double-next should be guarded)", handlerCalls)
	}
	if !strings.Contains(buf.String(), "called next() more than once") {
		t.Errorf("expected double-next warning log; got %q", buf.String())
	}
}

// TestLogRegistration_OutputsKeyCountAndMiddlewareCounts pins the
// composition-root contract: callers invoke LogRegistration at startup
// and the output covers handlers, global middleware, and per-key
// middleware bindings so missing registrations surface in boot logs.
func TestLogRegistration_OutputsKeyCountAndMiddlewareCounts(t *testing.T) {
	r := router.New[string]()
	router.Register(r, protocol.TypeOrderRequest, func(*protocol.Envelope, *fakePayload) {})
	router.Register(r, protocol.TypeOrderAck, func(*protocol.Envelope, *fakePayload) {})
	r.Use(func(_ *protocol.Envelope, _ any, next func()) { next() })
	r.UseFor(func(_ *protocol.Envelope, _ any, next func()) { next() }, protocol.TypeOrderRequest)

	var got string
	r.LogRegistration(func(format string, args ...any) {
		got = fmt.Sprintf(format, args...)
	})
	for _, want := range []string{"2 handler(s) registered", "1 global middleware", "1 per-key middleware"} {
		if !strings.Contains(got, want) {
			t.Errorf("expected log to mention %q; got %q", want, got)
		}
	}
}

// TestRegister_PayloadDecodeFailureSkipsHandler verifies that an
// undecodable payload doesn't invoke the handler. Real-world case: a
// regression that emits the wrong payload shape for an envelope type.
func TestRegister_PayloadDecodeFailureSkipsHandler(t *testing.T) {
	r := router.New[string]()

	handlerRan := false
	router.Register(r, protocol.TypeOrderRequest, func(_ *protocol.Envelope, _ *fakePayload) {
		handlerRan = true
	})

	// Envelope with non-JSON bytes as payload.
	env := &protocol.Envelope{
		Type:    protocol.TypeOrderRequest,
		Payload: []byte("not valid json"),
	}
	r.Dispatch(env, protocol.TypeOrderRequest)

	if handlerRan {
		t.Error("handler should not run when payload decode fails")
	}
}

func safeIndex(s []string, i int) string {
	if i >= len(s) {
		return "<out-of-range>"
	}
	return s[i]
}
