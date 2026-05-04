 package testharness
 
 import (
 	"testing"
 	"time"
 )
 
 // Eventually polls fn until it returns true or timeout elapses. Fatals the
 // test on timeout. Use this instead of time.Sleep to wait for async
 // conditions (goroutine completion, event delivery, state transition)
 // without introducing flaky fixed-duration sleeps.
 //
 // The polling interval is 2ms — fast enough for in-process synchronization
 // without burning CPU. For tests that hit external services (HTTP servers,
 // containers), consider a larger interval via EventuallyWithInterval.
 func Eventually(t *testing.T, timeout time.Duration, fn func() bool) {
 	EventuallyWithInterval(t, 2*time.Millisecond, timeout, fn)
 }
 
 // EventuallyWithInterval polls fn at the given interval until it returns
 // true or timeout elapses. Fatals the test on timeout.
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
 
 // AssertEventually polls fn until it returns true or timeout elapses.
 // Non-fatal: logs an error instead of fataling, so the test can continue
 // to collect other failures. Returns whether the condition was met.
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
