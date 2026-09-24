package messaging

import (
	"log"

	"shingo/protocol"
	"shingocore/store"
)

// HandleLinesideLevelReport persists the Edge's periodic per-seat lineside
// report and compares it against Core's replica. Edge → Core,
// SubjectLinesideLevelReport.
//
// THE REPORT IS A CHECKSUM. It triggers no evaluation and moves no decision:
// every replenishment decision reads Core's count (seat-count round 1 §5). The
// comparison (service.LinesideDivergenceService) opens and closes
// report_divergence episodes; nothing here writes bins.uop_remaining.
//
// It upserts one edge_lineside_reports row per entry keyed by
// (station, node, payload), latest-wins on reported_at, and compares only when
// at least one row MOVED. The inbox dedup gates order-channel envelopes, not
// TypeData, so a redelivered report arrives here again (at Springfield on
// 2026-08-20/21 envelope ids were redelivered 2-4x each), and a requeued dead
// letter can deliver an hour-old report after a current one. Neither moves a
// row, and neither may open or close an episode on stale values. A newer
// report moves every row it carries (reported_at advances), so a current
// report is always compared.
//
// An entry whose upsert failed is left out of the comparison as well.
//
// Cost per message: one upsert per well-formed entry, then — for a report that
// moved a row — two reads (Core's side, the station's open episodes) and a
// write only when an episode opens or closes.
func (s *CoreDataService) HandleLinesideLevelReport(env *protocol.Envelope, r *protocol.LinesideLevelReport) {
	station := r.Station
	if station == "" {
		station = env.Src.Station
	}
	if station == "" || len(r.Entries) == 0 {
		return
	}

	moved := false
	stored := make([]protocol.LinesideLevelEntry, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.CoreNodeName == "" || e.PayloadCode == "" {
			continue
		}
		m, err := s.db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: e.CoreNodeName,
			PayloadCode:  e.PayloadCode,
			BinCount:     e.BinCount,
			BinUOP:       e.BinUOP,
			BucketQty:    e.BucketQty,
			BinID:        e.BinID,
			BinEpoch:     e.BinEpoch,
			FlushedSeq:   e.FlushedSeq,
			ReportedAt:   r.ReportedAt,
		})
		if err != nil {
			log.Printf("core_handler: upsert lineside report station=%s node=%s payload=%s: %v",
				station, e.CoreNodeName, e.PayloadCode, err)
			continue
		}
		moved = moved || m
		stored = append(stored, e)
	}

	s.resp.dbg("lineside_level_report station=%s entries=%d moved=%t", station, len(r.Entries), moved)
	if !moved || s.linesideDivergence == nil {
		return
	}
	if _, _, err := s.linesideDivergence.CheckReport(station, stored); err != nil {
		log.Printf("core_handler: lineside report comparison station=%s: %v (no episode opened or closed)", station, err)
	}
}
