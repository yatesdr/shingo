package messaging

import (
	"log"

	"shingo/protocol"
)

// owned.go — Core's half of the Edge-owned map (protocol.AllOwned): THE RULE
// of protocol/feeds.go run the other way. The Edge owns its plant claims, its
// bins watch list and its cell catalog, and Core holds a copy of each. The
// Edge quotes one digest per family on its heartbeat (EdgeHeartbeat.Owned);
// Core answers every family it can read with the digest of what it holds and,
// only where that differs from the quote, the members themselves
// (EdgeHeartbeatAck.Owned); the Edge sends what differs (shingo-edge engine
// owned.go, answerOwned). Core keeps nothing for this beyond what it already
// holds: the claims report rows, the watch list in memory, the edge_cells
// rows, each read here as members.
//
// An Edge that sends no Owned is answered by the old fields instead
// (claimsFor, binWatchFor). HandleEdgeHeartbeat fills one or the other, never
// both (TestHeartbeatAck_TheOwnedMapPerFamily). Each family converging on two
// Edges, end to end: integration TestScenario_TwoEdges_EveryOwnedFamilyConverges.

// coreOwnedFamily is how Core reads what it holds of one family from one
// station: member name to the digest that arrived with the member.
// TestCoreOwned_EveryFamilyHasARow fails when a key of protocol.AllOwned has
// no row.
type coreOwnedFamily struct {
	key string
	// stored: the family is read from the database, so it is left off an ack
	// whose registry write failed, as Claims is: an answer read from a failing
	// database is not evidence enough for the Edge to send anything.
	stored bool
	held   func(s *CoreDataService, station string) (map[string]string, error)
}

// coreOwned has one row per Edge-owned family.
var coreOwned = [...]coreOwnedFamily{
	{key: protocol.OwnedClaims, stored: true, held: heldClaims},
	{key: protocol.OwnedBinWatch, held: heldBinWatch},
	{key: protocol.OwnedCells, stored: true, held: heldCells},
}

// answerOwned is the ack's Owned for a heartbeat that sent one: per family the
// heartbeat quoted and Core could read, the digest of what Core holds, and the
// members only where it differs from the quote. Never nil, so the Edge can
// tell this Core from one that does not speak the map. A family the heartbeat
// left off (the Edge could not read it), one Core does not serve, and one
// whose read failed are left off the answer, and the Edge sends nothing for
// them. stored false (the registry write failed) leaves off every family read
// from the database.
func (s *CoreDataService) answerOwned(station string, quoted map[string]string, stored bool) map[string]protocol.OwnedAnswer {
	out := make(map[string]protocol.OwnedAnswer, len(quoted))
	for _, f := range coreOwned {
		quote, named := quoted[f.key]
		if !named || (f.stored && !stored) {
			continue
		}
		members, err := f.held(s, station)
		if err != nil {
			log.Printf("core_feeds: owned %s for %s: %v — left off the ack", f.key, station, err)
			continue
		}
		digest, err := protocol.OwnedDigest(members)
		if err != nil {
			log.Printf("core_feeds: owned %s for %s: digest: %v — left off the ack", f.key, station, err)
			continue
		}
		a := protocol.OwnedAnswer{Digest: digest}
		if digest != quote {
			a.Members = members
		}
		out[f.key] = a
	}
	return out
}

// heldClaims is the claims Core holds from a station: per process, the digest
// of the last report it sent, and every mirrored process with no report row at
// "" (plantclaims.ReportDigests), the same read as the old field's (claimsFor).
func heldClaims(s *CoreDataService, station string) (map[string]string, error) {
	return s.db.PlantClaimsReportDigests(station)
}

// heldBinWatch is the watch list Core holds for a station, a family of one:
// no member when Core holds none or holds the empty list (the Edge's empty
// list is no member too, protocol.OwnedSingleton).
func heldBinWatch(s *CoreDataService, station string) (map[string]string, error) {
	names, digest, ok := s.bins.list(station)
	return protocol.OwnedSingleton(digest, !ok || len(names) == 0), nil
}

// heldCells is the cell catalog Core holds for a station, a family of one: the
// digest its live cells carry (EdgeCellsDigest), no member when none is live.
// Cells made live by a register carry no digest (HandleEdgeRegister), so they
// answer a member at "", which the Edge answers with its catalog.
func heldCells(s *CoreDataService, station string) (map[string]string, error) {
	digest, held, err := s.db.EdgeCellsDigest(station)
	if err != nil {
		return nil, err
	}
	return protocol.OwnedSingleton(digest, !held), nil
}
