package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/orders"
)

// handoverToFleet is the CLAIM-THEN-COMMIT handover every order-carrying dispatch
// path shares. Four sites built the same four steps by hand — the funnel
// (dispatcher.go), complex (complex_dispatch.go), and both gated valves
// (lane_gate_dispatch.go) — and three of them were missing the guards the fourth
// had.
//
// WHAT IS SHARED IS THE HANDOVER, NOT THE REQUEST. Each caller still builds its
// own fleet.CreateOrderRequest, because those are genuinely different jobs: a
// sealed two-block transport, a Complete:false plan split at a wait boundary, and
// a Complete:false unsealed waybill ending at a gate point. Merging the request
// building would gut them. Merging the handover is the whole of the duplication.
//
// ── THE ORDERING, AND WHY IT CHANGED ───────────────────────────────────────
//
//	mint → CAS the status → create the fleet order → write the id
//
// It used to be create-then-record at all four sites, so the losing caller of a
// race discovered it had lost AFTER the robot was physically committed — the old
// comment said so itself: "the robot is already committed — CreateOrder succeeded
// above." All that was left to do was chase it, by cancelling a mission that had
// already started.
//
// CASing first makes the race unlosable in the direction that costs a robot: a
// caller that loses never calls the fleet at all. Nothing forced create-first —
// the vendor order id is minted by Core at every site (mintVendorOrderID), and
// every site discards CreateOrder's return value, so no step here depends on
// anything the fleet hands back.
//
// THE ID DOES NOT MOVE, and that is deliberate rather than an oversight. An
// earlier proposal wrote the id before the create too; two guards key on
// vendor_order_id rather than on status and both would have broken:
//
//   - the exactly-once check (compound.go, "already dispatched as %s — refusing a
//     second dispatch") would have permanently refused to retry a leg whose
//     create failed;
//   - IsGateStaged requires a non-empty vendor id, so a failed create would make
//     an order LOOK gate-staged and the release evaluator would try to append a
//     tail to a fleet order that does not exist.
//
// So vendor_order_id keeps meaning exactly what it means today: the witness that
// the fleet has this job. Status means "Core has claimed it." They are different
// facts and now they are written in that order.
//
// ── THE FOUR ARMS ──────────────────────────────────────────────────────────
//
//	CAS loses                  → return. The fleet is never called. This is the point.
//	CAS wins, create fails     → return the error. The order is left claimed for the
//	                             CALLER to dispose of (see below), and the status is
//	                             NOT rolled back — rolling back invites a re-dispatch
//	                             loop against a fleet that just refused.
//	create ok, order terminal  → someone cancelled it DURING the fleet call. Cancel
//	                             the vendor order.
//	create ok, id write fails  → the fleet has a job Core cannot name. Cancel it.
//	all four ok                → the caller emits dispatched.
//
// THE ORPHAN GUARD KEEPS BOTH CAUSES. It was specified as narrowing to one, on the
// reasoning that "lost the race after committing a robot" cannot happen once the
// CAS comes first. That is true of the race between two DISPATCHERS and false of
// the race with a TERMINALIZER: TerminalizeOrder is a direct write that does not
// go through the CAS, so an operator cancel can still land while the fleet call is
// in flight, after this function has already claimed the order. Ordering fixes the
// first race and cannot touch the second — see arm 2b below, which the existing
// orphan-guard test (dispatcher_test.go, cancel injected inside CreateOrder)
// caught the moment it went missing.
//
// FAILING THE ORDER IS THE CALLER'S JOB, not this function's, and that is not a
// deferral — the callers already do it and they do it differently. dispatchToFleet
// calls failOrder with the envelope so a reply reaches Edge; DispatchDirect calls
// lifecycle.Fail with no envelope; complex calls failOrderInternal with its own
// error codes. Failing here as well would double-fail every one of them, and the
// second Fail on a terminal order is an IllegalTransition that would be logged as
// if something were wrong.
//
// The pre-dispatch terminal re-read the funnel used to do is GONE, absorbed rather
// than dropped: it was a non-atomic look at the status one statement before the
// create, and the CAS is an atomic one that also refuses a snapshot that went
// stale in any other direction. It answered a strict subset of what this now asks.
func (d *Dispatcher) handoverToFleet(order *orders.Order, req fleet.CreateOrderRequest, actor string) error {
	vendorOrderID := req.OrderID

	// 1. CLAIM. transition() compare-and-swaps on the status this caller loaded,
	// so this is the atomic claim on the order — and it is now the FIRST
	// irreversible-looking step rather than the second. A loser stops here having
	// touched nothing outside the database.
	// A FAILED CLAIM ABORTS, WHATEVER THE REASON. No fleet order is created for an
	// order this function did not successfully claim.
	//
	// This is a REVERSAL of what an earlier version of this file did, and the
	// reasoning matters because it is easy to get backwards. Under the OLD
	// create-then-record ordering, a non-race transition failure arrived AFTER the
	// robot was committed — proceeding was not a choice, it was the only remaining
	// option, so the old code logged and carried on. Porting that tolerance into
	// CAS-first looks like behaviour preservation and is actually the opposite: it
	// would create a fleet job for an order whose claim did not land, which is the
	// exact failure this reorder exists to prevent, arriving through another door.
	//
	// Two things can fail here besides a lost race, and neither justifies
	// proceeding:
	//
	//   - IllegalTransition — the order's status does not lead to `dispatched`.
	//     The caller is wrong about what it is holding.
	//   - a database error — whether this order is claimed is UNKNOWN. Creating a
	//     robot job on an unknown claim is the worst of the three outcomes.
	//
	// VERIFIED RATHER THAN ASSUMED. The tolerance was carried at first because
	// mid-transit redirect appeared to depend on it — `in_transit → dispatched` is
	// not a legal edge, so a redirect failed this call every time. That flow is now
	// refused at the handler as a never-tested stub, and with it gone the whole
	// shingo-core docker suite passes with this arm aborting. No caller depends on
	// proceeding.
	//
	// Idempotent re-entry does not need it either, and is caught earlier and
	// better: AdvanceCompoundOrder re-reads the child and refuses when
	// VendorOrderID is already set, so a re-entrant leg never reaches this
	// function.
	if err := d.lifecycle.Dispatch(order, vendorOrderID, actor); err != nil {
		if IsConcurrentTransition(err) {
			log.Printf("dispatch: order %d moved under us before the fleet order was created — "+
				"NOT dispatching (another caller owns it now): %v", order.ID, err)
			return err
		}
		log.Printf("dispatch: order %d could not be claimed for dispatch: %v — NOT dispatching "+
			"(nothing was sent to the fleet)", order.ID, err)
		return err
	}

	// 2. COMMIT. The order is claimed; this is where a robot is actually spent.
	if _, err := d.backend.CreateOrder(req); err != nil {
		// The status is NOT rolled back. The order is `dispatched` with no fleet
		// job, which the caller turns into a failure, and which the stuck sweep
		// catches independently if the caller's own write fails
		// (IsStuckSweepCandidate covers dispatched).
		log.Printf("dispatch: order %d claimed but the fleet refused the create: %v (nothing is moving; "+
			"the order is left claimed for its caller to fail)", order.ID, err)
		return err
	}

	// 2b. THE TERMINALIZER RACE, WHICH ORDERING CANNOT CLOSE.
	//
	// Moving the CAS first removes the race between two DISPATCHERS — a loser now
	// stops before spending a robot. It does NOT remove the race with a
	// TERMINALIZER, and that was missed when this reorder was specified: the claim
	// is taken before the create, but an operator cancel (or any TerminalizeOrder,
	// which is a direct write and does not go through the CAS) can still land
	// WHILE the fleet call is in flight. The CAS has already succeeded by then, so
	// it cannot catch it a second time.
	//
	// Without this arm the reorder is a regression on exactly the case the old
	// post-create guard existed for: the robot flies, the row says cancelled, the
	// order's claims have been released underneath it, and Core announces a
	// dispatch for something it has already given up on.
	//
	// So the guard keeps TWO causes, not one. They are different races and only
	// one of them is fixable by ordering.
	if fresh, ferr := d.db.GetOrder(order.ID); ferr != nil {
		// Could not tell. Proceed: the id write below is what makes the job
		// findable, and cancelling on an unreadable row would throw away a live
		// mission on a database blip.
		log.Printf("dispatch: order %d post-create re-read failed: %v (proceeding; the vendor order stands)",
			order.ID, ferr)
	} else if fresh != nil && protocol.IsTerminal(fresh.Status) {
		log.Printf("dispatch: order %d went %s while the fleet order was being created — cancelling vendor order %s",
			order.ID, fresh.Status, vendorOrderID)
		if cerr := d.backend.CancelOrder(vendorOrderID); cerr != nil {
			log.Printf("ERROR dispatch: ORPHAN MISSION — order %d is %s but vendor order %s could not be "+
				"cancelled: %v (robot may still be moving; cancel it in the fleet manager)",
				order.ID, fresh.Status, vendorOrderID, cerr)
		}
		return fmt.Errorf("order %d went %s during fleet create; vendor order %s cancelled",
			order.ID, fresh.Status, vendorOrderID)
	}

	// 3. NAME IT. The fleet has the job; the row must say which one.
	if err := d.db.UpdateOrderVendor(order.ID, vendorOrderID, "CREATED", ""); err != nil {
		// THE SURVIVING ORPHAN-GUARD CAUSE. Core cannot name this job, so nothing
		// will track it, nothing will re-track it after a restart
		// (loadActiveOrders selects on a non-empty vendor_order_id), and no later
		// cancel can reach it. Cancel it now, while the id is still in hand.
		log.Printf("dispatch: order %d vendor id %s FAILED to persist: %v — cancelling the fleet order "+
			"Core cannot name", order.ID, vendorOrderID, err)
		if cerr := d.backend.CancelOrder(vendorOrderID); cerr != nil {
			// Both ids on one line: the operator's breadcrumb for a robot moving
			// with nothing to account for it.
			log.Printf("ERROR dispatch: ORPHAN MISSION — order %d has no vendor id recorded and vendor "+
				"order %s could not be cancelled: %v (robot may still be moving; cancel it in the fleet manager)",
				order.ID, vendorOrderID, cerr)
		}
		return err
	}

	// The in-memory row matches the database, so a caller that keeps using this
	// struct (the gated valves append a tail from it) sees the id.
	order.VendorOrderID = vendorOrderID
	return nil
}
