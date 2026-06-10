//go:build !sim

package www

// simModeEnabled is false in production builds, so the dev speed top-strip is
// never injected.
func simModeEnabled() bool { return false }
