package engine

import (
	"log"

	"shingoedge/orders"
)

// ── ONE BIN LEAVES A LOADER ONCE ──────────────────────────────────────────
//
// A loader window is one physical slot holding one bin, so at most one outbound
// move can be owed at a time. Two paths create that move and neither knew about
// the other:
//
//	applyLoaderEmptyIn  — the L1 (empty-in) completion fires the L2 (filled-out)
//	LoadBin's fallback  — "no L1 in flight, create the L2 directly"
//
// The fallback is guarded by "did we just confirm an L1", which is a question
// about a moment, not about the world. Two callers that both read the loader's
// bin as empty before either stamps it will both proceed: the first confirms the
// L1 and the completion handler files an L2, the second finds no delivered L1
// left and takes the fallback. Two orders, one bin.
//
// MEASURED on the lane-stress rig, 2026-08-10: PLK_X2 took in 5 empties and
// produced 10 outbound moves; PLK_X1, 3 and 6. Exactly double on both, with the
// pairs 68ms apart. The winner carries the bin away and the loser is left asking
// an empty node for material forever — it reports finder-node-empty, which reads
// as a supply problem at the loader and sent this investigation to the plant
// file twice before it landed here. Nine of fifteen open orders were these.
//
// The duplicate was invisible because the fallback logs only on ERROR: eight L2
// creations appeared in the log and sixteen orders appeared in the database.
// createLoaderOutbound logs both outcomes for that reason.
//
// THIS IS THE UNLOADER'S GUARD, MOVED ACROSS. PushEmptyOut has carried it for
// the U2 side all along, and calls it a double-tap guard in as many words:
// "the order layer has no dedup for move orders, so two rapid taps would each
// create two U2 orders for the same physical bin". Same statement, same slot,
// same order type — the loader side simply never got it.

// outboundMoveInFlight reports whether this loader already owes an outbound
// move.
//
// KEYED ON THE CORE NODE, not only the process node. One core node is one
// physical slot, but a shared loader's windows are tracked against several
// process_node rows, and an L2 filed against a sibling row is still that slot's
// bin leaving. Scoping to the process node alone is the exact miss that orphaned
// L1s at `delivered` on a shared loader (plant 2026-06-01, see
// confirmLoaderL1OnLoad) — the same lesson, applied to the move instead of the
// retrieve.
//
// FAILS CLOSED-ISH: a read error returns "in flight" so the caller skips
// creating a second order. A missed L2 is recoverable — the operator's next LOAD
// or the reconcile sweep re-derives it — while a duplicate wedges a robot
// against an empty node until someone cancels it by hand.
func (e *Engine) outboundMoveInFlight(processNodeID int64, coreNodeName string) bool {
	active, err := e.db.ListActiveOrdersByProcessNodeOrSource(processNodeID, coreNodeName)
	if err != nil {
		log.Printf("loader-outbound: check in-flight move for node %s: %v — assuming one is in flight", coreNodeName, err)
		return true
	}
	for _, o := range active {
		if o.OrderType != orders.TypeMove {
			continue
		}
		// Only a move LEAVING this slot counts. An inbound move that happens to
		// be tracked at this process node is somebody else's bin arriving, and
		// blocking on it would stall the loader instead of protecting it.
		if o.SourceNode == coreNodeName {
			return true
		}
	}
	return false
}

// createLoaderOutbound files the one outbound move a loaded bin is owed, or
// declines because the slot already owes one.
//
// Returns the created order's ID and whether it created anything, so callers
// that bind runtime pointers can tell "filed" from "already owed" without
// re-deriving it.
func (e *Engine) createLoaderOutbound(processNodeID int64, coreNodeName, outbound, payloadCode, via string) (int64, bool) {
	if outbound == "" {
		return 0, false
	}
	if e.outboundMoveInFlight(processNodeID, coreNodeName) {
		// Not a warning: the guard doing its job is the normal outcome whenever
		// both paths see the same load. It is logged because the absence of this
		// line is what made the duplicate invisible for the whole first run.
		e.debugFn("loader-outbound: %s already owes an outbound move — skipping duplicate from %s", coreNodeName, via)
		return 0, false
	}
	// NoDemand: nothing asked for this. A loaded bin owes an outbound move, and
	// the guard above is the system deciding to file it — the definition of
	// structurally originless.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCode(&processNodeID, 1, coreNodeName, outbound, payloadCode, true,
		orders.NoDemand())
	if err != nil {
		log.Printf("loader-outbound: create outbound move for %s → %s (%s): %v", coreNodeName, outbound, via, err)
		return 0, false
	}
	log.Printf("loader-outbound: order %d for %s → %s payload=%q (%s)", order.ID, coreNodeName, outbound, payloadCode, via)
	return order.ID, true
}
