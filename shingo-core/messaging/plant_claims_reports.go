package messaging

import (
	"fmt"
	"log"
	"strings"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// plant_claims_reports.go — the plant-claims direction of the feed digests
// (Edge → Core). Core keeps, per station and process, the digest of the last
// report that station sent (plant_claims_reports), and quotes each station its
// own digests back on its heartbeat ack. The Edge re-sends what differs; Core
// never recomputes a digest from its mirror.

// recordClaimsReport writes a report's row after its mirror replace succeeded.
// A failed write is logged and left: the row then quotes an older digest (or
// none), and the Edge re-sends on its next heartbeat, which retries the write.
//
// The row is keyed by station and process while the mirror is keyed by process
// alone. So a second station reporting a process takes the mirror over (last
// writer wins, HandlePlantClaims), and each station keeps its own row: each is
// quoted its own digest, and neither re-sends what it already sent
// (TestScenario_TwoStationsOneProcess_EachSendsOnce). An empty report replaced
// the process's mirror whole, so it deletes every station's row for it: a
// station that still has the process is no longer quoted it, and re-sends.
func (s *CoreDataService) recordClaimsReport(env *protocol.Envelope, report *protocol.PlantClaimsReport) {
	station := ""
	if env != nil { // a message off the wire always has one; some tests pass nil
		station = env.Src.Station
	}
	if len(report.Styles) == 0 {
		gone, err := s.db.DeletePlantClaimsReport(report.ProcessID)
		if err != nil {
			log.Printf("core_handler: plant.claims report row %s: delete: %v", report.ProcessID, err)
			return
		}
		if len(gone) > 0 && (len(gone) > 1 || gone[0] != station) {
			log.Printf("core_handler: plant.claims %s emptied by %s; the report rows of %s go with the mirror, and a station that still has it re-sends it",
				report.ProcessID, station, strings.Join(gone, ", "))
		}
		return
	}
	if err := s.db.RecordPlantClaimsReport(report.ProcessID, station, report.Digest, clock.Now().UTC()); err != nil {
		log.Printf("core_handler: plant.claims report row %s: %v", report.ProcessID, err)
	}
}

// claimsFlags is a station's Edges-page flag for each process it and another
// station both report, read from the report rows: it stands while both hold a
// row and goes when one stops (its empty report deletes the process's rows).
// Read, not kept in memory, so a Core restart does not lose it: neither station
// re-sends a report Core quotes back to it, so nothing would raise it again. A
// failed read is a flag of its own, never the absence of one.
func (s *CoreDataService) claimsFlags(station string) []string {
	shared, err := s.db.SharedPlantClaimsReports(station)
	if err != nil {
		log.Printf("core_feeds: plant claims shared by %s: %v", station, err)
		return []string{"plant claims: could not read which processes another station also reports"}
	}
	out := make([]string, 0, len(shared))
	for process, stations := range shared {
		who := strings.Join(stations[:len(stations)-1], ", ") + " and " + stations[len(stations)-1]
		if len(stations) == 2 {
			who = "both " + who
		}
		out = append(out, fmt.Sprintf("process %s reported by %s", process, who))
	}
	return out
}

// claimsFor is the heartbeat ack's Claims: process → digest for every report
// row station (the heartbeat's sender) holds, and every mirrored process with
// no report row at "" (plantclaims.ReportDigests). nil for an Edge that does
// not speak feeds (it would not read them) and for one that quotes the owned
// map (answered there, heldClaims, from the same read), and nil when the read
// fails, so the Edge sends nothing on a read Core could not make; {} when Core
// holds none.
func (s *CoreDataService) claimsFor(station string, p *protocol.EdgeHeartbeat) map[string]string {
	if p.Feeds == nil || p.Owned != nil {
		return nil
	}
	claims, err := s.db.PlantClaimsReportDigests(station)
	if err != nil {
		log.Printf("core_feeds: plant claims for %s: %v — claims left off the ack", station, err)
		return nil
	}
	return claims
}
