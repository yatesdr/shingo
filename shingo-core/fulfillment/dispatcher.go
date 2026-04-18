package fulfillment

import (
	"shingocore/dispatch"
	"shingocore/dispatch/binresolver"
	"shingocore/store"
)

// Dispatcher is the narrow dispatch surface the scanner depends on.
//
// Declared consumer-side so *dispatch.Dispatcher satisfies it for
// free (structural). The scanner only exercises DispatchDirect on
// the happy path; narrowing the interface lets scanner_test.go
// stub dispatch with a one-method fake, which closes the coverage
// gap the old lines-14–31 scope note called out.
type Dispatcher interface {
	DispatchDirect(order *store.Order, sourceNode, destNode *store.Node) (string, error)
}

// Resolver is the narrow resolver surface the scanner depends on.
//
// Signature mirrors binresolver.NodeResolver (exported via the
// dispatch.NodeResolver alias). Declared here rather than reusing
// the alias so scanner_test.go does not have to pull in dispatch
// to stub a fake resolver.
type Resolver interface {
	Resolve(syntheticNode *store.Node, orderType string, payloadCode string, binTypeID *int64) (*binresolver.ResolveResult, error)
}

// Compile-time checks that the concrete dispatch types satisfy the
// consumer-side interfaces. If dispatch drops or renames either
// method, the assertion catches it before a build failure elsewhere.
var (
	_ Dispatcher = (*dispatch.Dispatcher)(nil)
	_ Resolver   = (*dispatch.DefaultResolver)(nil)
)
