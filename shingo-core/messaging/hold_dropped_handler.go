package messaging

import (
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// HandleHoldDropped records an Edge's dropped produce hold. Edge → Core,
// SubjectHoldDropped.
//
// One hold_dropped row on bin_uop_exception per envelope, with no bin (a hold
// has no carrier), the held part, the dropper as actor and the reason as op;
// the seat, quantity, reason, station and named order are in detail. The
// /inventory list reads it beside the declared-full stamps.
//
// AN INCOMPLETE MESSAGE IS STILL WRITTEN. The row is the only record that the
// parts left the count, so a message missing its seat or its station is kept
// as it came; one with no occurred_at takes Core's clock.
//
// A REDELIVERY WRITES NOTHING. TypeData passes no inbox dedup, and both the
// Edge's outbox and Kafka re-send an envelope with its id unchanged, so the row
// is keyed on the envelope id (audit.AppendHoldDropped).
//
// Cost per message: one statement (an INSERT guarded by the key's NOT EXISTS).
func (s *CoreDataService) HandleHoldDropped(env *protocol.Envelope, d *protocol.HoldDropped) {
	at := d.OccurredAt
	if at.IsZero() {
		at = clock.Now().UTC()
	}
	wrote, err := s.db.RecordHoldDropped(env.ID, env.Src.Station, d.Node, d.PayloadCode, d.Qty,
		d.Reason, d.CalledBy, d.OrderID, at)
	if err != nil {
		log.Printf("core_handler: hold dropped station=%s node=%s part=%s qty=%d reason=%s: %v",
			env.Src.Station, d.Node, d.PayloadCode, d.Qty, d.Reason, err)
		return
	}
	if !wrote {
		s.resp.dbg("hold dropped: envelope %s already recorded (redelivery)", env.ID)
	}
}
