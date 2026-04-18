package messaging

import (
	"shingo/protocol"

	"shingocore/dispatch"
)

// Dispatcher is the narrow dispatch surface CoreHandler depends on.
//
// Declared consumer-side so *dispatch.Dispatcher satisfies it for
// free (structural). The eight methods mirror the eight order
// message types CoreHandler receives on the orders topic — any new
// order handler on dispatch.Dispatcher requires a matching addition
// here, which the compile-time assertion below catches immediately.
//
// Holding the narrow interface in CoreHandler's dispatcher field is
// what lets core_handler_test.go substitute a fake dispatcher without
// spinning up the full *dispatch.Dispatcher (which owns DB, Fleet, and
// the tracker graph).
type Dispatcher interface {
	HandleOrderRequest(env *protocol.Envelope, p *protocol.OrderRequest)
	HandleOrderCancel(env *protocol.Envelope, p *protocol.OrderCancel)
	HandleOrderReceipt(env *protocol.Envelope, p *protocol.OrderReceipt)
	HandleOrderRedirect(env *protocol.Envelope, p *protocol.OrderRedirect)
	HandleOrderStorageWaybill(env *protocol.Envelope, p *protocol.OrderStorageWaybill)
	HandleOrderIngest(env *protocol.Envelope, p *protocol.OrderIngestRequest)
	HandleComplexOrderRequest(env *protocol.Envelope, p *protocol.ComplexOrderRequest)
	HandleOrderRelease(env *protocol.Envelope, p *protocol.OrderRelease)
}

// Compile-time check that *dispatch.Dispatcher satisfies the narrow
// Dispatcher interface. If dispatch renames or drops any of the eight
// order handlers, this assertion fails before the build errors out
// in core_handler.go — which would otherwise surface as a confusing
// interface-conversion error at engine wiring time.
var _ Dispatcher = (*dispatch.Dispatcher)(nil)
