//go:build sim

package simulator

import (
	"fmt"
	"log"
	"strings"
	"sync"
	"sync/atomic"
)

// ── THE DECK, AND WHY THE SIM NEEDS ONE ──────────────────────────────────────
//
// Core's cancel-return path (engine/stranded_transit.go branch B, then
// engine/cancel_return.go) decides what to do with a cancelled order's bin by
// READING THE ROBOT'S JACK: loaded-at-rest means the bin is still on the deck,
// empty-at-rest means it was put down somewhere. Until this file the simulator
// reported no jack at all (JackState 0, LiftHeight 0), which
// service.RobotCarryingBin reads as "uncertain" — so every cancel in sim fell to
// the anomaly branch and the return path could not be exercised without a plant.
//
// So each named robot now carries a deck bit, driven by the blocks it actually
// completes: a load-shaped block lifts a bin onto it, an unload-shaped block
// puts the bin down. A cancel releases the robot and touches neither its deck
// nor its position — which is exactly the incident: the order is gone, the bin
// is still riding the robot, and the robot is standing where it lifted it.
//
// Alongside the deck sit the injection toggles a scenario needs: a per-robot
// fault (IsError, and no new work while it is on) and a fleet-wide "cancel does
// not take" switch. They are set from the dev HTTP routes (www/sim_routes.go),
// so unlike the rest of the driver's state they are written from a goroutine
// that is not the driver's — hence the lock.

// simLiftHeightLoaded is the mast height reported for a loaded deck. Springfield
// measured 0.0601 m on a loaded deck; anything above the resolver's 0.005 m
// threshold reads as a bin, so the sim reports the plant's number rather than
// an invented one.
const simLiftHeightLoaded = 0.06

// robotState is the per-robot physical state the driver keeps beside its pool
// bookkeeping. Every field is behind mu: the driver goroutine writes deck and
// position, the HTTP routes write fault and (by hand) deck, and status readers
// read all of it.
type robotState struct {
	mu     sync.Mutex
	known  map[string]bool   // every robot name ever minted — the set the toggles may name
	loaded map[string]bool   // true = a bin is on the deck
	fault  map[string]bool   // true = injected fault: IsError, takes no new order
	at     map[string]string // last point the robot completed a block at
	// cancelFails makes SimulatorBackend.CancelOrder refuse (see cancelRefused).
	// Atomic rather than under mu because CancelOrder reads it while holding the
	// backend's own lock, and nesting the two locks would be one more ordering
	// rule to keep.
	cancelFails atomic.Bool
}

// init readies the maps. In place, not a constructor returning a value: the
// struct holds a mutex and an atomic, and copying either is a bug vet rightly
// refuses.
func (rs *robotState) init() {
	rs.known = map[string]bool{}
	rs.loaded = map[string]bool{}
	rs.fault = map[string]bool{}
	rs.at = map[string]string{}
}

// mint records a freshly named robot so the toggles can address it.
func (rs *robotState) mint(id string) {
	rs.mu.Lock()
	rs.known[id] = true
	rs.mu.Unlock()
}

// deckLoaded reports whether a bin is on the robot's deck.
func (rs *robotState) deckLoaded(id string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.loaded[id]
}

// faulted reports whether the robot carries an injected fault.
func (rs *robotState) faulted(id string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.fault[id]
}

// blockCompleted applies what a finished block physically did to the robot
// that ran it: the deck follows the binTask and the robot is now AT the
// block's location. A Wait or a bare move changes only the position.
func (rs *robotState) blockCompleted(robotID, location, binTask string) {
	if robotID == "" {
		return
	}
	rs.mu.Lock()
	defer rs.mu.Unlock()
	switch {
	case deckLoadsOn(binTask):
		rs.loaded[robotID] = true
	case deckEmptiesOn(binTask):
		rs.loaded[robotID] = false
	}
	if location != "" {
		rs.at[robotID] = location
	}
}

// deckLoadsOn / deckEmptiesOn classify a binTask as lifting or lowering a bin.
//
// A COPY OF engine.IsPickupBlock / engine.IsDropoffBlock, and it has to be:
// engine's tests import this package, so this package importing engine is an
// import cycle. The vendor vocabulary is roboshop-configurable, which is why
// the engine matches on patterns and not an exact set — and why a copy that
// drifted would drift silently. deck_classifier_parity_test.go holds the two
// together over the vocabulary both are written for; change one, change both.
func deckLoadsOn(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "load", "pickup", "pick", "jackload", "jack_load", "fork_load", "rollerload":
		return true
	}
	if strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release") {
		return false
	}
	return strings.Contains(t, "load") || strings.Contains(t, "pick")
}

func deckEmptiesOn(binTask string) bool {
	if binTask == "" {
		return false
	}
	t := strings.ToLower(binTask)
	switch t {
	case "unload", "dropoff", "drop", "jackunload", "jack_unload", "fork_unload", "rollerunload", "release":
		return true
	}
	return strings.Contains(t, "unload") || strings.Contains(t, "drop") || strings.Contains(t, "release")
}

// ── Injection, reached from the dev HTTP routes ──────────────────────────────

// errNoDriver is what every toggle answers on a backend with no driver: there
// is no fleet to inject into, and saying so beats silently doing nothing.
var errNoDriver = fmt.Errorf("simulator: no driver running (sim fleet not started)")

func (d *Driver) requireKnown(id string) error {
	d.robots.mu.Lock()
	defer d.robots.mu.Unlock()
	if !d.robots.known[id] {
		return fmt.Errorf("simulator: no robot %q in the sim fleet", id)
	}
	return nil
}

// SetRobotFault turns an injected fault on or off for one robot. While on the
// robot reports IsError and the driver hands it no new order — pinned or not.
// An order it is already running keeps running: the fault is a status a gate
// reads, not a stall model.
func (s *SimulatorBackend) SetRobotFault(id string, on bool) error {
	d := s.typedDriver()
	if d == nil {
		return errNoDriver
	}
	if err := d.requireKnown(id); err != nil {
		return err
	}
	d.robots.mu.Lock()
	d.robots.fault[id] = on
	d.robots.mu.Unlock()
	log.Printf("[sim] INJECT robot %s fault=%v", id, on)
	return nil
}

// SetRobotDeck forces a robot's deck loaded or empty, for setting a scenario up
// by hand. It moves no bin in Core — Core learns of it only through the jack
// reading, which is the point.
func (s *SimulatorBackend) SetRobotDeck(id string, loaded bool) error {
	d := s.typedDriver()
	if d == nil {
		return errNoDriver
	}
	if err := d.requireKnown(id); err != nil {
		return err
	}
	d.robots.mu.Lock()
	d.robots.loaded[id] = loaded
	d.robots.mu.Unlock()
	log.Printf("[sim] INJECT robot %s deck loaded=%v", id, loaded)
	return nil
}

// SetCancelFails makes every CancelOrder refuse while on: the order stays
// RUNNING and its robot stays Busy, as a vendor terminate that did not take.
func (s *SimulatorBackend) SetCancelFails(on bool) error {
	d := s.typedDriver()
	if d == nil {
		return errNoDriver
	}
	d.robots.cancelFails.Store(on)
	log.Printf("[sim] INJECT cancel-fails=%v", on)
	return nil
}

// CancelFails reports whether cancels are currently being refused.
func (s *SimulatorBackend) CancelFails() bool {
	return s.cancelRefused()
}

// SimRobots is the fleet as the dev routes report it. Same snapshot as
// GetRobotsStatus, without the vendor-shaped padding.
func (s *SimulatorBackend) SimRobots() ([]FleetRobot, error) {
	d := s.typedDriver()
	if d == nil {
		return nil, errNoDriver
	}
	return d.Fleet(), nil
}

// cancelRefused is CancelOrder's hook. Sim builds only; the !sim stub answers
// false, so a production build's CancelOrder is unchanged.
func (s *SimulatorBackend) cancelRefused() bool {
	d := s.typedDriver()
	return d != nil && d.robots.cancelFails.Load()
}
