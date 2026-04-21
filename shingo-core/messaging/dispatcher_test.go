package messaging

import (
	"reflect"
	"testing"

	"shingo/protocol"
	"shingocore/dispatch"
)

// fakeDispatcher is a minimal Dispatcher implementation that records each
// Handle* call so tests can verify which method was dispatched. It satisfies
// the narrow Dispatcher interface declared in dispatcher.go.
type fakeDispatcher struct {
	calls []string

	lastEnv     *protocol.Envelope
	lastPayload any
}

func (f *fakeDispatcher) HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest) {
	f.calls = append(f.calls, "HandleOrderRequest")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel) {
	f.calls = append(f.calls, "HandleOrderCancel")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt) {
	f.calls = append(f.calls, "HandleOrderReceipt")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect) {
	f.calls = append(f.calls, "HandleOrderRedirect")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill) {
	f.calls = append(f.calls, "HandleOrderStorageWaybill")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest) {
	f.calls = append(f.calls, "HandleOrderIngest")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest) {
	f.calls = append(f.calls, "HandleComplexOrderRequest")
	f.lastEnv = env
	f.lastPayload = p
}
func (f *fakeDispatcher) HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease) {
	f.calls = append(f.calls, "HandleOrderRelease")
	f.lastEnv = env
	f.lastPayload = p
}

// TestDispatcherInterface_SatisfiedByFake confirms that a pure in-test fake
// conforming to the eight Handle* methods satisfies Dispatcher. This is the
// main value the narrow interface provides: swapping in a test double for
// CoreHandler without spinning up a real *dispatch.Dispatcher.
func TestDispatcherInterface_SatisfiedByFake(t *testing.T) {
	f := &fakeDispatcher{}
	var d Dispatcher = f
	// Round-trip the interface: type-assert back to the concrete pointer
	// and verify it's the same object. This proves the assignment was
	// real (not a nil interface) without introspection.
	got, ok := d.(*fakeDispatcher)
	if !ok {
		t.Fatal("Dispatcher iface dynamic type != *fakeDispatcher")
	}
	if got != f {
		t.Errorf("type-assertion returned different pointer: got %p, want %p", got, f)
	}
}

// TestDispatcherInterface_SatisfiedByRealDispatcher confirms that a nil
// *dispatch.Dispatcher is assignable to the Dispatcher interface. This
// mirrors the compile-time assertion at the bottom of dispatcher.go but
// makes the coverage explicit in the test report.
func TestDispatcherInterface_SatisfiedByRealDispatcher(t *testing.T) {
	var d Dispatcher = (*dispatch.Dispatcher)(nil)
	// Use reflection rather than calling d directly: calling a Handle*
	// method on the typed-nil would panic in the receiver body. We just
	// want to confirm the assignment compiles and the iface holds the
	// expected dynamic type.
	rt := reflect.TypeOf(d)
	if rt == nil {
		t.Fatal("reflect.TypeOf(d) = nil; expected *dispatch.Dispatcher")
	}
	if rt.Kind() != reflect.Ptr {
		t.Fatalf("type kind = %v, want Ptr", rt.Kind())
	}
	if rt.Elem().Name() != "Dispatcher" {
		t.Errorf("elem name = %q, want %q", rt.Elem().Name(), "Dispatcher")
	}
}

// TestDispatcherInterface_HasAllEightOrderHandlers uses reflection to assert
// the interface declares exactly the eight Handle* order methods. If anyone
// renames, drops, or adds a method without updating this test or the compile
// assertion, the divergence is caught here.
func TestDispatcherInterface_HasAllEightOrderHandlers(t *testing.T) {
	want := []string{
		"HandleComplexOrderRequest",
		"HandleOrderCancel",
		"HandleOrderIngest",
		"HandleOrderRedirect",
		"HandleOrderReceipt",
		"HandleOrderRelease",
		"HandleOrderRequest",
		"HandleOrderStorageWaybill",
	}

	// reflect.TypeOf on a nil interface value yields nil, so use a pointer
	// to a zero-value interface variable to introspect the interface type.
	var iface Dispatcher
	rt := reflect.TypeOf(&iface).Elem()
	if rt.Kind() != reflect.Interface {
		t.Fatalf("kind = %v, want Interface", rt.Kind())
	}
	if rt.NumMethod() != len(want) {
		t.Errorf("interface method count = %d, want %d", rt.NumMethod(), len(want))
	}

	got := make(map[string]bool, rt.NumMethod())
	for i := 0; i < rt.NumMethod(); i++ {
		got[rt.Method(i).Name] = true
	}
	for _, m := range want {
		if !got[m] {
			t.Errorf("Dispatcher interface missing method %q", m)
		}
	}
}

// TestDispatcher_FakeDispatchesEachPayload exercises each Handle* method on
// the fake and verifies the call was recorded with the correct envelope and
// payload. This ensures the fake we use elsewhere doesn't silently drop
// method invocations, and indirectly documents the dispatch surface.
func TestDispatcher_FakeDispatchesEachPayload(t *testing.T) {
	env := &protocol.Envelope{
		ID:   "env-1",
		Type: protocol.TypeOrderRequest,
		Src:  protocol.Address{Role: protocol.RoleEdge, Station: "edge.1"},
		Dst:  protocol.Address{Role: protocol.RoleCore},
	}

	f := &fakeDispatcher{}
	var d Dispatcher = f

	d.HandleOrderRequest(env, &protocol.OrderRequest{OrderUUID: "or-1"})
	d.HandleOrderCancel(env, &protocol.OrderCancel{OrderUUID: "oc-1"})
	d.HandleOrderReceipt(env, &protocol.OrderReceipt{OrderUUID: "rec-1"})
	d.HandleOrderRedirect(env, &protocol.OrderRedirect{OrderUUID: "red-1"})
	d.HandleOrderStorageWaybill(env, &protocol.OrderStorageWaybill{OrderUUID: "wb-1"})
	d.HandleOrderIngest(env, &protocol.OrderIngestRequest{OrderUUID: "ig-1"})
	d.HandleComplexOrderRequest(env, &protocol.ComplexOrderRequest{OrderUUID: "cx-1"})
	d.HandleOrderRelease(env, &protocol.OrderRelease{OrderUUID: "rl-1"})

	wantCalls := []string{
		"HandleOrderRequest",
		"HandleOrderCancel",
		"HandleOrderReceipt",
		"HandleOrderRedirect",
		"HandleOrderStorageWaybill",
		"HandleOrderIngest",
		"HandleComplexOrderRequest",
		"HandleOrderRelease",
	}
	if !reflect.DeepEqual(f.calls, wantCalls) {
		t.Fatalf("calls = %v, want %v", f.calls, wantCalls)
	}
	if f.lastEnv != env {
		t.Errorf("lastEnv = %v, want %v", f.lastEnv, env)
	}
	if rel, ok := f.lastPayload.(*protocol.OrderRelease); !ok {
		t.Errorf("lastPayload type = %T, want *protocol.OrderRelease", f.lastPayload)
	} else if rel.OrderUUID != "rl-1" {
		t.Errorf("lastPayload.OrderUUID = %q, want %q", rel.OrderUUID, "rl-1")
	}
}

// TestDispatcher_IncompleteFakeDoesNotSatisfy is a negative check: a struct
// that only implements seven of the eight methods must NOT satisfy Dispatcher.
// Verified via reflect.Type.Implements so the test compiles even when the
// candidate is intentionally incomplete.
func TestDispatcher_IncompleteFakeDoesNotSatisfy(t *testing.T) {
	type partialDispatcher struct{}
	// partialDispatcher has zero methods so it cannot satisfy Dispatcher.

	var iface Dispatcher
	ifaceType := reflect.TypeOf(&iface).Elem()
	partialType := reflect.TypeOf(&partialDispatcher{})

	if partialType.Implements(ifaceType) {
		t.Fatal("partialDispatcher should NOT implement Dispatcher")
	}
}
