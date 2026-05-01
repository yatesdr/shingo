package harness

import (
	"fmt"
	"time"

	"shingo/protocol/outbox"
)

// pump drains an outbox once: every pending message is published through
// the supplied Publisher and acked. Returns the count of messages
// delivered.
//
// This intentionally re-implements protocol/outbox/drainer.go's drain()
// loop minus the retry/dead-letter branches. The production drainer's
// retry logic is timer-driven and async; tests need synchronous, single-
// pass delivery for deterministic assertions. If a future test needs to
// exercise retry behavior, use memPublisher.FailNext to inject a single
// failure and call pump twice — the second call goes through.
//
// debugTag is included in panic messages so a test that drains both sides
// can tell which one failed.
func pump(store outbox.Store, publisher outbox.Publisher, debugTag string) int {
	msgs, err := store.ListPendingOutbox(100)
	if err != nil {
		panic(fmt.Errorf("%s pump: ListPendingOutbox: %w", debugTag, err))
	}
	for _, msg := range msgs {
		if err := publisher.Publish(msg.Topic, msg.Payload); err != nil {
			// In-memory publish only fails when FailNext is set. Increment
			// retries to mirror what the production drainer would do; the
			// caller can pump again once it's cleared the failure.
			if rerr := store.IncrementOutboxRetries(msg.ID); rerr != nil {
				panic(fmt.Errorf("%s pump: IncrementOutboxRetries: %w", debugTag, rerr))
			}
			continue
		}
		if err := store.AckOutbox(msg.ID); err != nil {
			panic(fmt.Errorf("%s pump: AckOutbox: %w", debugTag, err))
		}
	}
	return len(msgs)
}

// pumpAll drains both outboxes alternately until both are empty in the
// same iteration, or maxIter iterations have run (deadlock guard).
// Returns the total messages delivered across all iterations.
//
// Why alternate: a Core handler can enqueue a reply (Core → Edge) while
// processing an inbound (Edge → Core) message, and Edge can do the
// same. A single one-direction drain doesn't catch these reply chains,
// so we keep pumping until both sides are quiet.
func pumpAll(edgeStore outbox.Store, edgePub outbox.Publisher,
	coreStore outbox.Store, corePub outbox.Publisher, maxIter int) int {
	if maxIter <= 0 {
		maxIter = 100
	}
	total := 0
	for i := 0; i < maxIter; i++ {
		edgeN := pump(edgeStore, edgePub, "edge→core")
		coreN := pump(coreStore, corePub, "core→edge")
		total += edgeN + coreN
		if edgeN == 0 && coreN == 0 {
			return total
		}
	}
	panic(fmt.Errorf("pumpAll: did not settle after %d iterations (likely a reply loop bug; total delivered=%d)",
		maxIter, total))
}

// purgeOldStub satisfies the outbox.Store.PurgeOldOutbox method on test
// fixtures that don't care about it. Tests don't run long enough to
// exercise purge logic. Concrete *store.DB implementations of the
// interface are used directly; this helper is for any future test
// fixture that needs to satisfy the interface partially.
func purgeOldStub(_ time.Duration) (int, error) { return 0, nil }
