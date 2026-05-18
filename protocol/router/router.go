// Package router is a generic, middleware-aware dispatch table for
// protocol envelopes. Phase 3 replaces the hand-written 17-case switch
// in protocol/ingestor.go (and the 28-case Subject switches in
// shingo-core/messaging/core_data_service.go and shingo-edge/messaging/
// edge_handler.go) with two Router[K] instances — one keyed on
// protocol.Type for envelope types, one keyed on protocol.Subject for
// Data subjects.
//
// The router package itself is decoupled from the protocol message types:
// it only cares that there's an Envelope with a Payload to dispatch on.
// Concrete payload types are bound via the generic Register[K, T].
package router

import (
	"encoding/json"
	"log"

	"shingo/protocol"
)

// Router is a typed message dispatch table. Keys are the discriminator
// (e.g., protocol.Type for envelopes, protocol.Subject for data subjects);
// handlers are registered with Register[K, T] and receive a fully-decoded
// typed payload at dispatch time.
//
// Middleware chains (Use / UseFor) wrap handlers with cross-cutting
// concerns like inbox dedup, payload validation, or per-message tracing.
// Middleware ordering is documented at Use's docstring.
type Router[K comparable] struct {
	routes   map[K]rawHandler
	globalMW []Middleware
	perKeyMW map[K][]Middleware
}

// rawHandler is the untyped form of a registered handler — invoked after
// the envelope has been decoded but before the typed payload unmarshal.
// Register[K, T] wraps a typed handler into a rawHandler that does the
// unmarshal at call time.
type rawHandler func(env *protocol.Envelope)

// Middleware wraps a handler invocation. The middleware function receives
// the envelope, the routing key (as `any` so the same middleware function
// can be shared across routers keyed on different K), and a next callback
// that invokes the rest of the chain.
//
// Contract:
//   - Calling next zero times short-circuits the chain (e.g., inbox dedup
//     detecting a duplicate).
//   - Calling next exactly once advances to the next middleware (or the
//     handler if this is the last middleware).
//   - Calling next more than once is safely guarded: only the first call
//     advances the chain; subsequent calls are dropped with a warning log
//     (per-key, naming the middleware index). Middleware authors do not
//     need to defend against accidental double-calls — the router does
//     it for them so a single handler invocation cannot be doubled.
type Middleware func(env *protocol.Envelope, key any, next func())

// New constructs an empty Router. Add routes with Register and middleware
// with Use / UseFor before the first Dispatch call.
func New[K comparable]() *Router[K] {
	return &Router[K]{
		routes:   make(map[K]rawHandler),
		perKeyMW: make(map[K][]Middleware),
	}
}

// Register binds a routing key to a handler that receives a typed payload
// of type T. The payload is unmarshaled from envelope.Payload at dispatch
// time; if unmarshal fails the handler is not invoked (the error is
// logged).
//
// Standalone generic function — Go disallows generic methods on generic
// receivers. Matches stdlib pattern (slices.SortFunc, maps.Keys).
//
// Re-registering an existing key replaces the previous handler.
func Register[K comparable, T any](r *Router[K], key K, fn func(*protocol.Envelope, *T)) {
	r.routes[key] = func(env *protocol.Envelope) {
		var p T
		if err := json.Unmarshal(env.Payload, &p); err != nil {
			log.Printf("router: payload decode error for key %v: %v", key, err)
			return
		}
		fn(env, &p)
	}
}

// Use appends middleware that runs for every dispatch. Order matters:
// middlewares run in registration order, each wrapping the next. The
// chain order at dispatch time is globalMW (in registration order),
// followed by perKeyMW for the dispatched key (in registration order),
// followed by the handler.
func (r *Router[K]) Use(mw Middleware) {
	r.globalMW = append(r.globalMW, mw)
}

// UseFor appends middleware that runs only when the dispatched key
// matches one of the listed keys. Use for cross-cutting concerns scoped
// to a subset of the routing table (e.g., the inbox-dedup middleware
// applies to order-channel envelopes only, not to general data subjects).
func (r *Router[K]) UseFor(mw Middleware, keys ...K) {
	for _, k := range keys {
		r.perKeyMW[k] = append(r.perKeyMW[k], mw)
	}
}

// Dispatch looks up the handler for key, builds the middleware chain
// (global + per-key), and invokes it. Returns and logs when no handler
// is registered for the key; the log line includes envelope ID, Type,
// and Src so ops can correlate a missed dispatch to a specific message.
// Production callers should pair Router construction with a startup
// coverage assertion (LogRegistration + a boot-time check that every
// expected key has a handler) so unhandled keys surface at boot rather
// than at first traffic.
func (r *Router[K]) Dispatch(env *protocol.Envelope, key K) {
	handler, ok := r.routes[key]
	if !ok {
		log.Printf("router: no handler registered for key %v (envelope id=%s type=%s src=%s/%s)",
			key, env.ID, env.Type, env.Src.Role, env.Src.Station)
		return
	}
	chain := append([]Middleware(nil), r.globalMW...)
	chain = append(chain, r.perKeyMW[key]...)
	invokeChain(env, key, handler, chain)
}

// Keys returns the set of registered routing keys in arbitrary order.
// Used by startup-coverage assertions and tests that walk the full
// table.
func (r *Router[K]) Keys() []K {
	keys := make([]K, 0, len(r.routes))
	for k := range r.routes {
		keys = append(keys, k)
	}
	return keys
}

// Has reports whether a handler is registered for the given key. Useful
// in startup assertions that walk a constants list.
func (r *Router[K]) Has(key K) bool {
	_, ok := r.routes[key]
	return ok
}

// LogRegistration writes a one-line summary of registered keys and
// middleware counts via the provided log function. Composition roots
// call this at startup, after all Register / Use / UseFor calls have
// completed, so any missing or unexpected registrations surface in
// boot logs before the first message arrives.
func (r *Router[K]) LogRegistration(logf func(string, ...any)) {
	keys := r.Keys()
	perKeyCount := 0
	for _, mws := range r.perKeyMW {
		perKeyCount += len(mws)
	}
	logf("router: %d handler(s) registered, %d global middleware, %d per-key middleware bindings; keys=%v",
		len(keys), len(r.globalMW), perKeyCount, keys)
}

// invokeChain runs the middleware chain inside-out: chain[0] wraps
// chain[1] wraps ... wraps the handler. Each middleware's next() callback
// advances the chain index by one; calling next 0 times short-circuits.
// next() is guarded against double-invocation: a middleware that calls
// next more than once gets the first call honored and subsequent calls
// logged + dropped. The dispatcher is single-goroutine so a plain bool
// suffices for the guard.
func invokeChain[K comparable](env *protocol.Envelope, key K, handler rawHandler, chain []Middleware) {
	if len(chain) == 0 {
		handler(env)
		return
	}
	var run func(i int)
	run = func(i int) {
		if i == len(chain) {
			handler(env)
			return
		}
		called := false
		chain[i](env, key, func() {
			if called {
				log.Printf("router: middleware at index %d called next() more than once (key=%v); subsequent calls ignored", i, key)
				return
			}
			called = true
			run(i + 1)
		})
	}
	run(0)
}
