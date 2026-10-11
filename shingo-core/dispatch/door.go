package dispatch

import (
	"log"

	"shingocore/store/orders"
)

// door.go — a door's birth, its apply and its one acquire are one unit.
//
// ── WHY ───────────────────────────────────────────────────────────────────
//
// Every order is born `pending` (v4 §2.6), and `pending` is in the scanner's
// population. A wire order's row exists before its door has finished with it:
// the door writes remaining_uop and the resolved destination after the INSERT,
// and a buried source's dig is planned from the pick the door made before it.
// A periodic pass landing in that window would acquire a half-applied order — a
// move with no remaining_uop, a robot sent to a group's name, a dispatch racing
// the dig. So the door holds the scanner's lock across all of it (RULINGS-C1-P0
// F1). Zero statements; only the lock's scope moved.
//
// The lock goes when a born row is a whole row. C2b moved the dig's pivot into
// the hand; the plan facts did not move into the birth's transaction. The wire
// door still writes remaining_uop, a group's resolved child, the payload code
// and the park's cause after its INSERT; complex intake its sibling back-link
// and park; the carried-bin recovery its pin. A door whose row IS whole at its
// INSERT holds no lock: the bin move (engine.CreateBinMove) and the Core ask
// (AcquireCoreAsk) ask for their one acquire through AcquireOne.
//
// ── WHAT A DOOR DOES INSIDE IT ────────────────────────────────────────────
//
// Birth, apply, then one acquire (acquireBorn). An order the acquire dispatches
// is told by the dispatch's own messages; one it parks is `queued` with its
// cause on the row, and its station is told once, with that cause, by the
// scanner's park. A door that parks by itself, because its decision was already
// a wait, tells the same way (parkBorn). A door never emits EventOrderQueued:
// the pass its subscriber asks for would be the second pass the door's own
// acquire replaces.
//
// NOTHING UNDER THIS LOCK MAY TAKE IT AGAIN. It is a plain mutex: a door, an
// acquire or a pass that reaches Door, AcquireOne or RunOnce on its own
// goroutine waits for itself, and Core hears no Edge and starts no order until
// it is restarted (carry-edge2, 2026-10-11). The bus runs subscribers on the
// emitting goroutine, so a subscriber that acquires, or creates an order that is
// then acquired, hands that work to the engine's after-queue (engine/after.go).
// Test and sim builds fail on a re-entry with both stacks (protocol/lockcheck).

// Acquirer is the fulfillment scanner as a door sees it: the engine wires it
// (SetAcquirer). An interface because the scanner's package imports this one.
type Acquirer interface {
	// Door runs fn under the scanner's lock, so no pass runs beside it.
	Door(fn func())
	// AcquireOne is one acquire of one order, under the scanner's lock, for a
	// caller outside Door. Reports whether the order was dispatched.
	AcquireOne(orderID int64) bool
	// AcquireLocked is one acquire of one order, for a caller inside Door. pick,
	// when not nil, is the source the door chose before its INSERT, so the
	// acquire does not find it again (a buried one: the acquire plans its dig).
	// An order it cannot dispatch is parked with the cause its arm wrote.
	// Reports whether the order was dispatched.
	AcquireLocked(orderID int64, pick *SourceResult) bool
}

// SetAcquirer wires the fulfillment scanner into the doors. A dispatcher with
// none (this package's own tests) runs a door's body unlocked and leaves a born
// order `pending`, in the scanner's population, for whoever scans: nothing
// stands in for a scan that is not there.
func (d *Dispatcher) SetAcquirer(a Acquirer) {
	d.acquirer = a
}

// door runs one door's unit under the scanner's lock.
func (d *Dispatcher) door(fn func()) {
	if d.acquirer == nil {
		fn()
		return
	}
	d.acquirer.Door(fn)
}

// acquireBorn asks for a just-born order's whole hand, once, from inside door.
// pick is the door's pre-INSERT pick when the acquire should use it (nil
// otherwise). The acquire parks what it cannot dispatch and tells the station.
func (d *Dispatcher) acquireBorn(order *orders.Order, pick *SourceResult) {
	if d.acquirer == nil {
		return
	}
	d.acquirer.AcquireLocked(order.ID, pick)
}

// parkBorn parks a just-born order whose door already knows it waits (a
// dropoff gate, a group with no free child, a finder wait): the cause is on the
// row, so the acquire would only ask the same questions again under the same
// lock. Tells the station once.
func (d *Dispatcher) parkBorn(order *orders.Order) {
	if err := d.ParkAndTell(order, "dispatcher"); err != nil {
		log.Printf("dispatch: park order %d at its door: %v", order.ID, err)
	}
}

// ParkAndTell moves a `pending` order whose cause is on its row to `queued`, and
// sends its station one `queued` update carrying that cause (the P0 rulings' Pi
// paragraph: pending → queued has no action, so whoever parks says so). An order
// already `queued` is left as it is and told nothing: its station heard when it
// parked, and hears a changed cause through the wait-change notify.
func (d *Dispatcher) ParkAndTell(order *orders.Order, actor string) error {
	if order.Status != StatusPending {
		return d.lifecycle.Park(order, actor)
	}
	if err := d.lifecycle.Park(order, actor); err != nil {
		return err
	}
	d.NotifyWaitChanged(order)
	return nil
}
