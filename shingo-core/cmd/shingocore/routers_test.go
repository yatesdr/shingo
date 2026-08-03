package main

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
	"shingocore/messaging"
	"shingocore/service"
)

// These two tests ARE the boot assertions, moved to where they can fail
// cheaply. Before the extraction the same checks lived as log.Fatalf inside
// main() over routers built inside main(), so the only way to discover a
// subject or envelope type without a handler was to start Core — which, at a
// plant, means the line is already down.
//
// NIL DEPENDENCIES ARE DELIBERATE AND SUFFICIENT. Registration takes method
// VALUES; nothing is invoked, so the services never touch their nil *store.DB.
// That is the whole reason this is testable at all: the table's completeness is
// a property of the WIRING, not of anything the wiring points at. If a future
// builder starts calling into its dependencies at registration time, these
// tests will panic and say so — which is itself the right signal, because a
// composition root that does work while composing cannot be asserted early.

func testSubjectRouter(t *testing.T) *router.SubjectRouter {
	t.Helper()
	r, err := buildSubjectRouter(messaging.NewCoreDataService(nil, nil, service.EpochAnnounce{}))
	if err != nil {
		t.Fatalf("buildSubjectRouter: %v", err)
	}
	return r
}

// TestSubjectRouter_CoversEveryInboundSubject is the seam-3 tripwire in its new
// home: protocol.CoreInboundSubjects() naming a subject that buildSubjectRouter
// does not register is a Core that will not boot.
func TestSubjectRouter_CoversEveryInboundSubject(t *testing.T) {
	t.Parallel()
	r := testSubjectRouter(t)
	for _, s := range protocol.CoreInboundSubjects() {
		if !r.Has(s) {
			t.Errorf("no handler registered for inbound subject %q — "+
				"add a router.RegisterSubject call in buildSubjectRouter", s)
		}
	}
}

// TestSubjectRouter_DemandOriginIsWired names seam 3's subject on its own.
//
// The loop above would catch its absence too, but only for as long as
// SubjectDemandOrigin stays in CoreInboundSubjects() — and NN5's failure mode is
// precisely a three-part change landing in pieces. Deleting the entry from the
// LIST silences the loop while leaving Core unable to receive episode state; this
// assertion still fails. Verified red both ways: dropping the RegisterSubject
// call fails both tests, dropping only the CoreInboundSubjects() entry fails only
// this one.
func TestSubjectRouter_DemandOriginIsWired(t *testing.T) {
	t.Parallel()
	if !testSubjectRouter(t).Has(protocol.SubjectDemandOrigin) {
		t.Fatalf("subject %q has no handler — Core cannot receive demand episode state",
			protocol.SubjectDemandOrigin)
	}
	var found bool
	for _, s := range protocol.CoreInboundSubjects() {
		if s == protocol.SubjectDemandOrigin {
			found = true
		}
	}
	if !found {
		t.Errorf("subject %q is handled but absent from protocol.CoreInboundSubjects() — "+
			"the boot assertion and this test would both go quiet while Edge kept sending",
			protocol.SubjectDemandOrigin)
	}
}

// TestProtocolRouter_CoversEveryEnvelopeType is the second boot assertion, same
// treatment. protocol.AllTypes() includes the reply-channel types Core sends but
// never receives; those are registered as no-ops precisely so this check can stay
// exhaustive rather than carrying an exemption list that would rot.
func TestProtocolRouter_CoversEveryEnvelopeType(t *testing.T) {
	t.Parallel()
	r, err := buildProtocolRouter(
		messaging.NewCoreHandler(nil, nil, "test-station", "dispatch-topic", nil),
		testSubjectRouter(t),
		func(_ *protocol.Envelope, _ any, next func()) { next() },
		nil, // dataDbg is optional; the builder nil-guards it
	)
	if err != nil {
		t.Fatalf("buildProtocolRouter: %v", err)
	}
	for _, ty := range protocol.AllTypes() {
		if !r.Has(ty) {
			t.Errorf("no handler registered for envelope type %q — "+
				"add a router.Register call in buildProtocolRouter", ty)
		}
	}
}
