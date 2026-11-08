package service

import (
	"database/sql"
	"fmt"
	"log"
	"time"

	"shingo/protocol"

	"shingocore/store/bins"
	"shingocore/store/inventory"
)

// AnnounceLateDrawTx tells the station holding a bin that a late draw changed
// its count (uop.ManifestClearer): a window of the generation its last seat
// lift ended, applied after the cart has landed at another seat (the
// applier's E-1 arm). That seat's Edge bound the cart at the count its landing
// carried, before the draw, and would otherwise hold a count above Core's for
// the cart's whole stay.
//
// The announcement is the count's own (announceCount's shape): Core's count
// after the draw at the bin's current generation, fenced at that generation's
// cursor so the station adds back what it has counted since and Core has not
// applied (engine.fencedCount). With no cursor at the generation yet (the
// seat's first windows are still in flight) the fence is net 0 at seq 0 for
// each station addressed: Core has applied none of them. With two stations
// counting it there is no fence, and a lifecycle announcement without one is
// news the station already holds (it keeps its count), as before.
//
// Addressed as every lifecycle announcement is (announceLifecycle's rule), and
// only when the bin stands at a seat: a bin in the air or in storage has no
// station counting it, and its next landing carries the count. Reports whether
// it stands at a seat, so the caller pushes the bins feed after its commit.
// Cost: the address and the cursor, two statements, on a late apply only.
func (s *BinManifestService) AnnounceLateDrawTx(tx *sql.Tx, binID, epoch int64, count int) (bool, error) {
	dest, address := bins.ScanLifecycleAddress()
	var nodeName string
	if err := tx.QueryRow(`SELECT COALESCE((SELECT n.name FROM nodes n WHERE n.id = bins.node_id), ''), `+
		bins.LifecycleAddressSQL+` FROM bins WHERE id=$1`, binID).Scan(append([]any{&nodeName}, dest...)...); err != nil {
		return false, fmt.Errorf("late draw on bin %d: read its address: %w", binID, err)
	}
	addr := address()
	if nodeName == "" || addr.NotASeat {
		return false, nil
	}
	if !s.announce.Wired() {
		log.Printf("bin_manifest: a late draw on bin %d at %s with no announce topic wired — "+
			"the station keeps the count its landing carried", binID, nodeName)
		return true, nil
	}
	cursorStation, cur, cursors, err := inventory.SoleBinCursor(tx, binID, epoch)
	if err != nil {
		return false, fmt.Errorf("late draw on bin %d: %w", binID, err)
	}
	sts, _ := lifecycleStations(addr)
	for _, st := range sts {
		adj := &protocol.UOPAdjustment{
			BinID:        binID,
			CoreNodeName: nodeName,
			NewRemaining: count,
			Epoch:        epoch,
			Actor:        protocol.ActorCoreLifecycle,
			AdjustedAt:   time.Now().UTC(),
		}
		switch {
		case cursors == 0:
			net, seq := int64(0), int64(0)
			adj.AsOfNet, adj.AsOfSeq, adj.AsOfStation = &net, &seq, st.StationID
		case cursors == 1 && cur.AppliedNet != nil:
			seq := cur.LastSeq
			adj.AsOfNet, adj.AsOfSeq, adj.AsOfStation = cur.AppliedNet, &seq, cursorStation
		}
		if err := s.announce.SendTo(tx, protocol.SubjectUOPAdjustment, st.StationID, adj); err != nil {
			return false, fmt.Errorf("announce the late draw on bin %d: %w", binID, err)
		}
	}
	log.Printf("bin_manifest: a late draw on bin %d applied after it landed at %s: count %d at epoch %d sent to %d station(s)",
		binID, nodeName, count, epoch, len(sts))
	return true, nil
}
