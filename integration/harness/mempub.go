// Package harness wires Edge and Core in a single test process and routes
// protocol envelopes between them deterministically. See
// edge-core-harness-design.md for architecture.
package harness

import "shingo/protocol"

// memPublisher implements protocol/outbox.Publisher. Production publishes
// to a Kafka topic; the test harness publishes to the OPPOSITE side's
// ingestor synchronously, preserving the JSON envelope round-trip without
// a real broker.
//
// memPublisher is the seam where wire-format regressions are caught: the
// payload is exactly what production would put on Kafka, and the receiver
// runs the real protocol.Ingestor.HandleRaw path. If either side changes
// envelope shape, the receiver's decode fails and the test goes red — same
// as a production bad-wire incident.
type memPublisher struct {
	target *protocol.Ingestor // ingestor on the opposite side

	// failNext, when set, causes the next Publish to return this error
	// once and then reset. Used by failure-injection scenario tests
	// (retries, dead-letter behavior). Default nil = always succeed.
	failNext error
}

func newMemPublisher(target *protocol.Ingestor) *memPublisher {
	return &memPublisher{target: target}
}

// Publish hands the payload to the target ingestor and returns. Topic is
// ignored — the harness routes by source-side Pump call rather than by
// broker topic. Production topic-routing is a Kafka concern; tests aren't
// the place to validate Kafka.
func (p *memPublisher) Publish(topic string, payload []byte) error {
	if p.failNext != nil {
		err := p.failNext
		p.failNext = nil
		return err
	}
	p.target.HandleRaw(payload)
	return nil
}

// IsConnected reports the publisher is always ready. The harness has no
// meaningful disconnect state; tests that want to simulate Kafka outages
// use FailNext instead.
func (p *memPublisher) IsConnected() bool { return true }

// FailNext arms the publisher to return err on its next Publish call,
// then reset to normal. Used by tests that want to verify retry behavior
// without needing to manage a long-lived failure state.
func (p *memPublisher) FailNext(err error) { p.failNext = err }
