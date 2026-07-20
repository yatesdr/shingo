//go:build docker

package sourceability_test

import (
	"database/sql"
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/plantclaims"
	"shingocore/store/sourceability"
)

// TestFullRecompute_PlantScalePerf seeds a plant-scale dataset and times one full
// recompute (BuildInputs + Compute — what the monitor's recomputeAll runs). It
// reports the wall-clock number the verify pass deferred, and guards against a
// gross regression. Expect it trivial on real hardware; this runs on a shared CI
// Postgres container, so the bound is loose.
func TestFullRecompute_PlantScalePerf(t *testing.T) {
	sdb := testdb.Open(t)
	db := sdb.DB
	std := testdb.SetupStandardData(t, sdb)

	const (
		payloads  = 40
		nodeCount = 2000 // one available bin each → 2000-bin pool
		processes = 8
		styles    = 15
		claims    = 12
	)

	bt, err := sdb.GetBinTypeByCode("DEFAULT")
	testutil.MustNoErr(t, err, "default bin type")

	// Bulk pool: 2000 available bins across 40 payloads, one per fresh node.
	execAll(t, db,
		fmt.Sprintf(`INSERT INTO nodes (name, enabled, is_synthetic, node_type_id)
		 SELECT 'PERF-N'||g, true, false, %d
		 FROM generate_series(1, %d) g`, *std.StorageNode.NodeTypeID, nodeCount),
		fmt.Sprintf(`INSERT INTO bins (bin_type_id, node_id, status, payload_code, manifest_confirmed, uop_remaining)
		 SELECT %d, n.id, 'available', 'P'||(n.id %% %d), true, 100
		 FROM nodes n WHERE n.name LIKE 'PERF-N%%'`, bt.ID, payloads),
	)

	// Mirror: 8 processes × 15 styles × 12 claims, payloads P0..P39.
	for p := range processes {
		process := fmt.Sprintf("PROC-%d", p)
		var srows []plantclaims.StyleRow
		var crows []plantclaims.ClaimRow
		for s := range styles {
			style := fmt.Sprintf("S%d", s)
			srows = append(srows, plantclaims.StyleRow{ProcessID: process, StyleID: style})
			for c := range claims {
				crows = append(crows, plantclaims.ClaimRow{
					ProcessID:    process,
					StyleID:      style,
					CoreNodeName: fmt.Sprintf("%s-%s-N%d", process, style, c),
					PayloadCode:  fmt.Sprintf("P%d", (s*claims+c)%payloads),
					Seq:          c,
				})
			}
		}
		testutil.MustNoErr(t, plantclaims.ReplaceProcess(db, process, srows, crows, 0), "seed mirror")
	}

	// Warm one pass (connection/plan warmup), then time the measured pass.
	if _, err := sourceability.BuildInputs(db, 30*time.Minute); err != nil {
		t.Fatalf("warmup: %v", err)
	}

	start := time.Now()
	in, err := sourceability.BuildInputs(db, 30*time.Minute)
	testutil.MustNoErr(t, err, "build inputs")
	states := sourceability.Compute(in, sourceability.Config{Horizon: 30 * time.Minute}, time.Now())
	elapsed := time.Since(start)

	t.Logf("full recompute: %d styles over a %d-bin / %d-payload pool → %s",
		len(states), nodeCount, payloads, elapsed)

	if len(states) != processes*styles {
		t.Fatalf("computed %d styles, want %d", len(states), processes*styles)
	}
	if elapsed > 5*time.Second {
		t.Errorf("full recompute took %s, over the 5s regression guard", elapsed)
	}
}

func execAll(t *testing.T, db *sql.DB, stmts ...string) {
	t.Helper()
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			t.Fatalf("seed exec: %v\n%s", err, s)
		}
	}
}
