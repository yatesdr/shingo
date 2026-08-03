package engine

import (
	"testing"

	"shingo/protocol"
)

// The operator's part-request button is the plainest demand the plant has, and
// it opened no demand episode at all. The order went to Core carrying no
// origin, Core classified it — correctly, given what arrived — as an orphan,
// and the whole category of ordinary cell demand was therefore absent from the
// episode surface rather than mislabelled on it. Springfield ran for months
// with only changeover and threshold episodes visible, which is exactly what an
// operator would report if this path were silent.
//
// openCellEpisode was already written for this door. Its own doc names "an
// operator push" as the common case for JOINING and EpisodeTriggerOperator is
// documented as "the HMI button" — the machinery existed and nothing called it.
func TestRequestEmptyBin_OpensACellEpisodeForTheOperator(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "EPIS", "LOADER-EP", protocol.ClaimRoleProduce, []string{"P1"}, 2, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LOADER-EP", "produce", "threshold", "P1", 0, 100))

	if _, err := eng.RequestEmptyBin(nodeID, "P1"); err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}

	var kinds int
	if err := db.DB.QueryRow(
		`SELECT count(*) FROM demand_origins_open WHERE kind = ?`, protocol.EpisodeKindCell,
	).Scan(&kinds); err != nil {
		t.Fatalf("read open episodes: %v", err)
	}
	if kinds == 0 {
		t.Errorf("no cell episode after an operator part request — the order goes to Core with "+
			"no origin and lands as an orphan, and ordinary cell demand never appears on the "+
			"episode surface at all (kind=%s rows = %d)", protocol.EpisodeKindCell, kinds)
	}
}

// The trigger has to say it was a person. autoreorder and operator are separate
// values precisely so "the level fired" and "somebody pressed the button" are
// not the same row, and an episode opened by this path that claimed autoreorder
// would be worse than no episode.
func TestRequestEmptyBin_EpisodeRecordsTheOperatorTrigger(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID := seedCapManualSwap(t, db, "EPIS2", "LOADER-EP2", protocol.ClaimRoleProduce, []string{"P1"}, 2, false)
	seedCoreLoader(t, eng, sharedLoaderInfo("LOADER-EP2", "produce", "threshold", "P1", 0, 100))

	if _, err := eng.RequestEmptyBin(nodeID, "P1"); err != nil {
		t.Fatalf("RequestEmptyBin: %v", err)
	}

	var trigger string
	if err := db.DB.QueryRow(
		`SELECT COALESCE(trigger_kind,'') FROM demand_origins_open WHERE kind = ? LIMIT 1`,
		protocol.EpisodeKindCell,
	).Scan(&trigger); err != nil {
		t.Fatalf("read episode trigger: %v", err)
	}
	if trigger != protocol.EpisodeTriggerOperator {
		t.Errorf("trigger_kind = %q, want %q — an episode opened by the HMI button must say so, "+
			"or the operator's pull is indistinguishable from the level predicate firing",
			trigger, protocol.EpisodeTriggerOperator)
	}
}
