package router

import (
	"encoding/json"
	"log"

	"shingo/protocol"
)

// SubjectRouter is the second-tier dispatch table for TypeData envelopes.
// Where Router[K] decodes from env.Payload, SubjectRouter decodes from
// data.Body — the inner payload nested inside the already-decoded Data
// envelope. Wired by CoreDataService and EdgeHandler to replace the
// 12+14 case Subject switches that motivated the router cutover.
//
// The two routers are intentionally separate types rather than one
// generic Router[K] with a configurable body source: the entry-point
// signatures differ (Dispatch(env, key) vs Dispatch(env, data)) and a
// single shared type would force an awkward decoder-function field on
// every router. Subject dispatch is a distinct enough pattern that
// duplicating ~60 lines reads more clearly than generalizing.
type SubjectRouter struct {
	routes   map[string]subjectHandler
	globalMW []Middleware
	perKeyMW map[string][]Middleware
}

// subjectHandler is the untyped form of a registered subject handler —
// invoked with the envelope and the already-decoded Data, leaving the
// per-subject T unmarshal to the closure built in RegisterSubject.
type subjectHandler func(env *protocol.Envelope, data *protocol.Data)

// NewSubject constructs an empty SubjectRouter.
func NewSubject() *SubjectRouter {
	return &SubjectRouter{
		routes:   make(map[string]subjectHandler),
		perKeyMW: make(map[string][]Middleware),
	}
}

// RegisterSubject binds a Data subject to a handler that receives a
// typed payload of type T. The payload is unmarshaled from data.Body at
// dispatch time; if unmarshal fails the handler is not invoked (the
// error is logged with the subject for correlation).
//
// Standalone generic function — mirrors Register[K, T] for the same
// reason (Go disallows generic methods on generic receivers, and we
// keep the API shape parallel even though SubjectRouter itself is not
// generic).
//
// Re-registering an existing subject replaces the previous handler.
func RegisterSubject[T any](r *SubjectRouter, subject string, fn func(*protocol.Envelope, *T)) {
	r.routes[subject] = func(env *protocol.Envelope, data *protocol.Data) {
		var p T
		if err := json.Unmarshal(data.Body, &p); err != nil {
			log.Printf("router: subject body decode error for %s: %v", subject, err)
			return
		}
		fn(env, &p)
	}
}

// RegisterSubjectBare binds a subject to a handler that receives only
// the envelope — for subjects whose payload body is empty or whose
// handler doesn't need the decoded body. Replaces the pre-router cases
// that did the type-specific unmarshal only to discard the result
// (e.g., SubjectNodeListRequest, SubjectCatalogPayloadsRequest).
func RegisterSubjectBare(r *SubjectRouter, subject string, fn func(*protocol.Envelope)) {
	r.routes[subject] = func(env *protocol.Envelope, _ *protocol.Data) {
		fn(env)
	}
}

// Use appends middleware that runs for every subject dispatch. Order
// matches Router.Use semantics.
func (r *SubjectRouter) Use(mw Middleware) {
	r.globalMW = append(r.globalMW, mw)
}

// UseFor appends middleware that runs only when the dispatched subject
// matches one of the listed subjects.
func (r *SubjectRouter) UseFor(mw Middleware, subjects ...string) {
	for _, s := range subjects {
		r.perKeyMW[s] = append(r.perKeyMW[s], mw)
	}
}

// Dispatch looks up the handler for data.Subject and invokes it through
// the middleware chain. Logs and returns when no handler is registered;
// the log line includes envelope ID, Subject, and Src for correlation.
func (r *SubjectRouter) Dispatch(env *protocol.Envelope, data *protocol.Data) {
	handler, ok := r.routes[data.Subject]
	if !ok {
		log.Printf("router: no handler registered for subject %s (envelope id=%s src=%s/%s)",
			data.Subject, env.ID, env.Src.Role, env.Src.Station)
		return
	}
	chain := append([]Middleware(nil), r.globalMW...)
	chain = append(chain, r.perKeyMW[data.Subject]...)
	invokeSubjectChain(env, data, handler, chain)
}

// Subjects returns the set of registered subjects in arbitrary order.
// Used by startup assertions.
func (r *SubjectRouter) Subjects() []string {
	keys := make([]string, 0, len(r.routes))
	for k := range r.routes {
		keys = append(keys, k)
	}
	return keys
}

// Has reports whether a handler is registered for the given subject.
func (r *SubjectRouter) Has(subject string) bool {
	_, ok := r.routes[subject]
	return ok
}

// invokeSubjectChain mirrors invokeChain but threads the *Data through
// to the leaf handler. Middleware sees the envelope and subject (as
// `any` for compatibility with Middleware shared across routers); it
// does not see the decoded Data — subject middleware should make its
// decisions on envelope identity (ID, Src) or subject key alone.
func invokeSubjectChain(env *protocol.Envelope, data *protocol.Data, handler subjectHandler, chain []Middleware) {
	if len(chain) == 0 {
		handler(env, data)
		return
	}
	var run func(i int)
	run = func(i int) {
		if i == len(chain) {
			handler(env, data)
			return
		}
		called := false
		chain[i](env, data.Subject, func() {
			if called {
				log.Printf("router: subject middleware at index %d called next() more than once (subject=%s); subsequent calls ignored", i, data.Subject)
				return
			}
			called = true
			run(i + 1)
		})
	}
	run(0)
}
