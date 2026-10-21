package service

import (
	"database/sql"
	"errors"
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/orders"
	"shingocore/store/plantclaims"
)

// bin_landing.go — the landing (Lane H, H.1–H.3, H.7; DECISIONS D9, D10).
//
// ONE VERB announces that a carrier came to rest at a node a station binds at,
// and every Core writer that puts a bin at a node either calls it, in the
// transaction that moves the bin, or is named with its reason in
// TestArch_EveryArrivalWriterAnnouncesOrIsNamed (bin_landing_arch_test.go).
//
// Core does not change WHEN it moves a bin. It used to tell the Edge in three
// spellings, none of them in the move's transaction: order.delivered after the
// whole order FINISHED (which also bound), a broadcast Bound after a mid-order
// drop, and a broadcast Bound after an admin move; the completion safety nets,
// the repair, the inferred placement and the recovery told nobody. A drop the
// Edge never heard of left the seat empty with a carrier on it, and a bind that
// arrived after ticks wrote Core's arrival count over them (DESIGN-H §1, §3).

// announceLanding writes the landing for placement p, made by orderID (0 when
// no order made it), on tx — the transaction that moved the bin, so the row
// commits with the move or not at all (H.2; the bumpEpoch pattern).
//
// Where it goes (H.3, D9; the last arm by RULINGS-C2c-run-through §4):
//   - a place no station binds at (p.NotASeat: synthetic, STOR, a lane or
//     group slot): not sent;
//   - else the stations whose processes claim the node in the mirror (its own
//     position or a paired one, or a position the process publishes: a loader
//     window or home, an FG drain; plantclaims.ClaimingStationsSQL): exactly
//     one → to it;
//   - several, and the order's station is one of them → to it; several and
//     it is not → not sent (a guess binds a carrier at the wrong Edge);
//   - none, and the order has a station → the order's station
//     (materialStation's spelling);
//   - none, and no order station (an admin move onto a seat no Edge publishes;
//     loader and FG seats are published as positions, so about never) → THE
//     LAST ARM: the plant's only Edge, or every Edge when there
//     are several, each addressed. An Edge that does not model the node drops
//     it (HandleBinLanded). Expected about 0 a run; logged per landing
//     ("bin_landing: last arm") and counted in soakstat [S.4].
//
// The brief said "never broadcast", and that left this seat with no door: the
// admin Move's broadcast Bound used to reach it, and order.delivered no
// longer binds (DECISIONS D19's reach gap; the orc's error, ruled 2026-10-10).
// A wasted message is cheaper than a seat that never binds.
//
// What it is (H.7), per Edge: a station whose registered schema can bind a
// landing (protocol.EdgeSchemaBinLanded) gets the landing. An older one gets
// today's wire: the Bound this writer used to broadcast (legacyBound), now
// addressed to that station, or nothing for a writer that sent none (a final
// drop's carrier still rides order.delivered's bind fields for that Edge).
//
// Cost: one statement (the address read, which carries both schema versions
// and the last arm's Edges) and one outbox row per Edge addressed, only at a
// node a station may bind at. The count, the
// generation, the part and the placement stamp come back from the move's own
// UPDATE (helpers.ArrivalReturningSQL), +0.
func (s *BinService) announceLanding(tx *sql.Tx, p store.Placed, orderID int64, legacyBound bool, actor string) error {
	if p.NotASeat || p.NodeName == "" {
		return nil
	}
	if s.manifest == nil || !s.manifest.announce.Wired() {
		log.Printf("bin_landing: bin %d placed at %s with no announce topic wired — "+
			"the station binding there is not told", p.BinID, p.NodeName)
		return nil
	}
	addr, err := s.db.ResolveLandingAddress(tx, p.NodeName, orderID)
	if err != nil {
		return fmt.Errorf("landing bin %d at %s: %w", p.BinID, p.NodeName, err)
	}
	sts := landingStations(addr)
	if len(addr.Plant) > 0 && len(sts) > 0 {
		log.Printf("bin_landing: last arm — bin %d at %s: no station claims it and no order station; "+
			"sent to %d Edge(s)", p.BinID, p.NodeName, len(sts))
	}
	for _, st := range sts {
		if err := s.sendLanding(tx, st, p, addr.OrderUUID, legacyBound, actor); err != nil {
			return err
		}
	}
	return nil
}

// sendLanding writes one Edge's landing under the version rule (H.7).
func (s *BinService) sendLanding(tx *sql.Tx, st plantclaims.AddressedStation, p store.Placed,
	orderUUID string, legacyBound bool, actor string) error {
	if st.SchemaVersion >= protocol.EdgeSchemaBinLanded {
		if err := s.manifest.announce.SendTo(tx, protocol.SubjectBinLanded, st.StationID, &protocol.BinLanded{
			BinID:        p.BinID,
			CoreNodeName: p.NodeName,
			Count:        p.Count,
			Epoch:        p.Epoch,
			PayloadCode:  p.PayloadCode,
			PayloadKnown: p.PayloadCode != "",
			OrderUUID:    orderUUID,
			PlacedSeq:    p.PlacedSeq,
		}); err != nil {
			return fmt.Errorf("landing bin %d at %s: %w", p.BinID, p.NodeName, err)
		}
		return nil
	}
	if !legacyBound {
		return nil
	}
	if err := s.manifest.announce.SendTo(tx, protocol.SubjectUOPAdjustment, st.StationID, &protocol.UOPAdjustment{
		BinID:        p.BinID,
		CoreNodeName: p.NodeName,
		NewRemaining: p.Count,
		Epoch:        p.Epoch,
		Bound:        true,
		Actor:        actor,
		AdjustedAt:   clock.Now().UTC(),
	}); err != nil {
		return fmt.Errorf("bound for bin %d at %s: %w", p.BinID, p.NodeName, err)
	}
	return nil
}

// landingStations picks the stations a landing goes to (H.3): one; or, on the
// last arm, every Edge (addr.Plant, which the read fills only when no station
// claims the node and the order has none); or none.
func landingStations(addr plantclaims.LandingAddress) []plantclaims.AddressedStation {
	if len(addr.Claiming) == 1 {
		return addr.Claiming[:1]
	}
	if addr.Order.StationID == "" {
		// Several claim it and no order station decides: nobody (a guess). None
		// claims it: the last arm, whose Edges the read already filled.
		if len(addr.Claiming) > 1 {
			return nil
		}
		return addr.Plant
	}
	if len(addr.Claiming) > 1 {
		// No node is claimed by two stations on any fixture (DESIGN-H §4). If
		// one is, the order's station decides only when it is one of them;
		// otherwise nobody is told rather than a guess.
		for _, c := range addr.Claiming {
			if c.StationID == addr.Order.StationID {
				return []plantclaims.AddressedStation{c}
			}
		}
		return nil
	}
	return []plantclaims.AddressedStation{addr.Order}
}

// announceLifecycle writes a lifecycle announcement (a generation bump's, a
// count's) on tx, addressed by the landing's rule for a placement with no
// order (Lane 0 item 5; RULINGS-C2c-certification §4.1): addr is what the
// bump's or count's own UPDATE read (bins.LifecycleAddressSQL), and the rule
// is landingStations, the landing's, so the two cannot drift.
//
//   - not a seat (storage, synthetic, a lane or group slot): not sent;
//   - exactly one station claims the node (a claim or a position): to it;
//   - several claim it: nobody (no order station to decide);
//   - none claims it: the last arm, every registered Edge, each addressed;
//     logged per announcement ("bin_landing: last arm") so soakstat [S.4]
//     counts it with the landings' last arm.
//
// Cost: +0 statements (the address rides the caller's UPDATE); one outbox row
// per station addressed. It used to be one broadcast row, parsed by every Edge
// and logged as "process node not found" by each that does not model the node.
func announceLifecycle(ann EpochAnnounce, tx *sql.Tx, addr bins.LifecycleAddress, what string, adj *protocol.UOPAdjustment) error {
	sts, lastArm := lifecycleStations(addr)
	if lastArm && len(sts) > 0 {
		log.Printf("bin_landing: last arm (%s) — bin %d at %s: no station claims it and no order; "+
			"sent to %d Edge(s)", what, adj.BinID, adj.CoreNodeName, len(sts))
	}
	for _, st := range sts {
		if err := ann.SendTo(tx, protocol.SubjectUOPAdjustment, st.StationID, adj); err != nil {
			return err
		}
	}
	return nil
}

// lifecycleStations is landingStations for a placement with no order: the
// stations addr names, and whether they are the last arm's.
func lifecycleStations(addr bins.LifecycleAddress) ([]plantclaims.AddressedStation, bool) {
	if addr.NotASeat {
		return nil, false
	}
	la := plantclaims.LandingAddress{Claiming: addressed(addr.Claiming)}
	if len(la.Claiming) == 0 {
		la.Plant = addressed(addr.Plant)
	}
	return landingStations(la), len(la.Claiming) == 0
}

func addressed(stations []string) []plantclaims.AddressedStation {
	out := make([]plantclaims.AddressedStation, 0, len(stations))
	for _, st := range stations {
		out = append(out, plantclaims.AddressedStation{StationID: st})
	}
	return out
}

// LandingHook is the landing verb as a placement hook for the store-level
// writers that own their transaction (store.ApplyMultiBinArrival,
// RepairConfirmedOrderCompletion): each calls it per bin in its transaction.
// legacyBound says whether that writer used to broadcast a Bound (none of the
// robot writers outside the mid-order drop did).
func (s *BinService) LandingHook(orderID int64, legacyBound bool) store.OnPlaced {
	return func(tx *sql.Tx, p store.Placed) error {
		return s.announceLanding(tx, p, orderID, legacyBound, "")
	}
}

// ApplyMultiBinArrival settles a multi-bin order's bins (store.ApplyMultiBinArrival)
// and announces each one that came to rest at a seat, in the settle's
// transaction (DESIGN-H §3: it used to announce one bin, by order.delivered,
// and the others were silent).
func (s *BinService) ApplyMultiBinArrival(placedByOrder int64, instructions []orders.BinArrivalInstruction) ([]int64, error) {
	return s.db.ApplyMultiBinArrival(placedByOrder, instructions, s.LandingHook(placedByOrder, false))
}

// ErrReseatUnknownNode is a reseat asked of a name Core has no node for: not an
// empty node (an unknown name is never an empty), so nothing is announced and
// the asking station keeps its seat as it is.
var ErrReseatUnknownNode = errors.New("no node by that name")

// Reseat re-announces, to station alone, the carrier standing at node through
// the landing verb (protocol.AskReseat): a transit.bin_landed carrying the
// carrier's count, generation and part as Core holds them, and a fresh
// placement stamp taken from bin_placement_seq, written in one transaction
// with the read so the announcement cannot carry a mismatched pair. landed is
// false when no carrier stands there; then nothing is sent. node is the name
// as the station asked it (a dot name for a group's child), and the landing
// carries that name, the one the station binds by.
//
// THE BIND IS THE STATION'S, ON THE LANDING. Core binds nothing; the seat's
// placed_seq guard takes a stamp newer than any it holds, and the same carrier
// at an equal or newer generation keeps the seat's own count
// (processes.BindLanding), so a repeat binds nothing new. The resident carrier
// is the one the bins table names: the newest non-retired bin at the node.
//
// The asking station speaks the ask pair, a later schema than the landing's
// (protocol.EdgeSchemaBinLanded), so it gets the landing itself, never the
// older Bound.
//
// Cost: the node read, then one transaction: the bin read with the stamp, and
// one outbox row when a carrier stands there.
func (s *BinService) Reseat(station, node string) (landed bool, err error) {
	n, err := s.db.GetNodeByDotName(node)
	if err != nil || n == nil {
		return false, fmt.Errorf("reseat %s: %w", node, ErrReseatUnknownNode)
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, fmt.Errorf("reseat %s: %w", node, err)
	}
	defer func() { _ = tx.Rollback() }()
	p := store.Placed{NodeName: node}
	err = tx.QueryRow(`SELECT b.id, nextval('bin_placement_seq'), b.uop_remaining, b.delta_epoch, b.payload_code
		FROM bins b WHERE b.node_id = $1 AND b.status != 'retired' ORDER BY b.id DESC LIMIT 1`, n.ID).
		Scan(&p.BinID, &p.PlacedSeq, &p.Count, &p.Epoch, &p.PayloadCode)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("reseat %s: read the carrier: %w", node, err)
	}
	if s.manifest == nil || !s.manifest.announce.Wired() {
		return false, fmt.Errorf("reseat %s: no announce topic wired, the carrier cannot be announced", node)
	}
	st := plantclaims.AddressedStation{StationID: station, SchemaVersion: protocol.EdgeSchemaBinLanded}
	if err := s.sendLanding(tx, st, p, "", false, ""); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("reseat %s: %w", node, err)
	}
	return true, nil
}
