package simulator

// Options controls simulated backend behavior.
type Options struct {
	failOnCreate bool // inject fleet creation failures
	failOnPing   bool // inject ping failures
}

// Option configures a SimulatorBackend.
type Option func(*Options)

// WithCreateFailure causes all CreateTransportOrder and CreateStagedOrder
// calls to return an error. Use this to test fleet-outage scenarios.
func WithCreateFailure() Option {
	return func(o *Options) { o.failOnCreate = true }
}

// WithPingFailure causes Ping to return an error.
func WithPingFailure() Option {
	return func(o *Options) { o.failOnPing = true }
}
