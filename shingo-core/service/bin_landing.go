package service

import (
	"database/sql"
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingocore/store"
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
// Where it goes (H.3, D9), never broadcast:
//   - a place no station binds at (p.NotASeat: synthetic, STOR, a lane or
//     group slot): not sent;
//   - else the stations whose processes claim the node in the mirror (its own
//     position or a paired one): exactly one → to it;
//   - none (or several, none of them the order's) and the order has a station
//     → the order's station (materialStation's spelling);
//   - none at all → not sent.
//
// What it is (H.7): a station whose registered schema can bind a landing
// (protocol.EdgeSchemaBinLanded) gets the landing. An older one gets today's
// wire: the Bound this writer used to broadcast (legacyBound), now addressed
// to that station, or nothing for a writer that sent none (a final drop's
// carrier still rides order.delivered's bind fields for that Edge).
//
// Cost: one statement (the address read, which carries both schema versions)
// and one outbox row, only at a node a station may bind at. The count, the
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
	st, ok := landingStation(addr)
	if !ok {
		return nil
	}
	if st.SchemaVersion >= protocol.EdgeSchemaBinLanded {
		if err := s.manifest.announce.SendTo(tx, protocol.SubjectBinLanded, st.StationID, &protocol.BinLanded{
			BinID:        p.BinID,
			CoreNodeName: p.NodeName,
			Count:        p.Count,
			Epoch:        p.Epoch,
			PayloadCode:  p.PayloadCode,
			PayloadKnown: p.PayloadCode != "",
			OrderUUID:    addr.OrderUUID,
			PlacedAt:     clock.Now().UTC(),
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

// landingStation picks the one station a landing goes to (H.3), or false.
func landingStation(addr plantclaims.LandingAddress) (plantclaims.AddressedStation, bool) {
	if len(addr.Claiming) == 1 {
		return addr.Claiming[0], true
	}
	if addr.Order.StationID == "" {
		return plantclaims.AddressedStation{}, false
	}
	if len(addr.Claiming) > 1 {
		// No node is claimed by two stations on any fixture (DESIGN-H §4). If
		// one is, the order's station decides only when it is one of them;
		// otherwise nobody is told rather than a guess.
		for _, c := range addr.Claiming {
			if c.StationID == addr.Order.StationID {
				return c, true
			}
		}
		return plantclaims.AddressedStation{}, false
	}
	return addr.Order, true
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
