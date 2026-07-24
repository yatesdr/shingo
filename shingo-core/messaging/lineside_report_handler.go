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
// reported payloads. R1 is LIVE: in edge_reports mode the fresh report can fire
// replenishment off the edge-adjusted total; in ledger mode it stays audit-only.
// Either way it logs the ledger-vs-edge disagreement audit line, and nothing here
// writes bins.uop_remaining (its own table, edge_lineside_reports).
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
		if err := s.db.UpsertEdgeLinesideReport(store.EdgeLinesideReport{
			Station:      station,
			CoreNodeName: e.CoreNodeName,
			PayloadCode:  e.PayloadCode,
			BinCount:     e.BinCount,
			BinUOP:       e.BinUOP,
			BucketQty:    e.BucketQty,
			ReportedAt:   r.ReportedAt,
		}); err != nil {
			log.Printf("core_handler: upsert lineside report station=%s node=%s payload=%s: %v",
				station, e.CoreNodeName, e.PayloadCode, err)
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
