//go:build !sim

package simulator

// cancelRefused is the CancelOrder injection hook. The refusal toggle lives on
// the sim driver (deck_sim.go) and exists only under -tags sim; every other
// build answers false, so CancelOrder behaves exactly as it always has.
func (s *SimulatorBackend) cancelRefused() bool { return false }
