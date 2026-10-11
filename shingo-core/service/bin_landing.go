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
// Where it goes (H.3, D9; the last arm by RULINGS-C2c-run-through §4):
//   - a place no station binds at (p.NotASeat: synthetic, STOR, a lane or
//     group slot): not sent;
//   - else the stations whose processes claim the node in the mirror (its own
//     position or a paired one): exactly one → to it;
//   - several, and the order's station is one of them → to it; several and
//     it is not → not sent (a guess binds a carrier at the wrong Edge);
//   - none, and the order has a station → the order's station
//     (materialStation's spelling);
//   - none, and no order station (an admin move onto a loader or FG seat: the
//     Edge's publisher skips manual_swap claims, so the mirror never names
//     them) → THE LAST ARM: the plant's only Edge, or every Edge when there
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
