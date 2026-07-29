//go:build docker

package store_test

import (
	"testing"
	"time"

	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/plantclaims"
)

// demand_origin_process_join_docker_test.go — the join migration v63 exists for.
//
// ── WHAT WAS WRONG, AND WHY A TYPE TEST WOULD NOT CATCH IT ───────────────────
//
// demand_origins.process_id was BIGINT holding an Edge SQLite row id.
// process_styles.process_id and style_claims.process_id are TEXT holding the
// Edge process NAME ("SNF2"), and so is PlantClaimsReport.ProcessID on the wire.
// Two descriptions of the same processes, and no query able to put them side by
// side: Postgres will not compare bigint to text at all, and there was no
// mapping table to route through because the absence of that mapping IS the
// problem. Round 3 killed two Phase 6 designs on it.
//
// So the assertion is not "the column is text" — a schema test says that and
// says nothing about whether the values MEAN the same thing. It is that the join
// RETURNS THE ROW: an episode written by the demand path finds the active style
// mirrored by the plant-claims path, with no cast and no translation. That is
// the capability, and it is the thing that silently stops working if either side
// drifts back to ids.
func TestDemandOriginJoinsProcessStyles(t *testing.T) {
	db := testdb.Open(t)

	// The plant-claims mirror, written the way the real feed writes it: the
	// process's NAME as the key.
	const proc = "SNF2"
	if err := plantclaims.ReplaceProcess(db.DB, proc,
		[]plantclaims.StyleRow{
			{ProcessID: proc, StyleID: "STYLE-RUNNING", ConfigGen: 7, IsActive: true},
			{ProcessID: proc, StyleID: "STYLE-IDLE", ConfigGen: 7, IsActive: false},
		}, nil, 0); err != nil {
		t.Fatalf("mirror plant claims: %v", err)
	}

	// The demand path, writing an episode for the same process.
	opened := time.Now().UTC().Add(-30 * time.Minute)
	if err := db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: "11111111-1111-1111-1111-111111111111", Revision: 1,
		EpisodeKey: "cell|PLANT.LINE1|" + proc + "|PANEL-B|supply",
		Kind:       "cell", Direction: "supply",
		StationID: "PLANT.LINE1", ProcessID: proc,
		PayloadCode: "PANEL-B", OpenedAt: opened,
	}); err != nil {
		t.Fatalf("upsert demand origin: %v", err)
	}

	// THE JOIN. No ::text, no ::bigint, no lookup table.
	var style string
	if err := db.QueryRow(`
		SELECT ps.style_id
		  FROM demand_origins o
		  JOIN process_styles ps ON ps.process_id = o.process_id
		 WHERE o.origin_id = $1 AND ps.is_active`,
		"11111111-1111-1111-1111-111111111111").Scan(&style); err != nil {
		t.Fatalf("joining demand_origins to process_styles on process_id failed: %v\n\n"+
			"This is the join migration v63 exists for. If the error mentions bigint/text "+
			"or an operator that does not exist, one side has drifted back to the Edge "+
			"SQLite row id and Core is carrying two identity systems for one set of "+
			"processes again.", err)
	}
	if style != "STYLE-RUNNING" {
		t.Errorf("active style for the episode's process = %q, want STYLE-RUNNING", style)
	}
}

// TestDemandOriginProcessIDRoundTripsAName is the narrower half: a name goes in
// and the SAME name comes back.
//
// Held separately because the join above would still pass if the column silently
// truncated or normalised the value — both sides would be wrong in the same way
// and would still match each other. "SNF2" is short and unremarkable; a process
// name with a hyphen and a digit run is the one that would expose a numeric
// coercion left behind by the old type.
func TestDemandOriginProcessIDRoundTripsAName(t *testing.T) {
	db := testdb.Open(t)

	const proc = "PLN-01/L"
	const id = "22222222-2222-2222-2222-222222222222"
	if err := db.UpsertDemandOrigin(store.DemandOrigin{
		OriginID: id, Revision: 1,
		EpisodeKey: "cell|PLANT.LINE1|" + proc + "|PANEL-B|supply",
		Kind:       "cell", Direction: "supply",
		StationID: "PLANT.LINE1", ProcessID: proc,
		PayloadCode: "PANEL-B", OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("upsert demand origin: %v", err)
	}

	got, err := db.GetDemandOrigin(id)
	if err != nil {
		t.Fatalf("get demand origin: %v", err)
	}
	if got == nil {
		t.Fatal("GetDemandOrigin returned no row for an episode just written")
	}
	if got.ProcessID != proc {
		t.Errorf("process_id round-tripped as %q, want %q", got.ProcessID, proc)
	}
}
