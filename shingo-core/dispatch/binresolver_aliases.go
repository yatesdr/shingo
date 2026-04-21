package dispatch

import "shingocore/dispatch/binresolver"

// Stage 5 of the architecture refactor moved the bin-resolution code
// (resolver.go, group_resolver.go, lane_lock.go, helpers.go) into the
// shingocore/dispatch/binresolver sub-package so the dispatch/ package
// no longer owns both the lifecycle state machine *and* the slot-picking
// algorithms.
//
// These aliases preserve the previous dispatch.* public API surface so
// existing callers in engine/ and the dispatch/ tests keep compiling
// without import-site churn. New code should import binresolver directly.

type (
	ResolveResult   = binresolver.ResolveResult
	NodeResolver    = binresolver.NodeResolver
	DefaultResolver = binresolver.DefaultResolver
	GroupResolver   = binresolver.GroupResolver
	LaneLock        = binresolver.LaneLock
	StructuralError = binresolver.StructuralError
	BuriedError     = binresolver.BuriedError
)

// ErrBuried mirrors binresolver.ErrBuried. Kept as a var alias (not a
// direct re-declare) so errors.Is still unwraps correctly.
var ErrBuried = binresolver.ErrBuried

// NewLaneLock constructs a *LaneLock — re-exported so callers using
// dispatch.NewLaneLock continue to compile.
var NewLaneLock = binresolver.NewLaneLock

// Retrieval and storage algorithm codes. Exported as untyped string
// constants so they behave identically to the originals at call sites.
const (
	RetrieveFIFO = binresolver.RetrieveFIFO
	RetrieveCOST = binresolver.RetrieveCOST
	RetrieveFAVL = binresolver.RetrieveFAVL
	StoreLKND    = binresolver.StoreLKND
	StoreDPTH    = binresolver.StoreDPTH
)

// IsAvailableAtConcreteNode mirrors binresolver.IsAvailableAtConcreteNode.
// Re-exported so dispatch/ code can call it without a direct sub-package import.
var IsAvailableAtConcreteNode = binresolver.IsAvailableAtConcreteNode
