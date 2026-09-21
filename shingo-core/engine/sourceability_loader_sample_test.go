//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/store"
	"shingocore/store/demands"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// sourceability_loader_sample_test.go — the Q3 finding, as a test.
//
// MEASURED AT SPRINGFIELD, 2026-09-21: 22 monitored demand_registry bindings,
// and NOT ONE of them at a node any style claim names for the same payload.
// Samples were written per style_claims row of the running style, so the
// score's threshold arm had nothing to join — against a history of 2,931
// threshold episodes to 937 cell ones. Three quarters of the plant's demand was
// unscoreable, and would have stayed unscoreable for the whole collection
// window, because no forecast was ever recorded about the places it came from.
//
// The fixture below is that shape: a binding at a loader node, and a claim
// somewhere else entirely.

// seedRateFor gives the payload a plant-wide consumption rate by writing one
// consume tick into the ledger the rate scan reads. Without a rate there is no
// projection, and without a projection there is nothing for the score to grade
// — Known is false on no-rate exactly as it is for a cell line.
func seedRateFor(t *testing.T, db *store.DB, binID int64, payload string, consumed int) {
	t.Helper()
	_, err := db.DB.Exec(`INSERT INTO bin_uop_ledger
		(bin_id, before_uop, after_uop, op, payload_code, reason, applied_at)
		VALUES ($1, $2, 0, 'bin_uop_delta', $3, 'consume_tick', NOW() - INTERVAL '1 minute')`,
		binID, consumed, payload)
	testutil.MustNoErr(t, err, "seed consumption tick")
}

// TestLoaderSample_ThresholdEpisodeAtALoaderNodeIsScoreable is the whole point
// of B8: a threshold episode at a monitored binding's node finds a forecast.
//
// RED AT BASE — HasSample false, because the only samples were cell samples and
// none of them stood at that node.
func TestLoaderSample_ThresholdEpisodeAtALoaderNodeIsScoreable(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)

	const payload = "BIN-LS"
	const loaderNode = "LOADER-LS" // deliberately NOT the claim's node

	bin := createTestBinAtNode(t, db, payload, storageNode.ID, "src")
	seedRateFor(t, db, bin.ID, payload, 60)

	// A claim, at a different node. This is what made the arm empty at SPR: the
	// samples land where the claims are, and the bindings are somewhere else.
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNFL",
		[]plantclaims.StyleRow{{ProcessID: "SNFL", StyleID: "A"}},
		[]plantclaims.ClaimRow{{ProcessID: "SNFL", StyleID: "A",
			CoreNodeName: storageNode.Name, PayloadCode: payload}},
		0), "seed mirror")

	_, err := db.SyncDemandRegistry("ST-LS", []demands.RegistryEntry{{
		StationID: "ST-LS", CoreNodeName: loaderNode, Role: protocol.ClaimRoleConsume,
		PayloadCode: payload, ReplenishUOPThreshold: 5,
	}})
	testutil.MustNoErr(t, err, "seed monitored binding")

	eng := newUnstartedEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()
	m.publishFn = nil

	m.recomputeAll()

	// The episode opens AFTER the pass that forecast it. ScoreTTE takes the last
	// sample strictly before the episode, which is the rule that stops a
	// forecast being graded on hindsight.
	opened := time.Now().UTC().Add(1 * time.Minute)
	_, err = db.DB.Exec(`INSERT INTO demand_origins
		(origin_id, episode_key, kind, trigger_kind, process_id, core_node_name, payload_code, opened_at)
		VALUES ('77777777-7777-7777-7777-777777777777', $1, 'threshold', 'autoreorder', '', $2, $3, $4)`,
		"thr|"+loaderNode+"|"+payload, loaderNode, payload, opened)
	testutil.MustNoErr(t, err, "open threshold episode")

	scores, err := sourceability.ScoreTTE(db.DB, time.Now().UTC().Add(-time.Hour))
	testutil.MustNoErr(t, err, "score tte")

	var found bool
	for _, s := range scores {
		if s.EpisodeKey != "thr|"+loaderNode+"|"+payload {
			continue
		}
		found = true
		if !s.HasSample {
			t.Fatal("a threshold episode at a monitored binding's node has no forecast — " +
				"the arm is empty, which is the Springfield finding this exists to close")
		}
	}
	if !found {
		t.Fatalf("the threshold episode did not come back from ScoreTTE at all: %+v", scores)
	}
}

// TestLoaderSample_DebouncedVerdictSurvivesAnUnreadableActiveStyle is item 3.
//
// RED AT BASE — B6 put ActiveStyles inside BuildInputs, which the 300 ms
// debounce also calls, so a read error there took out the DEBOUNCED VERDICT:
// the path that tells an Edge what it can source, failing for a map only the
// two-minute pass's samples consume.
//
// THE SEAM IS THE COLUMN, NOT THE TABLE. loadStylesAndClaims selects
// process_id and style_id FROM process_styles, so dropping the table would fail
// BuildInputs either way and prove nothing about where ActiveStyles is read.
// Dropping is_active leaves the table readable and breaks precisely the one
// query that moved.
func TestLoaderSample_DebouncedVerdictSurvivesAnUnreadableActiveStyle(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	storageNode, _, _ := setupTestData(t, db)
	createTestBinAtNode(t, db, "BIN-DV", storageNode.ID, "src")

	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SNFD",
		[]plantclaims.StyleRow{{ProcessID: "SNFD", StyleID: "A"}},
		[]plantclaims.ClaimRow{{ProcessID: "SNFD", StyleID: "A",
			CoreNodeName: storageNode.Name, PayloadCode: "BIN-DV"}},
		0), "seed mirror")

	eng := newUnstartedEngine(t, db, simulator.New())
	m := eng.SourceabilityMonitor()

	var published int
	m.publishFn = func(protocol.SourcingStateReport) { published++ }

	_, err := db.DB.Exec(`ALTER TABLE process_styles DROP COLUMN is_active`)
	testutil.MustNoErr(t, err, "drop is_active")

	m.recomputeKeys([]plantclaims.ProcessKey{{ProcessID: "SNFD", StyleID: "A"}})

	if published == 0 {
		t.Fatal("the debounced path published no verdict with is_active unreadable — " +
			"a sample's input is failing the answer an Edge sources on")
	}
}
