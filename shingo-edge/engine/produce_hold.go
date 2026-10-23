package engine

// produce_hold.go — the hold on every produce node.
//
// A produce seat's hold (pending_uop_delta / pending_uop_payload) is the
// strokes waiting for the next bin of their part. It is bounded on every path
// that could grow it without a bin to take it:
//
//   - an UNBOUND seat holds a tick only while a live order is placing a
//     carrier at the node (carrierComing); with none the tick is logged and
//     not held, and a standing pile of any part is dropped with a
//     hold_dropped row (dropPileAndLogTick); the strokes logged that way
//     reach Core as one hold_dropped (no_bin) row per episode, carried by
//     the write that ends it: the next bound tick's charge, the tick's hold
//     once a carrier is coming, or the scan that finds the seat bound
//     (noteNoBinStroke, sendNoBinEpisode);
//   - except BETWEEN BINS: an unbound seat whose last carrier departed
//     through Shingo holds with nothing coming, capped at one pack; the
//     stroke that would pass the pack drops the pile with the no_carrier row
//     and clears the departure, and the seat logs until the next landing
//     (betweenBins, dropPileAndLogTick);
//   - the stranded monitor drops such a pile between ticks, every scan and
//     once at boot (sweepProduceHold), except a hold between bins; the boot
//     pass is what writes off a pile that stood before this rule existed;
//   - a bound seat at its pack holds without an order, capped at one pack
//     (the strict arm, splitStrict);
//   - a cutover clears the departing part's hold, in the strand's
//     transaction (cutoverHoldClears);
//   - a sequential flip carries the released position's hold to its
//     partner (carryFlipBucket).
//
// Every drop writes one hold_dropped row, through writeSeatWithDrops, in the
// seat write's own transaction.

import (
	"database/sql"
	"fmt"
	"log"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/orders"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// The actors a hold_dropped row names for the drops written here.
const (
	holdDropBySweep   = "edge:sweep"
	holdDropByCutover = "edge:cutover"
	holdDropByFlip    = "edge:flip"
)

// The once-per-episode notes of carrierComing's answers (carrierNoteLogged).
const (
	holdNoteNoCarrier  = "no_carrier"
	holdNoteUnreadable = "unreadable"
)

// carrierComing asks whether a live order is placing a carrier at the node,
// for an unbound produce seat about to hold (store.DB.LiveOrderPlacing: one
// statement, only on that path; a bound tick never asks). It fails closed: a
// read that errors, or a live leg whose release facts cannot be read, answers
// yes, so nothing is dropped on a guess, and says so. Caller holds countMu.
func (e *Engine) carrierComing(node *processes.Node) bool {
	placing, err := e.db.LiveOrderPlacing(node.CoreNodeName)
	p := placing[node.CoreNodeName]
	switch {
	case err != nil:
		log.Printf("produce hold: node %s: %v -- read as a carrier coming: the hold is kept and nothing is dropped",
			node.CoreNodeName, err)
		return true
	case p == orders.PlacingUnreadable:
		e.noteCarrier(node, holdNoteUnreadable, "a live order's release facts cannot be read, so one may be placing a "+
			"carrier here -- the hold is kept and nothing is dropped")
		return true
	case p == orders.PlacingCarrier:
		e.endCarrierNote(node.ID)
		return true
	}
	return false
}

// betweenBins answers, for an unbound produce seat with no carrier coming,
// whether a tick of input strokes of part is held between bins: the seat's
// last carrier departed through Shingo (carrierGone, the mark carrierLeft
// writes on the row the tick already read), the seat holds nothing of another
// part, and the pile with the tick stays within pack (the claim's
// UOPCapacity). pastPack: everything but the size says hold, and the tick
// would take the pile past the pack. A real gap between bins never makes a
// whole bin on the table, so that stroke proves nobody is coming for these
// parts (dropPileAndLogTick). A claim with no pack never holds here. The
// sweep asks it with input 0 (sweepProduceHold). Pure.
func betweenBins(rt *processes.RuntimeState, part string, pack, input int) (hold, pastPack bool) {
	if pack <= 0 || !carrierGone(rt) || !holdIsOf(rt.PendingUOPPayload, part) {
		return false, false
	}
	if int(rt.PendingUOPDelta)+input > pack {
		return false, true
	}
	return true, false
}

// dropPileAndLogTick is chargeOrHold's arm for an unbound produce seat with no
// carrier coming that does not hold between bins (betweenBins). The tick is
// not held: nothing will take it, and it is logged once per episode
// (noteCarrier). A standing pile is dropped with one hold_dropped (no_carrier)
// row in the drop's transaction, whatever its part: with no part test in the
// predicate, nothing is coming for another part's pile either. pastPack (the
// tick would take a between-bins pile past the pack) also clears the
// departure mark in that transaction (CarrierNotSeen), so the seat logs from
// this tick until the next landing. A refused drop (the seat changed since it
// was read) returns written false, and applyHoldAndReplay re-reads once. (A
// produce hold is never negative; a negative one of another part, which only a
// seat whose claim changed role could carry, is dropped too.)
func (e *Engine) dropPileAndLogTick(node *processes.Node, rt *processes.RuntimeState, input int, tick string, pastPack bool) (bound bool, binAttributed int, written bool) {
	seat := processes.SeatAsReadOf(rt)
	held, heldPart := int(rt.PendingUOPDelta), rt.PendingUOPPayload
	dropPile := held > 0 || (held != 0 && !holdIsOf(heldPart, tick))
	if dropPile || pastPack {
		var drops []protocol.HoldDropped
		if dropPile {
			drops = append(drops, protocol.HoldDropped{PayloadCode: heldPart, Qty: int64(held), Reason: protocol.HoldDropNoCarrier})
		}
		ok, err := e.writeSeatWithDrops(node, drops, func(ex processes.DBTX) (bool, error) {
			if ok, err := processes.SetSeatHold(ex, node.ID, 0, "", seat); err != nil || !ok || !pastPack {
				return ok, err
			}
			return true, e.recordLinesideCarrierIn(ex, node.ID, domain.UnknownCarrier(), domain.CarrierNotSeen)
		})
		if err != nil {
			log.Printf("produce hold: node %s: drop %d of part %q with no carrier coming: %v", node.CoreNodeName, held, heldPart, err)
			return false, 0, true
		}
		if !ok {
			return false, 0, false
		}
		e.endUnheldEpisode(node.ID)
		if pastPack {
			log.Printf("produce hold: node %s: %d of part %s held since its carrier left would pass the pack with "+
				"nothing coming -- dropped; the departure is forgotten and the seat logs until the next landing",
				node.CoreNodeName, held+input, tick)
		}
	}
	e.noteCarrier(node, holdNoteNoCarrier, fmt.Sprintf("%d of part %s not held -- no bin is bound and no live order is "+
		"placing a carrier here, so nothing would take it", input, tick))
	e.noteNoBinStroke(node.ID, tick, input)
	return false, 0, true
}

// noteNoBinStroke adds a logged tick to the node's no-bin episode: input
// strokes of part, added to the episode's row for that part. No statement:
// the episode reaches Core once, when it ends. Caller holds countMu.
func (e *Engine) noteNoBinStroke(nodeID int64, part string, input int) {
	if e.noBinStrokes == nil {
		e.noBinStrokes = map[int64][]protocol.HoldDropped{}
	}
	rows := e.noBinStrokes[nodeID]
	if n := len(rows); n > 0 && rows[n-1].PayloadCode == part {
		rows[n-1].Qty += int64(input)
	} else {
		rows = append(rows, protocol.HoldDropped{PayloadCode: part, Qty: int64(input), Reason: protocol.HoldDropNoBin})
	}
	e.noBinStrokes[nodeID] = rows
}

// noBinRows is a copy of the node's open no-bin episode's rows, nil when none
// is open, for the write that ends the episode to carry in its transaction
// (writeSeatWithDrops stamps the copies). Caller holds countMu.
func (e *Engine) noBinRows(nodeID int64) []protocol.HoldDropped {
	rows := e.noBinStrokes[nodeID]
	if len(rows) == 0 {
		return nil
	}
	return append([]protocol.HoldDropped(nil), rows...)
}

// endNoBinEpisode forgets a node's no-bin episode once the write that carried
// its rows has committed. A refused or failed write keeps it, and the next
// tick or scan that ends it carries the rows again. Caller holds countMu.
func (e *Engine) endNoBinEpisode(nodeID int64, rows []protocol.HoldDropped) {
	if len(rows) != 0 {
		delete(e.noBinStrokes, nodeID)
	}
}

// sendNoBinEpisode ends a node's open no-bin episode where no seat write ends
// it (the scan finds the seat bound; a tick finds a carrier coming but the
// seat holds another part's ticks): its rows alone, in one transaction. by
// names the actor. Caller holds countMu.
func (e *Engine) sendNoBinEpisode(node *processes.Node, by string) {
	rows := e.noBinRows(node.ID)
	if rows == nil {
		return
	}
	for i := range rows {
		rows[i].CalledBy = by
	}
	ok, err := e.writeSeatsInTx(node, rows, func(processes.DBTX) (bool, error) { return true, nil })
	if err != nil || !ok {
		log.Printf("produce hold: node %s: send the no-bin strokes: %v; kept for the next end", node.CoreNodeName, err)
		return
	}
	e.endNoBinEpisode(node.ID, rows)
}

// noteCarrier logs, once per episode, what carrierComing found at an unbound
// produce seat; the episode ends when a carrier is coming or a bin binds
// (endCarrierNote). An unreadable order's line also goes to the diagnostics
// page (debugFn): the pile it keeps has no Core record while it stands.
// Caller holds countMu.
func (e *Engine) noteCarrier(node *processes.Node, kind, text string) {
	if e.carrierNoteLogged == nil {
		e.carrierNoteLogged = map[int64]string{}
	}
	if e.carrierNoteLogged[node.ID] == kind {
		return
	}
	e.carrierNoteLogged[node.ID] = kind
	const line = "produce hold: node %s: %s; logged once until a carrier is coming or a bin binds"
	log.Printf(line, node.CoreNodeName, text)
	if kind == holdNoteUnreadable {
		e.debugFn(line, node.CoreNodeName, text)
	}
}

// endCarrierNote ends noteCarrier's episode for a node. Caller holds countMu.
func (e *Engine) endCarrierNote(nodeID int64) {
	if e.carrierNoteLogged != nil {
		delete(e.carrierNoteLogged, nodeID)
	}
}

// cutoverClears is the cutover's clear of the departing part's produce holds:
// one guarded drop per seat, written on the strand's transaction (writeIn) and
// announced after its commit (queued).
type cutoverClears struct {
	seats []cutoverClear
}

type cutoverClear struct {
	nodeID int64
	seat   processes.SeatAsRead
	q      queuedDrops
	landed bool
}

// cutoverHoldClears reads what the cutover clears: each produce seat of the
// process holding ticks whose part is not the part of the node's incoming
// produce claim (keepsHoldAcrossCutover), with its hold_dropped (cutover) row
// built. proc is the process before the flip (its active style the departing
// one) and styleID the incoming style. Two statements (the nodes, their
// runtime rows), and the claims of both styles only when a seat holds
// anything. Caller holds countMu, so the seats read here are the seats the
// clear writes; each write still names its seat.
func (e *Engine) cutoverHoldClears(proc *processes.Process, styleID *int64) (*cutoverClears, error) {
	nodes, err := e.db.ListProcessNodesByProcess(proc.ID)
	if err != nil {
		return nil, err
	}
	runtimes, err := e.db.ProcessNodeRuntimes(nodeIDsOf(nodes))
	if err != nil {
		return nil, err
	}
	var holding []*processes.Node
	for i := range nodes {
		if rt := runtimes[nodes[i].ID]; rt != nil && rt.PendingUOPDelta > 0 {
			holding = append(holding, &nodes[i])
		}
	}
	c := &cutoverClears{}
	if len(holding) == 0 {
		return c, nil
	}
	incoming := *proc
	incoming.ActiveStyleID = styleID
	claims, err := e.db.NodeClaimsForStyles(store.ClaimStyleIDs(proc, &incoming))
	if err != nil {
		return nil, err
	}
	for _, node := range holding {
		rt := runtimes[node.ID]
		was := claims.Resolve(proc, node, store.ActiveStyleFirst)
		if was == nil || was.Role != protocol.ClaimRoleProduce || was.IsLoaderNode() ||
			keepsHoldAcrossCutover(rt.PendingUOPPayload, claims.Resolve(&incoming, node, store.ActiveStyleFirst)) {
			continue
		}
		drop := protocol.HoldDropped{PayloadCode: rt.PendingUOPPayload, Qty: rt.PendingUOPDelta,
			Reason: protocol.HoldDropCutover, CalledBy: holdDropByCutover}
		q, err := e.holdDrops(node, []protocol.HoldDropped{drop})
		if err != nil {
			return nil, err
		}
		c.seats = append(c.seats, cutoverClear{nodeID: node.ID, seat: processes.SeatAsReadOf(rt), q: q})
	}
	return c, nil
}

// keepsHoldAcrossCutover: a hold survives the cutover only when the node's
// incoming claim makes the same part (a same-part changeover). A hold recorded
// with no part replays onto any part, so it never survives one.
func keepsHoldAcrossCutover(heldPart string, incoming *processes.NodeClaim) bool {
	return heldPart != "" && incoming != nil && incoming.Role == protocol.ClaimRoleProduce &&
		incoming.PayloadCode == heldPart
}

// writeIn drops each seat's hold and queues its row on the strand's
// transaction. A seat that changed since it was read is left as it is, with no
// row; under countMu that cannot happen, and the guard costs nothing.
func (c *cutoverClears) writeIn(tx *sql.Tx) error {
	for i := range c.seats {
		s := &c.seats[i]
		ok, err := writeSeatAndDropsIn(tx, func(ex processes.DBTX) (bool, error) {
			return processes.SetSeatHold(ex, s.nodeID, 0, "", s.seat)
		}, s.q)
		if err != nil {
			return err
		}
		s.landed = ok
	}
	return nil
}

// queued announces each drop that landed, after the strand's commit.
func (c *cutoverClears) queued() {
	for _, s := range c.seats {
		if s.landed {
			dropsQueued(s.q)
		} else {
			log.Printf("produce hold: node %s: the seat changed before the cutover cleared its hold; left as it is", s.q.node)
		}
	}
}

// flipCarry is one sequential flip's division of A's bucket at B: what B's
// bound bin takes (charge), the hold B keeps (hold of holdPart), and what fits
// nowhere (drops, flip_carry_overflow).
type flipCarry struct {
	charge, hold int
	holdPart     string
	drops        []protocol.HoldDropped
}

// splitFlipCarry divides a bucket of part carried onto B, from B's seat as
// read: bound, its count, its hold (held of heldPart), and pack (B's strict
// pack, strictPackFor; 0 when B's claim is not strict, and then no pack caps
// it). Pure.
//
//   - B's bound bin takes MIN(bucket, room), room = MAX(0, pack - count), or
//     all of it when pack is 0. An unbound B takes nothing into a bin.
//   - The rest joins B's hold of the part, capped at one pack on a strict
//     claim; the excess is dropped.
//   - When B holds ANOTHER part's ticks, that hold is kept (it is not
//     relabelled), and the rest, which can join no hold, is dropped.
func splitFlipCarry(bucket int, bound bool, count, held int, heldPart, part string, pack int) flipCarry {
	var c flipCarry
	if bound {
		room := bucket
		if pack > 0 {
			room = max(0, pack-count)
		}
		c.charge = min(bucket, room)
	}
	rest := bucket - c.charge
	overflow := func(n int) {
		c.drops = append(c.drops, protocol.HoldDropped{PayloadCode: part, Qty: int64(n), Reason: protocol.HoldDropFlipCarryOverflow})
	}
	if held != 0 && !holdIsOf(heldPart, part) {
		c.hold, c.holdPart = held, heldPart
		if rest > 0 {
			overflow(rest)
		}
		return c
	}
	c.hold, c.holdPart = held+rest, heldPart
	if rest > 0 {
		c.holdPart = part
	}
	if pack > 0 && c.hold > pack {
		overflow(c.hold - pack)
		c.hold = pack
	}
	return c
}

// carryFlipBucket moves A's bucket to B at a sequential flip (commitFlip):
// between A's bin reaching its pack and the operator's RELEASE, the parts were
// going into B. It runs only when both positions are produce claims and A
// holds ticks of B's part (holdIsOf). B's bin takes what fits (splitFlipCarry)
// and emits it as one produce_tick on B's bin; the rest is B's hold; A's
// bucket goes to 0. Both seats are written in ONE transaction, each guarded on
// the seat as read here (writeSeatsInTx), with any flip_carry_overflow row in
// it; a refusal changes neither. A bucket of another part is not carried: it
// stays on A, for A's next bin of its part (or the tick's and the sweep's
// rules once A is unbound).
//
// Caller holds countMu. One statement (both runtime rows) per flip, and only
// when A holds ticks the process and its claims (two more) and the write.
func (e *Engine) carryFlipBucket(node, partner *processes.Node, orderUUID string) {
	rts, err := e.db.ProcessNodeRuntimes([]int64{node.ID, partner.ID})
	if err != nil {
		log.Printf("release flip: node %s: read the seats to carry the hold: %v", node.CoreNodeName, err)
		return
	}
	a, b := rts[node.ID], rts[partner.ID]
	if a == nil || b == nil || a.PendingUOPDelta <= 0 {
		return
	}
	aClaim, bClaim := e.flipClaims(node, partner)
	if aClaim == nil || aClaim.Role != protocol.ClaimRoleProduce || bClaim == nil || bClaim.Role != protocol.ClaimRoleProduce {
		return
	}
	part := producedPayload(bClaim)
	if !holdIsOf(a.PendingUOPPayload, part) {
		log.Printf("release flip: node %s: its hold of %d of part %q is not carried to %s, which makes %s; it stays for %s's next bin of its part",
			node.CoreNodeName, a.PendingUOPDelta, a.PendingUOPPayload, partner.CoreNodeName, part, node.CoreNodeName)
		return
	}
	c := splitFlipCarry(int(a.PendingUOPDelta), b.ActiveBinID != nil, b.RemainingUOPCached,
		int(b.PendingUOPDelta), b.PendingUOPPayload, part, strictPackFor(bClaim))
	for i := range c.drops {
		c.drops[i].CalledBy, c.drops[i].OrderID = holdDropByFlip, orderUUID
	}
	seatA, seatB := processes.SeatAsReadOf(a), processes.SeatAsReadOf(b)
	ok, err := e.writeSeatsInTx(partner, c.drops, func(ex processes.DBTX) (bool, error) {
		if ok, err := processes.SetSeatHold(ex, node.ID, 0, "", seatA); err != nil || !ok {
			return ok, err
		}
		// The charge is decided above on B's count as read, which the statement
		// pins; a cap of count + charge makes it charge exactly that.
		return processes.ChargeSeatUOPStrict(ex, partner.ID, c.charge, b.RemainingUOPCached+c.charge, c.hold, c.holdPart, seatB)
	})
	switch {
	case err != nil:
		log.Printf("release flip: node %s: carry its hold of %d to %s: %v", node.CoreNodeName, a.PendingUOPDelta, partner.CoreNodeName, err)
		return
	case !ok:
		log.Printf("release flip: node %s: a seat changed under the count lock; its hold of %d is not carried to %s",
			node.CoreNodeName, a.PendingUOPDelta, partner.CoreNodeName)
		return
	}
	e.endUnheldEpisode(node.ID)
	log.Printf("release flip: node %s: its hold of %d of part %s carried to %s (bin +%d, held %d)",
		node.CoreNodeName, a.PendingUOPDelta, part, partner.CoreNodeName, c.charge, c.hold)
	if c.charge > 0 && e.inventoryDelta != nil {
		_ = e.inventoryDelta.Produced(uop.TickEvent{NodeID: partner.ID, BinID: *b.ActiveBinID, PayloadCode: part,
			BinEpoch: b.ActiveBinEpoch, BinRemainder: c.charge})
	}
}

// flipClaims resolves both positions' claims the way the tick does (the
// active style first), in two statements: the process and its claims.
func (e *Engine) flipClaims(node, partner *processes.Node) (*processes.NodeClaim, *processes.NodeClaim) {
	proc, err := e.db.GetProcess(node.ProcessID)
	if err != nil {
		return nil, nil
	}
	claims, err := e.db.NodeClaimsForStyles(store.ClaimStyleIDs(proc))
	if err != nil {
		return nil, nil
	}
	return claims.Resolve(proc, node, store.ActiveStyleFirst), claims.Resolve(proc, partner, store.ActiveStyleFirst)
}

// sweepProduceHold is the stranded monitor's produce arm, run on every scan
// and once at boot: an UNBOUND produce seat with a hold and no carrier coming
// has its hold dropped with one hold_dropped (sweep) row, unless the tick
// would keep it: a hold between bins (betweenBins) is the tick's to drop. rt
// is the seat as the scan read it, before the lock, and the drop names it in
// its WHERE, so a bind, a tick or a landing written in between refuses the
// drop and the next pass looks again. A bound seat is never touched: a bin at
// its pack keeps its bucket for the next bin whether or not an order exists
// yet. A seat that is not an unbound produce seat with a hold, or that holds
// between bins, costs no statement.
func (e *Engine) sweepProduceHold(node *processes.Node, claim *processes.NodeClaim, rt *processes.RuntimeState) {
	if claim == nil || claim.Role != protocol.ClaimRoleProduce || claim.IsLoaderNode() || rt == nil {
		return
	}
	if rt.ActiveBinID != nil {
		// A bin bound since a no-bin episode's last tick ends the episode
		// here when no tick has yet.
		e.countMu.Lock()
		e.sendNoBinEpisode(node, holdDropBySweep)
		e.countMu.Unlock()
		return
	}
	if rt.PendingUOPDelta <= 0 {
		return
	}
	if hold, _ := betweenBins(rt, producedPayload(claim), claim.UOPCapacity, 0); hold {
		return
	}
	e.countMu.Lock()
	defer e.countMu.Unlock()
	if e.carrierComing(node) {
		return
	}
	drop := protocol.HoldDropped{PayloadCode: rt.PendingUOPPayload, Qty: rt.PendingUOPDelta,
		Reason: protocol.HoldDropSweep, CalledBy: holdDropBySweep}
	ok, err := e.writeSeatWithDrops(node, []protocol.HoldDropped{drop}, func(ex processes.DBTX) (bool, error) {
		return processes.SetSeatHold(ex, node.ID, 0, "", processes.SeatAsReadOf(rt))
	})
	switch {
	case err != nil:
		log.Printf("produce hold: node %s: sweep of %d of part %q: %v", node.CoreNodeName, drop.Qty, drop.PayloadCode, err)
	case !ok:
		log.Printf("produce hold: node %s: the seat changed since the sweep read it; its hold is left for the next pass",
			node.CoreNodeName)
	default:
		e.endUnheldEpisode(node.ID)
	}
}
