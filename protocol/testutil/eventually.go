// Package testutil provides cross-module test helpers.
package testutil

import (
	"testing"
	"time"
)

// Eventually polls fn until it returns true or timeout elapses. Fatals
// the test on timeout. Use instead of time.Sleep for async conditions.
//
// Default polling interval is 2ms — fast enough for in-process
// synchronization without burning CPU. For tests that hit external
// services (HTTP, containers), pass a larger interval via
// EventuallyWithInterval (recommended: 20ms for DB-backed waits).
func Eventually(t *testing.T, timeout time.Duration, fn func() bool) {
	EventuallyWithInterval(t, 2*time.Millisecond, timeout, fn)
}

// EventuallyWithInterval polls fn at the given interval until it
// returns true or timeout elapses. Fatals on timeout.
func EventuallyWithInterval(t *testing.T, interval, timeout time.Duration, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(interval)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// AssertEventually is the non-fatal variant. Returns whether the
// condition was met. Use when the test wants to continue collecting
// failures.
func AssertEventually(t *testing.T, interval, timeout time.Duration, fn func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if fn() {
			return true
		}
		time.Sleep(interval)
	}
	t.Errorf("condition not met within %v", timeout)
	return false
}
