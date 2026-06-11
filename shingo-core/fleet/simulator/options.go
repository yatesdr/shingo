package simulator

import "shingo/shared/clock"

// Options controls simulated backend behavior.
type Options struct {
	failOnCreate bool        // inject fleet creation failures
	failOnPing   bool        // inject ping failures
	clk          clock.Clock // clock for terminal-order timestamps + eviction
}

// Option configures a SimulatorBackend.
type Option func(*Options)

// WithClock injects the clock used to stamp terminal-order timestamps (for
// eviction) and, via the driver, to time transitions. Defaults to clock.Real();
// tests pass a manual clock for deterministic eviction.
func WithClock(c clock.Clock) Option {
	return func(o *Options) { o.clk = c }
}

// WithCreateFailure causes all CreateTransportOrder and CreateStagedOrder
// calls to return an error. Use this to test fleet-outage scenarios.
func WithCreateFailure() Option {
	return func(o *Options) { o.failOnCreate = true }
}

// WithPingFailure causes Ping to return an error.
func WithPingFailure() Option {
	return func(o *Options) { o.failOnPing = true }
}
