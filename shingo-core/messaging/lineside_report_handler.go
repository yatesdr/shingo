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
// EVERY SEAT, EVERY INTERVAL (owner ruling, 2026-09-24). The Edge sends each
// consume seat it runs, an empty one as a row with nothing bound and no part,
// and sends a report with no rows when it runs none. A row with no part is
// stored and compared like any other. A report with no rows moves no row, so
// latest-wins cannot tell a current one from a late one; it is compared only
// when it is newer than every row the station has stored, and it then closes
// what the station's earlier reports found and no longer holds.
//
// Cost per message: one upsert per well-formed entry, then — for a report that
// moved a row — two reads (Core's side, the station's open episodes) and a
// write only when an episode opens or closes. A report with no rows costs one
// read (the station's latest row) instead of the upserts.
func (s *CoreDataService) HandleLinesideLevelReport(env *protocol.Envelope, r *protocol.LinesideLevelReport) {
	station := r.Station
	if station == "" {
		station = env.Src.Station
	}
	if station == "" {
		return
	}

	moved := false
	stored := make([]protocol.LinesideLevelEntry, 0, len(r.Entries))
	for _, e := range r.Entries {
		if e.CoreNodeName == "" {
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
	if len(r.Entries) == 0 {
		latest, ok, err := s.db.LatestLinesideReportAt(station)
		if err != nil {
			log.Printf("core_handler: lineside report with no rows station=%s: %v (not compared)", station, err)
			return
		}
		moved = !ok || r.ReportedAt.After(latest)
	}

	s.resp.dbg("lineside_level_report station=%s entries=%d moved=%t", station, len(r.Entries), moved)
	if !moved || s.linesideDivergence == nil {
		return
	}
	if _, _, err := s.linesideDivergence.CheckReport(station, stored); err != nil {
		log.Printf("core_handler: lineside report comparison station=%s: %v (no episode opened or closed)", station, err)
	}
}
