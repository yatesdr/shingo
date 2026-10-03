//go:build sim

package simulator

// Exported for the external parity test (deck_classifier_parity_test.go),
// which lives in simulator_test because it imports engine — and engine's own
// tests import this package, so the parity check cannot sit inside it.
var (
	DeckLoadsOn   = deckLoadsOn
	DeckEmptiesOn = deckEmptiesOn
)
