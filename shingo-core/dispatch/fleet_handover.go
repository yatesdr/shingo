package dispatch

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/fleet"
	"shingocore/store/nodes"
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
// commitToFleet is THE CREATE SEAM: record the presence this dispatch is about
// to create, then claim-and-commit, then give the presence back if the commit
// did not happen.
//
// ── WHY IT IS ONE FUNCTION ─────────────────────────────────────────────────
//
// The two steps were written together on the plain path and then copied — badly.
// Each arm decided for itself whether to take an occupancy row, and one of them
// decided not to: a complex order asked admission "is anyone inside this lane",
// waited when the answer was yes, and then dispatched without ever appearing in
// anyone else's answer. Reader wired, writer absent, on the arm carrying most of
// both plants' lane traffic. The collision that allows has no illegal step in it
// — the next order asks, the ledger says the lane is empty because nobody wrote
// the page, and the admission that follows is lawful.
//
// A fourth copy of "remember to take the row" would have been a fourth place to
// forget. The take is structural here instead: an arm that reaches the fleet
// through this function cannot skip it, and an arm that needs no row says so by
// passing no nodes.
//
// ── THE TWO RULES IT CARRIES ───────────────────────────────────────────────
//
// TAKE BEFORE THE HANDOVER. There must be no window in which a robot is
// committed and its presence unrecorded, because a lane whose occupancy could
// not be written reads EMPTY to the next order — the same collision from the
// other side. So a failed take sends nothing at all and the caller retries.
//
// RELEASE ON EVERY FAILURE EXCEPT A LOST CAS. compound.go worked this rule out
// and it is the same rule here for the same reason: AcquireOccupancy dedups on
// (order, node), so two callers racing ONE order produce ONE row, and it belongs
// to whichever of them wins the claim. A loser that released would delete the
// winner's row and the winner would then be standing in a lane that reads empty
// to everyone else. Every other failure leaves no robot in the lane —
// handoverToFleet cancels the vendor order on the two arms where one exists — so
// there the row must go, or it wedges the lane with nothing alive to clear it.
//
// ── entering ───────────────────────────────────────────────────────────────
//
// The nodes this dispatch puts a robot into. Variadic, and EMPTY IS A REAL
// ANSWER rather than an oversight: a gated create ends at the wait point outside
// the corridor, so it passes nothing and takes nothing, and its row is taken by
// the tail append that actually enters. Nodes outside a lane resolve to no lane
// and cost one map lookup.
func (d *Dispatcher) commitToFleet(order *orders.Order, req fleet.CreateOrderRequest, actor string, entering ...*nodes.Node) error {
	if err := d.TakeLaneOccupancy(order.ID, entering...); err != nil {
		return err // nothing sent; the caller parks and the next tick retries
	}
	if err := d.assertDeclaredEveryLaneItEnters(order.ID, req, entering); err != nil {
		d.ReleaseLaneOccupancy(order.ID)
		return err // nothing sent
	}
	if err := d.handoverToFleet(order, req, actor); err != nil {
		if !IsConcurrentTransition(err) {
			d.ReleaseLaneOccupancy(order.ID)
		}
		return err
	}
	return nil
}

// assertDeclaredEveryLaneItEnters is the INVARIANT, asserted where it is created.
//
// The caller declares what it is entering; this checks that declaration against
// what the robot is actually being TOLD to do — the blocks about to go to the
// fleet. A lane named by a block the caller did not declare is a lane this
// dispatch will drive into while holding no row on it, which is F-12's exact
// shape: admission asks, the ledger under-reports, and the next entrant is
// lawfully admitted into an occupied corridor.
//
// WHY HERE RATHER THAN IN A QUERY. soakstat carries a phantom-entrant check that
// looks for executing orders holding no occupancy row, and it stays — but it is
// compensating rather than expressing. It has to GUESS where the robot is, from
// the order's source/delivery columns, which describe the whole order and not
// the step it is on; its own comment concedes the attribution is coarse and will
// false-positive on an order that has already placed. That is what a query has
// instead of an invariant.
//
// At the seam there is nothing to guess. The blocks are the instruction, the
// declaration is beside them, and the comparison is exact and at write time. So
// the query becomes a backstop for a rule enforced here, which is the right
// relationship between the two — a tripwire that never fires because the thing
// it watches for cannot be built.
//
// IT FAILS CLOSED, and refuses the dispatch rather than logging. The whole point
// is that the alternative is a robot in a lane nobody can see; a create that
// never happens is recoverable and one that happens invisibly is not. The caller
// releases and parks, and the next tick retries — the same disposition as a take
// that could not be written, for the same reason.
//
// Blocks whose location is not a Core node — the gate point most of all, which
// is a property and deliberately never resolved against nodes — resolve to
// nothing and are skipped. That is what makes the gated arm's empty declaration
// correct rather than merely tolerated: its create really does name no lane.
func (d *Dispatcher) assertDeclaredEveryLaneItEnters(orderID int64, req fleet.CreateOrderRequest, declared []*nodes.Node) error {
	held := make(map[int64]bool, len(declared))
	for _, laneID := range d.lanesFor(declared...) {
		held[laneID] = true
	}
	for _, b := range req.Blocks {
		if b.Location == "" {
			continue
		}
		node, err := d.db.GetNodeByDotName(b.Location)
		if err != nil || node == nil {
			continue // not a Core node — a gate point, or a vendor-only location
		}
		lane, err := d.db.LaneForNode(node.ID)
		if err != nil {
			// An unreadable lane is a lane we cannot prove we declared. Fail closed:
			// the same rule TakeLaneOccupancy applies to a presence it could not
			// write, and for the same reason.
			return fmt.Errorf("commit: resolve lane for block location %q on order %d: %w",
				b.Location, orderID, err)
		}
		if lane == nil || held[lane.ID] {
			continue
		}
		return fmt.Errorf(
			"commit: order %d is being sent to %q in lane %s but did not declare that lane, so it "+
				"would hold no occupancy row there — admission would report the lane empty to the "+
				"next entrant and admit it lawfully into an occupied corridor (F-12's shape). "+
				"The dispatch arm must pass every node its blocks touch to commitToFleet",
			orderID, b.Location, lane.Name)
	}
	return nil
}

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
