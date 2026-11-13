package engine

import (
	"fmt"
	"log"
	"strconv"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store/processes"
	"shingoedge/uop"
)

// fencedCount applies a count Core fenced (SYNTH-round2 S7, citrine-kestrel
// §8 S4) to the carrier bound at node. It reports handled=false when the
// adjustment is not a fence this station can use, and the caller then takes
// the number as is, which is what every station did before the fence:
//
//   - no AsOfNet/AsOfSeq (a Core that predates the fence, or a count Core could
//     not fence: no station, or two, counting the carrier in this generation);
//   - AsOfStation is another station (the net measures its stream, not ours);
//   - the slot does not hold adj.BinID at adj.Epoch (an Edge behind on the
//     generation takes the epoch with the absolute count, as before);
//   - no accumulator wired.
//
// When it is handled, the slot gets
//
//	remaining = NewRemaining + (flushed_net - AsOfNet) + unflushed
//
// Core wrote NewRemaining having applied this station's stream up to AsOfNet.
// Every window after that point is either on the wire (flushed, in the net) or
// still in the accumulator (unflushed); Core will apply both on top of its
// number, and this adds both to ours, so the two sides agree. A window already
// on the wire when the operator counted is taken off both sides: low if the
// counter had already seen those parts gone, which is the safe direction.
//
// An adjustment whose AsOfSeq is older than the fenced count the slot already
// holds for the same carrier and generation is refused (a late delivery; see
// processes.SetCountFenced). applied reports whether the count landed.
//
// A PERSON'S COUNT WINS (D5): when the adjustment is a person's
// (protocol.IsLifecycleActor false: the bins page, the line's Record Count)
// the count clears the seat's hold in the same transaction, with one
// hold_dropped (no_carrier) row by edge:recount (writeCountClearingHold). A
// Core lifecycle announcement never touches the hold.
//
// Cost: one SELECT (the flushed net) on top of the write the absolute path
// already does. The write is one statement, as before; a person's count on a
// seat with a hold adds the runtime read and the hold's drop, in one
// transaction.
func (e *Engine) fencedCount(nodeID int64, rt *processes.RuntimeState, adj protocol.UOPAdjustment) (handled, applied bool, remaining int) {
	if adj.AsOfNet == nil || adj.AsOfSeq == nil || e.inventoryDelta == nil {
		return false, false, 0
	}
	if adj.AsOfStation != e.cfg.StationID() {
		return false, false, 0
	}
	if rt.ActiveBinID == nil || *rt.ActiveBinID != adj.BinID || rt.ActiveBinEpoch != adj.Epoch {
		return false, false, 0
	}

	// ONE INSTANT, the lineside report's way (uop.Mutator.WithPending): the
	// flushed net and the unflushed snapshot are read under the accumulator's
	// flush lock, so no flush moves a window from one to the other between the
	// two reads; and under countMu, which every tick holds across its runtime
	// write and its accumulator record, so no tick lands in the count we
	// overwrite without also being in the drift we add. The write happens
	// inside both locks: nothing moves the slot's count between the read and
	// it. Lock order countMu, then the flush lock, as everywhere.
	var (
		drift      int64
		ok         bool
		readFailed bool
	)
	e.countMu.Lock()
	err := e.inventoryDelta.WithPending(func(p uop.Pending) error {
		flushed, err := e.db.InventoryDeltaNet(protocol.InvDeltaScopeBin, strconv.FormatInt(adj.BinID, 10), adj.Epoch)
		if err != nil {
			readFailed = true
			return err
		}
		drift = flushed - *adj.AsOfNet + int64(p.Bin(adj.BinID, adj.Epoch))
		remaining = adj.NewRemaining + int(drift)
		seat := &processes.Node{ID: nodeID, CoreNodeName: adj.CoreNodeName}
		ok, err = e.writeCountClearingHold(seat, !protocol.IsLifecycleActor(adj.Actor), func(ex processes.DBTX) (bool, error) {
			return processes.SetCountFenced(ex, nodeID, adj.BinID, adj.Epoch, remaining, *adj.AsOfSeq)
		})
		return err
	})
	e.countMu.Unlock()
	if err != nil {
		// Handled, not applied. Falling back to the absolute write would
		// throw away exactly the windows the fence exists to keep.
		log.Printf("uop_adjustment: fenced count for bin %d at node %s (read failed %t): %v — keeping the current count",
			adj.BinID, adj.CoreNodeName, readFailed, err)
		return true, false, 0
	}
	if !ok {
		log.Printf("uop_adjustment: fenced count %d for bin %d at node %s (as of seq %d) not applied: "+
			"the slot holds a count taken later in the stream, or the carrier moved",
			adj.NewRemaining, adj.BinID, adj.CoreNodeName, *adj.AsOfSeq)
		return true, false, 0
	}
	log.Printf("uop_adjustment: fenced count bin %d at node %s: counted %d, as of net %d seq %d, drift %+d -> %d",
		adj.BinID, adj.CoreNodeName, adj.NewRemaining, *adj.AsOfNet, *adj.AsOfSeq, drift, remaining)
	return true, true, remaining
}

// holdDropByRecount is the actor of the hold drop a person's count writes (D5).
const holdDropByRecount = "edge:recount"

// writeCountClearingHold runs a count's write and, for a person's count
// (person), in the same transaction: drops the seat's standing hold with one
// hold_dropped (no_carrier) row by edge:recount, at any count (D5), and lifts
// a stopped seat's stop (U1). The hold was the line's guess at parts for this
// seat, and the person at the cart has just said what is in it. Before it, a
// count below the pack reopened room and the next tick replayed the hold into
// it (SPR ALN_011, 2026-11-12: a recount of 132 to 67 read 132 again 4 s
// later). A count that does not land writes nothing. A lifecycle count writes
// alone and touches neither the hold nor the stop: the clear follows who
// counted, not which statement ran. Caller holds countMu.
func (e *Engine) writeCountClearingHold(node *processes.Node, person bool, write func(processes.DBTX) (bool, error)) (bool, error) {
	if !person {
		return write(e.db.DB)
	}
	var rows []protocol.HoldDropped
	var pile int64
	var stopped bool
	if rt, err := e.db.GetProcessNodeRuntime(node.ID); err == nil && rt != nil {
		stopped = rt.HoldStopped
		if rt.PendingUOPDelta != 0 {
			pile = rt.PendingUOPDelta
			rows = append(rows, protocol.HoldDropped{PayloadCode: rt.PendingUOPPayload, Qty: pile,
				Reason: protocol.HoldDropNoCarrier, CalledBy: holdDropByRecount})
		}
	}
	ok, err := e.writeEndingHold(node, rows, stopped, func(ex processes.DBTX) (bool, error) {
		if ok, err := write(ex); err != nil || !ok || pile == 0 {
			return ok, err
		}
		return processes.DropSeatHold(ex, node.ID, pile)
	})
	if err == nil && ok && pile != 0 {
		e.endUnheldEpisode(node.ID)
	}
	return ok, err
}

// writeEndingHold runs write with rows, as writeSeatWithDrops does, and when
// lift, also lifts the seat's stop (processes.LiftSeatStop) once write has
// landed, in the same transaction (writeSeatsInTx: lift or rows, never one
// without the other). A seat read unstopped costs no statement more. Caller
// holds countMu.
func (e *Engine) writeEndingHold(node *processes.Node, rows []protocol.HoldDropped, lift bool,
	write func(processes.DBTX) (bool, error)) (bool, error) {
	if !lift {
		return e.writeSeatWithDrops(node, rows, write)
	}
	return e.writeSeatsInTx(node, rows, func(ex processes.DBTX) (bool, error) {
		if ok, err := write(ex); err != nil || !ok {
			return ok, err
		}
		return true, processes.LiftSeatStop(ex, node.ID)
	})
}

// writeReleaseCount writes the old carrier's count at a release and reads the
// stream fence the release carries (Lane H, DECISIONS D11 (i)): the count the
// release declares is as of THIS point in the carrier's (bin, epoch) delta
// stream — the last seq allocated and the flushed net plus what the
// accumulator holds unflushed. Core writes the declared count with no
// generation bump and moves its dedup cursor to the fence, so a delta flushed
// before the release and delivered after it is skipped (its count is in the
// declaration) and every later window applies only the ticks after it.
//
// ONE INSTANT, as fencedCount reads it, and it holds for the count too: under
// countMu, which every tick holds across its runtime write and its record, and
// the accumulator's flush lock (WithPending), with the count write inside
// both. A tick is either before the write and in the fence, or after both and
// charged on top of the declaration, in both books. With no count a person
// typed (fromSeat, ReleaseDisposition.CountFromSeat) the declared count is the
// seat's as the statement finds it, not the leg's plan-time read: a stroke
// between the plan and the commit — across the act's Core round trip for the
// release point — is in the count and in AsOfNet together (decision 44a). A
// typed count is the person's and is written as is. Cost: one statement, the
// count write, with the cursor read in its RETURNING
// (store.ReleaseCountWithCursor).
//
// Returns the fence and the count written, which goes on the envelope. The
// fence is nil — Core bumps at the release, as for an Edge that cannot fence —
// when no accumulator is wired, the leg read no runtime row, no carrier is
// bound at the seat, the bound carrier is not the released one, the seat has
// no runtime row, or the statement fails; the count written is then the
// leg's own. Every release logs its declaration and its fence, or which of
// those arms it took.
func (e *Engine) writeReleaseCount(node *processes.Node, rt *processes.RuntimeState, binID int64, count int, fromSeat bool) (*protocol.ReleaseFence, int) {
	source, declared := "typed", &count
	if fromSeat {
		source, declared = "seat", nil
	}
	unfenced := func(why string, write bool) (*protocol.ReleaseFence, int) {
		if write {
			if err := e.db.SetProcessNodeReleaseCount(node.ID, count, clock.Now()); err != nil {
				log.Printf("release: finalize old-bin count for node %s: %v", node.CoreNodeName, err)
			}
		}
		log.Printf("release: node %s declared %d (%s, as the leg read it) — unfenced: %s", node.CoreNodeName, count, source, why)
		return nil, count
	}
	switch {
	case e.inventoryDelta == nil:
		return unfenced("no accumulator", true)
	case rt == nil:
		return unfenced("the leg read no runtime row", true)
	case rt.ActiveBinID == nil:
		return unfenced("no carrier bound at the seat", true)
	case binID != 0 && *rt.ActiveBinID != binID:
		return unfenced(fmt.Sprintf("the bound carrier %d is not the released %d", *rt.ActiveBinID, binID), true)
	}
	bin, epoch := *rt.ActiveBinID, rt.ActiveBinEpoch
	var (
		fence   *protocol.ReleaseFence
		written int
		found   bool
	)
	e.countMu.Lock()
	err := e.inventoryDelta.WithPending(func(p uop.Pending) error {
		w, seq, net, ok, err := e.db.ReleaseCountWithCursor(node.ID, declared, protocol.InvDeltaScopeBin, strconv.FormatInt(bin, 10), epoch, clock.Now())
		if err != nil || !ok {
			return err
		}
		written, found = w, true
		fence = &protocol.ReleaseFence{BinID: bin, Epoch: epoch, AsOfSeq: seq, AsOfNet: net + int64(p.Bin(bin, epoch)), AsOfAt: clock.Now().UTC()}
		return nil
	})
	e.countMu.Unlock()
	switch {
	case err != nil && fence == nil:
		return unfenced(fmt.Sprintf("the statement failed: %v", err), true)
	case err != nil:
		log.Printf("release: bin %d epoch %d at node %s declared %d (%s) — unfenced: the accumulator's lock: %v", bin, epoch, node.CoreNodeName, written, source, err)
		return nil, written
	case !found:
		return unfenced("the seat has no runtime row", false)
	}
	log.Printf("release: bin %d epoch %d at node %s declared %d (%s) as of seq %d net %d — fenced",
		bin, epoch, node.CoreNodeName, written, source, fence.AsOfSeq, fence.AsOfNet)
	return fence, written
}

// declareReleaseCount writes a release's count on the seat with its fence
// (writeReleaseCount) and returns the count written, for the envelope's
// RemainingUOP. A count from the seat goes on the wire disposition too, as its
// Count, and as its CountSuggested when the screen sent a pre-fill: the
// declaration is Shingo's own number, not a person's override of it, and a
// suggestion left at the pre-fill beside a count the line has since drawn
// down would make Core's AuditReleaseOverride write an override row for every
// release. With no suggestion sent, none goes (Core audits nothing). A typed
// count keeps the screen's pre-fill as its suggestion, and Core audits the
// difference as before.
//
// A cart drawn below 0 declares what it reads, on the seat, in the release's
// log line and on the wire (decision 55): Core writes 0 for it and records the
// overpack (released_overpack, the declared count in the row). A Core from
// before that refuses the negative, as it did, with manifest_sync_failed.
func (e *Engine) declareReleaseCount(node *processes.Node, rt *processes.RuntimeState, binID int64, count int, disp ReleaseDisposition, wire *protocol.UOPDisposition) (*int, *protocol.ReleaseFence) {
	fromSeat := disp.CountFromSeat()
	fence, written := e.writeReleaseCount(node, rt, binID, count, fromSeat)
	if fromSeat && wire != nil && wire.Kind == protocol.DispositionReleasePartial {
		wire.Count = written
		if wire.CountSuggested != nil {
			suggested := written
			wire.CountSuggested = &suggested
		}
	}
	return &written, fence
}
