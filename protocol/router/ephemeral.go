package router

import "shingo/protocol"

// ReadEphemeral is the delivery for a reader of the ephemeral topic
// (protocol/ephemeral.go): a message accept takes, of a type r has a handler
// for, is dispatched through r by its type. Every other message is dropped
// without a log line by protocol.DecodeEphemeral: a bad signature, one past
// its exp, one addressed elsewhere, and a type this side does not read (Core
// reads its own robot.positions back off the topic, every Edge every other
// Edge's watch). The ingestor is not involved, so nothing here is counted or
// logged as an expired drop. r's own decode still logs a body that does not
// decode, which on this topic is a peer's bug, not a message to drop quietly.
//
// r is a router of its own, not the durable topics' protocol router: register
// on it only the ephemeral types this side reads (protocol.EphemeralTypes).
func ReadEphemeral(signingKey []byte, accept protocol.FilterFunc, r *Router[string]) func(data []byte) {
	filter := func(h *protocol.RawHeader) bool {
		return r.Has(h.Type) && (accept == nil || accept(h))
	}
	return func(data []byte) {
		if env, ok := protocol.DecodeEphemeral(data, signingKey, filter); ok {
			r.Dispatch(env, env.Type)
		}
	}
}
