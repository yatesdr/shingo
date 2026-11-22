package messaging

import (
	"fmt"
	"log"
	"strings"
)

// node_claims.go — what Core says of a node two stations claim (owner ruling
// 2026-11-22: no refusal, no arbitration).

// nodeFlags is a station's Edges-page flag for each physical node it and
// another station both name, where at least one side is a claim: one line a
// node, naming it and every station that names it, the same on each of their
// rows. Read from the claims mirror, so it goes with the next report that no
// longer names the node and a Core restart does not lose it. A failed read is
// a flag of its own, never the absence of one.
func (s *CoreDataService) nodeFlags(station string) []string {
	shared, err := s.db.SharedClaimedNodes(station)
	if err != nil {
		log.Printf("core_feeds: nodes shared by %s: %v", station, err)
		return []string{"plant claims: could not read which nodes another station also claims"}
	}
	out := make([]string, 0, len(shared))
	for _, n := range shared {
		last := len(n.Stations) - 1
		out = append(out, fmt.Sprintf("node %s claimed by %s and %s", n.Node, strings.Join(n.Stations[:last], ", "), n.Stations[last]))
	}
	return out
}
