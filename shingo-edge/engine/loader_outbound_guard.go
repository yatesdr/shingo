package engine

import (
	"log"

	"shingoedge/domain"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

// ── ONE BIN LEAVES A LOADER ONCE ──────────────────────────────────────────
//
// A loader window is one physical slot holding one bin, so at most one outbound
// move can be owed at a time. It is filed by LOAD (fileLoadedOutbound), the one
// act that knows what the carrier holds, whichever way the empty that brought
// the carrier was confirmed.
//
// THERE USED TO BE TWO DOORS. The L1's completion (applyLoaderEmptyIn) filed the
// same move whenever the empty was confirmed, and LOAD filed it when it found no
// delivered L1 left to confirm. For a Core-born empty the confirm is Core's
// auto-confirm sweep, 5 to 10 minutes after the empty lands in production,
// loaded or not: a robot was sent for a carrier the operator might still be
// filling, with a blank part, so Core filled the part from the bin at dispatch
// and the robot group followed whatever the bin read by then. And two callers
// that both read the slot as owing nothing each filed a move: two orders, one
// bin. MEASURED on the lane-stress rig, 2026-08-10: PLK_X2 took in 5 empties and
// produced 10 outbound moves, the pairs 68ms apart; again on the B3 sim,
// 2026-09-26, PLK_X2 orders 190 and 191 in one second. The loser asked an empty
// node for material and reported finder-node-empty, which reads as a supply
// problem at the loader. Pinned by TestLoaderOutbound_IsFiledAtTheLoad_* and
// TestLoaderOutbound_ConfirmAndLoadTogetherFileOne.
//
// THE GUARD STAYS for a second LOAD on the same bin (a double tap, or a stale
// board): outboundMoveInFlight asks the world whether the slot already owes a
// move, and the order layer has no dedup for move orders. PushEmptyOut carries
// the same guard on the unloader side (the U2), for the same reason.
//
// fileLoadedOutbound logs both outcomes: while there were two doors the
// duplicate was invisible because one of them logged only on ERROR — eight L2
// creations in the log, sixteen orders in the database.

// outboundFor resolves where a side-cycle move leaving node goes, for both
// creators of one: fileLoadedOutbound (the L2, RoleProduce) and
// createUnloaderEmptyOut (the U2, RoleConsume).
//
// THE AGGREGATE WINS, THE CLAIM IS THE FALLBACK: Core's loader for role, when it
// names an outbound, overrides the claim's. A loader whose outbound was edited in
// Core, or retired and recreated, used to route one L2 door's move to the old
// destination and the other's to the new one.
//
// A consume window reaching LoadBin (the modal's delivered-card tap) misses the
// produce lookup and keeps the claim's outbound.
//
// ok is false for a blank outbound (nowhere to send it) and for one equal to the
// node itself (a same-node move), each logged in the sentence that site has
// always printed. LOAD used to skip the same-node check and file a move from
// the window to itself. Pinned by TestPinOutbound_* and
// TestLoadFallback_RefusesASameNodeL2.
func (e *Engine) outboundFor(node *processes.Node, claim *processes.NodeClaim, role domain.LoaderRole) (string, bool) {
	outbound := ""
	if claim != nil {
		outbound = claim.OutboundDestination
	}
	if l, err := e.loaders().LoaderAt(domain.NodeID(node.CoreNodeName), role); err == nil && l != nil && l.OutboundDest() != "" {
		outbound = l.OutboundDest()
	}
	who, leg, what := "loader", "L2", "filled"
	if role == domain.RoleConsume {
		who, leg, what = "unloader", "U2", "empty"
	}
	switch outbound {
	case "":
		e.logFn("side-cycle: %s %s has no OutboundDestination — cannot create %s (%s bin will sit until operator manually moves it)",
			who, node.Name, leg, what)
		return "", false
	case node.CoreNodeName:
		e.logFn("side-cycle: %s %s OutboundDestination same as CoreNode — skipping %s (would be a same-node move)",
			who, node.Name, leg)
		return "", false
	}
	return outbound, true
}

// outboundMoveInFlight reports whether this loader already owes an outbound
// move.
//
// KEYED ON THE CORE NODE, not only the process node. One core node is one
// physical slot, but a shared loader's windows are tracked against several
// process_node rows, and an L2 filed against a sibling row is still that slot's
// bin leaving. Scoping to the process node alone is the exact miss that orphaned
// L1s at `delivered` on a shared loader (plant 2026-06-01, see
// confirmDeliveredAt) — the same lesson, applied to the move instead of the
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

// fileLoadedOutbound files the one outbound move (the L2) a loaded bin is owed,
// or declines because the slot already owes one, and points the node's runtime
// at the move it filed so the loader board shows it next. It reports whether it
// filed one.
//
// The L2 carries the part the operator loaded, so the robot group is chosen from
// it at birth; the move is unattended at the outbound end (a supermarket node),
// so it auto-confirms, or it would stick at `delivered` with no operator to tap
// CONFIRM there.
//
// binID names the carrier the move is for when the filer read it (the LOAD's
// lost-reply arm, which files the move with no load call): Core refuses the
// move when that carrier is not on the window, and lifts it and no other. 0
// names none, the move after a LOAD Core answered.
func (e *Engine) fileLoadedOutbound(node *processes.Node, claim *processes.NodeClaim, payloadCode string, binID int64) bool {
	outbound, ok := e.outboundFor(node, claim, domain.RoleProduce)
	if !ok {
		return false
	}
	if e.outboundMoveInFlight(node.ID, node.CoreNodeName) {
		// Not a warning: a second LOAD on the same bin is the guard's whole job.
		e.debugFn("loader-outbound: %s already owes an outbound move — skipping a second one from LOAD", node.CoreNodeName)
		return false
	}
	// NoDemand: nothing asked for this. A loaded bin owes an outbound move, and
	// the guard above is the system deciding to file it — the definition of
	// structurally originless.
	order, err := e.orderMgr.CreateMoveOrderWithPayloadCodeForBin(&node.ID, 1, node.CoreNodeName, outbound, payloadCode, binID, true,
		orders.NoDemand())
	if err != nil {
		log.Printf("loader-outbound: create outbound move for %s → %s: %v", node.CoreNodeName, outbound, err)
		return false
	}
	log.Printf("loader-outbound: order %d for %s → %s payload=%q (load)", order.ID, node.CoreNodeName, outbound, payloadCode)
	if err := e.db.SetProcessNodeRuntimeActiveOrder(node.ID, &order.ID); err != nil {
		log.Printf("bin_ops: update runtime orders for node %d: %v", node.ID, err)
	}
	return true
}
