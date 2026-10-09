package messaging

import (
	"fmt"
	"log"

	"shingo/protocol"
	"shingo/protocol/clock"
)

// plant_claims_reports.go — the plant-claims direction of the feed digests
// (Edge → Core). Core keeps, per process, the station whose report its mirror
// holds and the digest that report carried (plant_claims_reports), and quotes
// the digests back on that station's heartbeat ack. The Edge re-sends what
// differs; Core never recomputes a digest from its mirror.

// claimsFlagID names the Edges-page flag for a process two stations report.
func claimsFlagID(process string) string { return "claims:" + process }

// recordClaimsReport writes a report's row after its mirror replace succeeded.
// A failed write is logged and left: the row then quotes an older digest (or
// none), and the Edge re-sends on its next heartbeat, which retries the write.
//
// A process is keyed alone, as the mirror is, so a second station reporting it
// takes it over — last writer wins — and both stations' rows on the Edges page
// say so until the process is reported twice running by one station, or
// emptied.
func (s *CoreDataService) recordClaimsReport(env *protocol.Envelope, report *protocol.PlantClaimsReport) {
	station := ""
	if env != nil { // a message off the wire always has one; some tests pass nil
		station = env.Src.Station
	}
	id := claimsFlagID(report.ProcessID)
	if len(report.Styles) == 0 {
		prev, err := s.db.DeletePlantClaimsReport(report.ProcessID)
		if err != nil {
			log.Printf("core_handler: plant.claims report row %s: delete: %v", report.ProcessID, err)
			return
		}
		if prev != "" && prev != station {
			log.Printf("core_handler: plant.claims %s emptied by %s, last reported by %s", report.ProcessID, station, prev)
		}
		s.feeds.clearFlagEverywhere(id)
		return
	}
	prev, err := s.db.RecordPlantClaimsReport(report.ProcessID, station, report.Digest, clock.Now().UTC())
	if err != nil {
		log.Printf("core_handler: plant.claims report row %s: %v", report.ProcessID, err)
		return
	}
	if prev == "" || prev == station {
		s.feeds.clearFlagEverywhere(id)
		return
	}
	log.Printf("core_handler: plant.claims %s reported by %s, last by %s — the mirror now holds %s's (last writer wins)",
		report.ProcessID, station, prev, station)
	text := fmt.Sprintf("process %s reported by both %s and %s", report.ProcessID, prev, station)
	s.feeds.setFlag(prev, id, text)
	s.feeds.setFlag(station, id, text)
}

// claimsFor is the heartbeat ack's Claims: process → digest for every report
// row this station holds. nil for an Edge that does not speak feeds (it would
// not read them), and nil when the read fails, so the Edge sends nothing on a
// read Core could not make; {} when Core holds none.
func (s *CoreDataService) claimsFor(p *protocol.EdgeHeartbeat) map[string]string {
	if p.Feeds == nil {
		return nil
	}
	claims, err := s.db.PlantClaimsReportDigests(p.StationID)
	if err != nil {
		log.Printf("core_feeds: plant claims for %s: %v — claims left off the ack", p.StationID, err)
		return nil
	}
	return claims
}
