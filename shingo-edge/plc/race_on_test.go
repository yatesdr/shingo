//go:build race

package plc

// raceEnabled is true when tests are built with -race. Race-specific
// tests can use this to skip themselves when the detector isn't
// active (the test code paths still exercise correctly without -race
// but the assertion the test is making — "no race fires" — is
// meaningless without it).
const raceEnabled = true
