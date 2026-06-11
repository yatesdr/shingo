//go:build sim

package www

// simModeEnabled reports whether this is a sim build. It gates the dev-only
// speed top-strip in the layout — true only under -tags sim.
func simModeEnabled() bool { return true }
