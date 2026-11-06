// fleet_terminate_chase.go — noticing an order shingo ended that the fleet is
// still running, and re-sending the terminate.

package engine

import (
	"fmt"
	"time"

	"shingo/protocol/clock"
	"shingocore/fleet"
)

// The chase's bounds.
//
//   - Settle: a terminate the fleet took reports STOPPED within a poll (twice a
//     second), and the row's vendor_state follows; half a minute is far past it.
//   - Window: older rows are not chased — an order ended hours ago whose fleet
//     state was never read again is history, not a robot.
//   - Attempts: three re-sends, then a person is told and the chase stops, so a
//     fleet that refuses the terminate is not hammered with it forever.
const (
	terminateChaseInterval = 60 * time.Second
	terminateChaseSettle   = 30 * time.Second
	terminateChaseWindow   = 2 * time.Hour
	terminateChaseAttempts = 3
	terminateChaseBatch    = 20
)

// startClaimAndTerminateSweeps launches the orphaned-claim sweep and the
// terminate chase. One call from Engine.Start, which sits at its statement
// budget (funlen); the two are the release-what-ended sweeps, so they start
// together.
func (e *Engine) startClaimAndTerminateSweeps() {
	go e.claimSweepLoop()
	go e.terminateChaseLoop()
}

// auditOrderNote records a fleet note on the order's audit trail, which the
// Core order page's Notes show (www.orderNotes), and logs it. The poller's
// unreadable/readable reports reach it (pollerEmitter.audit).
func (e *Engine) auditOrderNote(orderID int64, action, detail string) {
	e.db.AppendAudit("order", orderID, action, "", detail, "system")
	e.logFn("engine: order %d: %s", orderID, detail)
}

// terminateChaseLoop runs chaseUnterminatedOrders every minute.
//
// WHY A SWEEP, and not the status event. LifecycleService.CancelOrder ends the
// order in Core whether or not the fleet took the terminate (it must: the order
// is over for shingo either way) and only logs a refusal. The poller keeps
// polling the vendor order, but it EMITS ONLY ON A STATE CHANGE — a robot the
// fleet just keeps running produces one "Core refused cancelled -> in_transit"
// log line and then silence. Silence cannot be subscribed to, so the condition
// is checked directly (house rule: be able to notice, not only be told).
//
// The attempt counts are this goroutine's own and are never shared, so they
// need no lock. Losing them on restart re-arms the chase, which is the safe
// direction.
func (e *Engine) terminateChaseLoop() {
	vc, ok := e.fleet.(fleet.VendorCommander)
	if !ok {
		return // a backend that cannot be asked about one order (the simulator)
	}
	attempts := map[int64]int{}
	ticker := clock.Default().NewTicker(terminateChaseInterval)
	defer ticker.Stop()
	for {
		select {
		case <-e.stopChan:
			return
		case <-ticker.C():
			e.chaseUnterminatedOrders(vc, attempts)
		}
	}
}

// chaseUnterminatedOrders asks the fleet about each order Core ended whose
// last-seen fleet state is still live, and:
//
//   - the fleet has ended it → records the fleet's state so the row drops out;
//   - the fleet is still running it → re-sends the terminate, up to
//     terminateChaseAttempts, each one on the order's audit trail;
//   - still running after the last → one audit row and a log line saying a
//     person has to stop it, and no further sends (the order page's
//     Re-send Terminate button remains).
//
// The text is short on purpose (owner's rule for what renders on the order
// page): the fleet state, the robot, the count.
func (e *Engine) chaseUnterminatedOrders(vc fleet.VendorCommander, attempts map[int64]int) {
	now := clock.Now().UTC()
	ended, err := e.db.ListEndedWithLiveVendorState(now.Add(-terminateChaseWindow), now.Add(-terminateChaseSettle), terminateChaseBatch)
	if err != nil {
		e.logFn("engine: terminate chase: list ended orders: %v", err)
		return
	}
	seen := make(map[int64]bool, len(ended))
	for _, o := range ended {
		seen[o.ID] = true
		d, err := vc.GetVendorOrderDetail(o.VendorOrderID)
		if err != nil {
			continue // unreadable: asked again next pass
		}
		if d.IsTerminal {
			if err := e.db.UpdateOrderVendor(o.ID, o.VendorOrderID, d.State, o.RobotID); err != nil {
				e.logFn("engine: terminate chase: record fleet state for order %d: %v", o.ID, err)
			}
			delete(attempts, o.ID)
			continue
		}
		robot := o.RobotID
		if robot == "" {
			robot = "no robot named"
		}
		n := attempts[o.ID]
		switch {
		case n < terminateChaseAttempts:
			attempts[o.ID] = n + 1
			detail := fmt.Sprintf("RDS still %s (%s); terminate re-sent %d/%d", d.State, robot, n+1, terminateChaseAttempts)
			if cErr := e.fleet.CancelOrder(o.VendorOrderID); cErr != nil {
				detail += ": " + cErr.Error()
			}
			e.db.AppendAudit("order", o.ID, "fleet_terminate_resent", d.State, detail, "system")
			e.logFn("engine: order %d (%s) is %s in Core but %s at the fleet: %s", o.ID, o.VendorOrderID, o.Status, d.State, detail)
		case n == terminateChaseAttempts:
			attempts[o.ID] = n + 1 // raised once
			detail := fmt.Sprintf("RDS still %s (%s) after %d terminates — stop it in RoboShop", d.State, robot, terminateChaseAttempts)
			e.db.AppendAudit("order", o.ID, "fleet_still_running", d.State, detail, "system")
			e.logFn("engine: ATTENTION order %d (%s): %s", o.ID, o.VendorOrderID, detail)
		}
	}
	for id := range attempts {
		if !seen[id] {
			delete(attempts, id)
		}
	}
}
