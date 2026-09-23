package messaging

import (
	"log"

	"shingo/protocol"
	"shingocore/store"
)

// HandleLinesideLevelReport persists Edge's periodic per-consuming-node lineside
// levels (the R1 read-model) and triggers the monitor's report-arrival path.
// Edge → Core, SubjectLinesideLevelReport.
//
// It upserts one edge_lineside_reports row per entry keyed by
// (station, node, payload), then asks the threshold monitor to evaluate the
// payloads with at least one row that MOVED. R1 is LIVE: in edge_reports mode
// the fresh report can fire replenishment off the edge-adjusted total; in
// ledger mode it stays audit-only. Either way it logs the ledger-vs-edge
// disagreement audit line, and nothing here writes bins.uop_remaining (its own
// table, edge_lineside_reports).
//
// Only moved rows count because the inbox dedup gates order-channel envelopes,
// not TypeData, so a redelivered report arrives here again — at Springfield on
// 2026-08-20/21 envelope ids were redelivered 2-4x each, and each one ran the
// fire gate for every payload in it. The upsert's latest-wins condition already
// turns a duplicate or an older report into a no-op on the row; a row that did
// not move has nothing new for the monitor to decide on. A newer report with
// identical values still moves the row (reported_at advances), so it still
// evaluates, and that is what keeps the node inside the staleness window.
func (s *CoreDataService) HandleLinesideLevelReport(env *protocol.Envelope, r *protocol.LinesideLevelReport) {
	station := r.Station
	if station == "" {
		station = env.Src.Station
	}
	if station == "" || len(r.Entries) == 0 {
		return
	}

	payloads := make([]string, 0, len(r.Entries))
	seen := make(map[string]struct{}, len(r.Entries))
	for _, e := range r.Entries {
		if e.CoreNodeName == "" || e.PayloadCode == "" {
			continue
		}
		moved, err := s.db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: e.CoreNodeName,
			PayloadCode:  e.PayloadCode,
			BinCount:     e.BinCount,
			BinUOP:       e.BinUOP,
			BucketQty:    e.BucketQty,
			ReportedAt:   r.ReportedAt,
		})
		if err != nil {
			log.Printf("core_handler: upsert lineside report station=%s node=%s payload=%s: %v",
				station, e.CoreNodeName, e.PayloadCode, err)
			continue
		}
		if !moved {
			continue
		}
		if _, dup := seen[e.PayloadCode]; !dup {
			seen[e.PayloadCode] = struct{}{}
			payloads = append(payloads, e.PayloadCode)
		}
	}

	s.resp.dbg("lineside_level_report station=%s entries=%d payloads=%d", station, len(r.Entries), len(payloads))
	if s.thresholdMonitor != nil && len(payloads) > 0 {
		s.thresholdMonitor.OnLinesideReports(payloads)
	}
}
