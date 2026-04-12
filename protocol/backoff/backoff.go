package backoff

import (
	"math/rand"
	"time"
)

// Exponential implements exponential backoff with optional jitter.
// Usage:
//
//	b := backoff.New(500*time.Millisecond, 5*time.Second)
//	for {
//		if try() { b.Reset(); continue }
//		time.Sleep(b.Next())
//	}
type Exponential struct {
	base    time.Duration
	max     time.Duration
	current time.Duration
}

// New creates an Exponential backoff with the given base and max durations.
func New(base, max time.Duration) *Exponential {
	return &Exponential{base: base, max: max, current: base}
}

// Next returns the current backoff duration with ±20% jitter applied,
// then doubles the backoff for the next call (capped at max).
func (b *Exponential) Next() time.Duration {
	d := b.current
	// Apply ±20% jitter: 0.8 + 0.4 * random
	jittered := time.Duration(float64(d) * (0.8 + 0.4*rand.Float64()))

	b.current *= 2
	if b.current > b.max {
		b.current = b.max
	}
	return jittered
}

// Reset returns the backoff to its base duration.
func (b *Exponential) Reset() {
	b.current = b.base
}

// Base returns the configured base duration.
func (b *Exponential) Base() time.Duration { return b.base }

// Max returns the configured max duration.
func (b *Exponential) Max() time.Duration { return b.max }

