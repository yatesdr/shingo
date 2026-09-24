package engine

import (
	"log"
	"strconv"

	"shingo/protocol"
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
// Cost: one SELECT (the flushed net) on top of the write the absolute path
// already does. The write is one statement, as before.
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
		ok, err = e.db.SetProcessNodeCountFenced(nodeID, adj.BinID, adj.Epoch, remaining, *adj.AsOfSeq)
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
