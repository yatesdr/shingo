package protocol_test

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/router"
)

// TestIngestorRouterAgreement_AllTypesReachSameHandler asserts that for
// every envelope Type, an Ingestor whose Dispatch hook is wired to a
// router with handlers registered for all types dispatches each type to
// the correct handler method. The router is the only dispatcher;
// pre-cutover this test also pinned parity against a legacy ingestor
// switch (now removed).
//
// If a type is missing from the router registration, its counter stays
// at 0 — test fails loudly with the missing type named. If the router
// dispatches to the wrong handler, the wrong counter increments — test
// fails naming the misroute.
func TestIngestorRouterAgreement_AllTypesReachSameHandler(t *testing.T) {
	for _, tc := range agreementCases() {
		t.Run(tc.typ, func(t *testing.T) {
			h := &countingHandler{}

			r := router.New[string]()
			registerAllTypesOnRouter(r, h)

			ing := protocol.NewIngestor(nil)
			ing.Dispatch = func(env *protocol.Envelope) {
				r.Dispatch(env, env.Type)
			}

			env, err := protocol.NewEnvelope(tc.typ,
				protocol.Address{Role: protocol.RoleEdge, Station: "test-station"},
				protocol.Address{Role: protocol.RoleCore},
				tc.payload)
			if err != nil {
				t.Fatalf("build envelope for %s: %v", tc.typ, err)
			}
			data, err := env.Encode()
			if err != nil {
				t.Fatalf("encode envelope for %s: %v", tc.typ, err)
			}

			ing.HandleRaw(data)

			if got := h.calls[tc.method]; got != 1 {
				t.Errorf("router path: %s called %d times, want 1 (other methods: %v)",
					tc.method, got, h.calls)
			}
		})
	}
}

// TestAllTypes_MatchesAgreementCases ensures the AllTypes() list stays
// in sync with the agreement test table. Adding a new envelope type
// without adding it to both lists fails this test loudly.
func TestAllTypes_MatchesAgreementCases(t *testing.T) {
	cases := agreementCases()
	caseTypes := make(map[string]bool, len(cases))
	for _, c := range cases {
		caseTypes[c.typ] = true
	}
	for _, typ := range protocol.AllTypes() {
		if !caseTypes[typ] {
			t.Errorf("protocol.AllTypes() lists %s but agreementCases() does not — add it to the table", typ)
		}
	}
	for _, c := range cases {
		found := false
		for _, typ := range protocol.AllTypes() {
			if typ == c.typ {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("agreementCases() lists %s but protocol.AllTypes() does not — add it to the constants slice", c.typ)
		}
	}
}

// agreementCase pairs an envelope Type with a sample payload and the
// MessageHandler method that should be invoked when that type
// dispatches via the router. One entry per type in protocol.AllTypes().
type agreementCase struct {
	typ     string
	payload any
	method  string
}

func agreementCases() []agreementCase {
	return []agreementCase{
		{protocol.TypeData, &protocol.Data{Subject: "any.subject"}, "HandleData"},
		{protocol.TypeOrderRequest, &protocol.OrderRequest{}, "HandleOrderRequest"},
		{protocol.TypeOrderCancel, &protocol.OrderCancel{}, "HandleOrderCancel"},
		{protocol.TypeOrderReceipt, &protocol.OrderReceipt{}, "HandleOrderReceipt"},
		{protocol.TypeOrderRedirect, &protocol.OrderRedirect{}, "HandleOrderRedirect"},
		{protocol.TypeOrderStorageWaybill, &protocol.OrderStorageWaybill{}, "HandleOrderStorageWaybill"},
		{protocol.TypeComplexOrderRequest, &protocol.ComplexOrderRequest{}, "HandleComplexOrderRequest"},
		{protocol.TypeOrderRelease, &protocol.OrderRelease{}, "HandleOrderRelease"},
		{protocol.TypeOrderIngest, &protocol.OrderIngestRequest{}, "HandleOrderIngest"},
		{protocol.TypeOrderAck, &protocol.OrderAck{}, "HandleOrderAck"},
		{protocol.TypeOrderWaybill, &protocol.OrderWaybill{}, "HandleOrderWaybill"},
		{protocol.TypeOrderUpdate, &protocol.OrderUpdate{}, "HandleOrderUpdate"},
		{protocol.TypeOrderDelivered, &protocol.OrderDelivered{}, "HandleOrderDelivered"},
		{protocol.TypeOrderError, &protocol.OrderError{}, "HandleOrderError"},
		{protocol.TypeOrderCancelled, &protocol.OrderCancelled{}, "HandleOrderCancelled"},
		{protocol.TypeOrderStaged, &protocol.OrderStaged{}, "HandleOrderStaged"},
		{protocol.TypeOrderSkipped, &protocol.OrderSkipped{}, "HandleOrderSkipped"},
	}
}

// registerAllTypesOnRouter wires every envelope Type to the matching
// method on the given handler. Used by the agreement test. Production
// composition roots (cmd/shingocore/main.go, cmd/shingoedge/main.go)
// do their own per-type Register calls so the dispatch table is the
// explicit, grep-able source of truth rather than a one-line helper.
func registerAllTypesOnRouter(r *router.Router[string], h *countingHandler) {
	router.Register(r, protocol.TypeData, h.HandleData)
	router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	router.Register(r, protocol.TypeOrderCancel, h.HandleOrderCancel)
	router.Register(r, protocol.TypeOrderReceipt, h.HandleOrderReceipt)
	router.Register(r, protocol.TypeOrderRedirect, h.HandleOrderRedirect)
	router.Register(r, protocol.TypeOrderStorageWaybill, h.HandleOrderStorageWaybill)
	router.Register(r, protocol.TypeComplexOrderRequest, h.HandleComplexOrderRequest)
	router.Register(r, protocol.TypeOrderRelease, h.HandleOrderRelease)
	router.Register(r, protocol.TypeOrderIngest, h.HandleOrderIngest)
	router.Register(r, protocol.TypeOrderAck, h.HandleOrderAck)
	router.Register(r, protocol.TypeOrderWaybill, h.HandleOrderWaybill)
	router.Register(r, protocol.TypeOrderUpdate, h.HandleOrderUpdate)
	router.Register(r, protocol.TypeOrderDelivered, h.HandleOrderDelivered)
	router.Register(r, protocol.TypeOrderError, h.HandleOrderError)
	router.Register(r, protocol.TypeOrderCancelled, h.HandleOrderCancelled)
	router.Register(r, protocol.TypeOrderStaged, h.HandleOrderStaged)
	router.Register(r, protocol.TypeOrderSkipped, h.HandleOrderSkipped)
}

// countingHandler holds the 17 envelope-Type handler methods as plain
// instance methods, each incrementing a per-method counter. Used by the
// agreement test to assert that router dispatch reaches the correct
// method for every envelope Type.
type countingHandler struct {
	calls map[string]int
}

func (h *countingHandler) incr(method string) {
	if h.calls == nil {
		h.calls = make(map[string]int)
	}
	h.calls[method]++
}

func (h *countingHandler) HandleData(*protocol.Envelope, *protocol.Data) {
	h.incr("HandleData")
}
func (h *countingHandler) HandleOrderRequest(*protocol.Envelope, *protocol.OrderRequest) {
	h.incr("HandleOrderRequest")
}
func (h *countingHandler) HandleOrderCancel(*protocol.Envelope, *protocol.OrderCancel) {
	h.incr("HandleOrderCancel")
}
func (h *countingHandler) HandleOrderReceipt(*protocol.Envelope, *protocol.OrderReceipt) {
	h.incr("HandleOrderReceipt")
}
func (h *countingHandler) HandleOrderRedirect(*protocol.Envelope, *protocol.OrderRedirect) {
	h.incr("HandleOrderRedirect")
}
func (h *countingHandler) HandleOrderStorageWaybill(*protocol.Envelope, *protocol.OrderStorageWaybill) {
	h.incr("HandleOrderStorageWaybill")
}
func (h *countingHandler) HandleComplexOrderRequest(*protocol.Envelope, *protocol.ComplexOrderRequest) {
	h.incr("HandleComplexOrderRequest")
}
func (h *countingHandler) HandleOrderRelease(*protocol.Envelope, *protocol.OrderRelease) {
	h.incr("HandleOrderRelease")
}
func (h *countingHandler) HandleOrderIngest(*protocol.Envelope, *protocol.OrderIngestRequest) {
	h.incr("HandleOrderIngest")
}
func (h *countingHandler) HandleOrderAck(*protocol.Envelope, *protocol.OrderAck) {
	h.incr("HandleOrderAck")
}
func (h *countingHandler) HandleOrderWaybill(*protocol.Envelope, *protocol.OrderWaybill) {
	h.incr("HandleOrderWaybill")
}
func (h *countingHandler) HandleOrderUpdate(*protocol.Envelope, *protocol.OrderUpdate) {
	h.incr("HandleOrderUpdate")
}
func (h *countingHandler) HandleOrderDelivered(*protocol.Envelope, *protocol.OrderDelivered) {
	h.incr("HandleOrderDelivered")
}
func (h *countingHandler) HandleOrderError(*protocol.Envelope, *protocol.OrderError) {
	h.incr("HandleOrderError")
}
func (h *countingHandler) HandleOrderCancelled(*protocol.Envelope, *protocol.OrderCancelled) {
	h.incr("HandleOrderCancelled")
}
func (h *countingHandler) HandleOrderStaged(*protocol.Envelope, *protocol.OrderStaged) {
	h.incr("HandleOrderStaged")
}
func (h *countingHandler) HandleOrderSkipped(*protocol.Envelope, *protocol.OrderSkipped) {
	h.incr("HandleOrderSkipped")
}

