// routers.go — the two inbound dispatch tables, and the completeness
// assertions over them.
//
// WHY THESE LEFT main(): the assertions were correct and fired at the worst
// possible moment. Both were `log.Fatalf` inside main(), over routers assembled
// inside main() and reachable from nowhere else, so a subject or envelope type
// declared without a handler produced "Core does not start" — discovered at a
// deploy, on a plant, with the line down, by whoever was standing there. Nothing
// in the test suite could see it, because there was no seam to call.
//
// Extracted, the same checks run in a unit test in milliseconds and the failure
// becomes a red build. THE PRINCIPLE: AN ASSERTION SHOULD FIRE AS EARLY AS IT
// POSSIBLY CAN. A boot assertion that could have been a test assertion is the
// same check at a much worse moment.
//
// Both builders take their dependencies as parameters and return an error
// instead of exiting, which is what makes them callable from a test at all —
// they register method VALUES, so a zero-valued service is enough to build the
// table. main() turns the error back into log.Fatalf: at boot an incomplete
// composition root is still fatal, and it is now merely the second line of
// defence rather than the only one.
//
// Ledger item B10. The generalisation B10 asked for was run: main() holds nine
// log.Fatalf calls and exactly TWO are composition-root assertions — these. The
// other seven (debug log, config load, DB reset/open, sim fleet backend, web
// server bind, www.NewRouter) are environment failures that depend on a config
// file, a database or a port, and cannot be answered by a test. They stay.

package main

import (
	"fmt"

	"shingo/protocol"
	"shingo/protocol/router"
	"shingocore/messaging"
)

// buildSubjectRouter registers every Data subject Core handles against a
// CoreDataService method and then asserts the table covers
// protocol.CoreInboundSubjects().
//
// Adding a subject is a THREE-PART change — the const, the
// CoreInboundSubjects() entry, and the RegisterSubject call here — and the
// assertion is what makes the third part non-optional. Seam 3's demand.origin
// is the live example: an entry in the list without a handler here used to mean
// a Core that would not boot.
func buildSubjectRouter(svc *messaging.CoreDataService) (*router.SubjectRouter, error) {
	r := router.NewSubject()
	router.RegisterSubject(r, protocol.SubjectEdgeRegister, svc.HandleEdgeRegister)
	router.RegisterSubject(r, protocol.SubjectEdgeHeartbeat, svc.HandleEdgeHeartbeat)
	router.RegisterSubjectBare(r, protocol.SubjectNodeListRequest, svc.HandleNodeListRequest)
	router.RegisterSubject(r, protocol.SubjectProductionReport, svc.HandleProductionReport)
	router.RegisterSubject(r, protocol.SubjectTagVerifyRequest, svc.HandleTagVerifyRequest)
	router.RegisterSubjectBare(r, protocol.SubjectCatalogPayloadsRequest, svc.HandleCatalogPayloadsRequest)
	router.RegisterSubject(r, protocol.SubjectNodeStateRequest, svc.HandleNodeStateRequest)
	router.RegisterSubject(r, protocol.SubjectOrderStatusRequest, svc.HandleOrderStatusRequest)
	router.RegisterSubject(r, protocol.SubjectClaimSync, svc.HandleClaimSync)
	router.RegisterSubject(r, protocol.SubjectCountGroupAck, svc.HandleCountGroupAck)
	router.RegisterSubject(r, protocol.SubjectBinUOPDelta, svc.HandleBinUOPDelta)
	router.RegisterSubject(r, protocol.SubjectLinesideBucketDelta, svc.HandleLinesideBucketDelta)
	router.RegisterSubject(r, protocol.SubjectProductionTick, svc.HandleProductionTick)
	router.RegisterSubject(r, protocol.SubjectDowntimeEvent, svc.HandleDowntimeEvent)
	router.RegisterSubject(r, protocol.SubjectPlantClaims, svc.HandlePlantClaims)
	router.RegisterSubject(r, protocol.SubjectLinesideLevelReport, svc.HandleLinesideLevelReport)
	router.RegisterSubject(r, protocol.SubjectDemandOrigin, svc.HandleDemandOrigin)

	for _, s := range protocol.CoreInboundSubjects() {
		if !r.Has(s) {
			return nil, fmt.Errorf("subject router missing handler for %s — "+
				"add a router.RegisterSubject call for it in buildSubjectRouter", s)
		}
	}
	return r, nil
}

// buildProtocolRouter registers every envelope Type against a CoreHandler method
// (or, for TypeData, a closure into the subject router) and asserts the table
// covers protocol.AllTypes().
//
// The 8 order-channel Types share the inbox-dedup middleware via UseFor;
// TypeData and the reply-channel Types pass through ungated, which matches the
// order-channel scoping of the legacy InboxDedup decorator this replaced.
func buildProtocolRouter(
	h *messaging.CoreHandler,
	subjectRouter *router.SubjectRouter,
	dedupMW router.Middleware,
	dataDbg func(string, ...any),
) (*router.Router[string], error) {
	r := router.New[string]()
	r.UseFor(dedupMW,
		protocol.TypeOrderRequest,
		protocol.TypeOrderCancel,
		protocol.TypeOrderReceipt,
		protocol.TypeOrderRedirect,
		protocol.TypeComplexOrderRequest,
		protocol.TypeOrderRelease,
		protocol.TypeOrderIngest,
	)
	router.Register(r, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) {
		if dataDbg != nil {
			dataDbg("data: subject=%s body_size=%d from=%s", p.Subject, len(p.Body), env.Src.Station)
		}
		subjectRouter.Dispatch(env, p)
	})
	router.Register(r, protocol.TypeOrderRequest, h.HandleOrderRequest)
	router.Register(r, protocol.TypeOrderCancel, h.HandleOrderCancel)
	router.Register(r, protocol.TypeOrderReceipt, h.HandleOrderReceipt)
	router.Register(r, protocol.TypeOrderRedirect, h.HandleOrderRedirect)
	router.Register(r, protocol.TypeComplexOrderRequest, h.HandleComplexOrderRequest)
	router.Register(r, protocol.TypeOrderRelease, h.HandleOrderRelease)
	router.Register(r, protocol.TypeOrderIngest, h.HandleOrderIngest)
	// Core sends these reply-channel types to Edge but never receives them.
	// Registered as inline no-ops so the completeness assertion is satisfied
	// without inventing a junk MessageHandler implementation. The router's own
	// "no handler registered" log makes accidental inbound reply-channel traffic
	// visible if it ever shows up.
	router.Register(r, protocol.TypeOrderAck, func(*protocol.Envelope, *protocol.OrderAck) {})
	router.Register(r, protocol.TypeOrderWaybill, func(*protocol.Envelope, *protocol.OrderWaybill) {})
	router.Register(r, protocol.TypeOrderUpdate, func(*protocol.Envelope, *protocol.OrderUpdate) {})
	router.Register(r, protocol.TypeOrderDelivered, func(*protocol.Envelope, *protocol.OrderDelivered) {})
	router.Register(r, protocol.TypeOrderError, func(*protocol.Envelope, *protocol.OrderError) {})
	router.Register(r, protocol.TypeOrderCancelled, func(*protocol.Envelope, *protocol.OrderCancelled) {})
	router.Register(r, protocol.TypeOrderStaged, func(*protocol.Envelope, *protocol.OrderStaged) {})
	router.Register(r, protocol.TypeOrderSkipped, func(*protocol.Envelope, *protocol.OrderSkipped) {})

	for _, t := range protocol.AllTypes() {
		if !r.Has(t) {
			return nil, fmt.Errorf("protocol router missing handler for envelope type %s — "+
				"add a router.Register call for it in buildProtocolRouter", t)
		}
	}
	return r, nil
}
