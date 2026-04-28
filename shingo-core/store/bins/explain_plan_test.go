//go:build explain

// Build-tag-gated harness for inspecting query plans of the post-2026-04-27
// advisory FindEmptyCompatible / FindSourceFIFO queries against plant-scale
// synthetic data. Does not run as part of the regular test suite.
//
// To execute:
//
//	cd shingo-core
//	go test -tags explain -v -run TestExplainPlan ./store/bins/...
//
// Requires Docker Desktop (uses testcontainers-go to spin up Postgres,
// same as every other integration test in this package).
//
// What it does:
//  1. Spins up a fresh Postgres container.
//  2. Seeds plant-scale synthetic data: 5000 bins, 200 nodes, 20 bin
//     types, 102 payloads (50 with rules, 52 without).
//  3. Runs ANALYZE so the planner has fresh statistics.
//  4. Executes EXPLAIN (ANALYZE, BUFFERS) against both branches of the
//     advisory clause: rules-enforced (IN) and no-rules (NOT EXISTS).
//     Same harness for FindSourceFIFO (full-bin retrieve), which uses
//     the same advisory clause.
//  5. Calls bins.FindEmptyCompatible directly so the harness mirrors
//     real prod call shape, not just hand-rolled SQL.
//
// What to look for in `t.Log` output:
//   - Total Time near each plan footer: under ~50ms is fine, sub-5ms is
//     great, multiple seconds is the bad case the v2 doc warned about.
//   - Subquery nodes (the IN and the NOT EXISTS): "actual loops=1" is
//     what you want — planner ran them once. "loops=N" matching the
//     bins row count means per-row re-execution; refactor to LEFT
//     JOIN form before deploy.
//   - "Hash Semi Join" / "Hash Anti Join" node types are good signs
//     that the planner correctly recognized the IN/NOT EXISTS as
//     set-membership tests.

package bins_test

import (
	"database/sql"
	"errors"
	"fmt"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
)

// explainSeedSQL populates plant-scale synthetic data in a single
// statement batch. Sized to give the planner enough rows to choose a
// real strategy (single-digit-row tables get sequential-scanned
// regardless and don't tell you anything about production behavior).
const explainSeedSQL = `
INSERT INTO nodes (name, zone, enabled, is_synthetic)
SELECT 'EXP-NODE-' || g,
       (ARRAY['A','B','C','D','E'])[(g % 5) + 1],
       true, false
FROM generate_series(1, 200) g;

INSERT INTO bin_types (code, description)
SELECT 'EXP-BT-' || g, 'synthetic'
FROM generate_series(1, 20) g;

INSERT INTO payloads (code, description)
SELECT 'EXP-PL-' || g, 'synthetic'
FROM generate_series(1, 100) g;

-- First 50 of the 100 generic payloads get rules pointing at bin
-- types 1-3. The other 50 have no rules — they exercise the
-- NOT EXISTS fallback branch via the generic data path.
INSERT INTO payload_bin_types (payload_id, bin_type_id)
SELECT p.id, bt.id
FROM payloads p
CROSS JOIN bin_types bt
WHERE p.code LIKE 'EXP-PL-%'
  AND CAST(SUBSTRING(p.code FROM 8) AS INTEGER) <= 50
  AND bt.code IN ('EXP-BT-1', 'EXP-BT-2', 'EXP-BT-3');

-- Two specific payloads used by the EXPLAIN runs. Named so the queries
-- below are self-documenting about which branch they exercise.
INSERT INTO payloads (code) VALUES ('EXP-WITH-RULES');
INSERT INTO payload_bin_types (payload_id, bin_type_id)
SELECT (SELECT id FROM payloads WHERE code = 'EXP-WITH-RULES'), bt.id
FROM bin_types bt WHERE bt.code IN ('EXP-BT-1', 'EXP-BT-2');

INSERT INTO payloads (code) VALUES ('EXP-NO-RULES');

-- 5000 bins distributed across 20 bin types and 200 nodes. ~30%
-- empty (payload_code = ''), ~70% loaded with one of the EXP-PL-N
-- payload codes. ~70% manifest_confirmed. Mix is intentional so the
-- planner has to filter, not just scan.
INSERT INTO bins (bin_type_id, label, node_id, status, payload_code, manifest_confirmed)
SELECT
    (SELECT id FROM bin_types WHERE code = 'EXP-BT-' || (((g - 1) % 20) + 1)),
    'EXP-BIN-' || g,
    (SELECT id FROM nodes WHERE name = 'EXP-NODE-' || (((g - 1) % 200) + 1)),
    'available',
    CASE WHEN g % 10 < 3 THEN ''
         ELSE 'EXP-PL-' || (((g * 7) % 100) + 1)
    END,
    g % 10 >= 3
FROM generate_series(1, 5000) g;

-- Refresh planner stats. Without this the bulk-inserted tables look
-- empty to the planner and you get a misleadingly cheap plan.
ANALYZE;
`

func TestExplainPlan_FindEmptyCompatible(t *testing.T) {
	db := testdb.Open(t)

	if _, err := db.Exec(explainSeedSQL); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var binCount, nodeCount, btCount, plCount, pbtCount int
	db.QueryRow("SELECT COUNT(*) FROM bins").Scan(&binCount)
	db.QueryRow("SELECT COUNT(*) FROM nodes").Scan(&nodeCount)
	db.QueryRow("SELECT COUNT(*) FROM bin_types").Scan(&btCount)
	db.QueryRow("SELECT COUNT(*) FROM payloads").Scan(&plCount)
	db.QueryRow("SELECT COUNT(*) FROM payload_bin_types").Scan(&pbtCount)
	t.Logf("seeded: %d bins / %d nodes / %d bin_types / %d payloads / %d payload_bin_types rules",
		binCount, nodeCount, btCount, plCount, pbtCount)

	// any-zone branch query — same shape as the post-2026-04-27
	// FindEmptyCompatible. Reuses the exported BinJoinQuery and
	// PayloadBinTypeAdvisoryClause constants so any future drift in
	// the production query is reflected here automatically.
	anyZoneQuery := fmt.Sprintf(`
		%s
		WHERE b.status = 'available'
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  AND b.node_id IS NOT NULL
		  AND n.enabled = true
		  AND n.is_synthetic = false
		  AND COALESCE(b.payload_code, '') = ''
		  AND ($2 = 0 OR b.node_id != $2)
		  %s
		ORDER BY b.id ASC
		LIMIT 1
	`, bins.BinJoinQuery, bins.PayloadBinTypeAdvisoryClause)

	runExplain := func(label, payloadCode, query string, args ...any) {
		t.Run(label, func(t *testing.T) {
			rows, err := db.Query("EXPLAIN (ANALYZE, BUFFERS) "+query, args...)
			if err != nil {
				t.Fatalf("explain: %v", err)
			}
			defer rows.Close()
			t.Logf("--- payload=%q ---", payloadCode)
			for rows.Next() {
				var line string
				if err := rows.Scan(&line); err != nil {
					t.Fatalf("scan: %v", err)
				}
				t.Log(line)
			}
		})
	}

	// Two FindEmptyCompatible runs — one per branch of the advisory
	// clause. The IN branch exercises what happens when rules exist;
	// the NOT EXISTS branch exercises the no-rules fallback.
	runExplain("FindEmptyCompatible_IN_branch_rules_exist",
		"EXP-WITH-RULES", anyZoneQuery, "EXP-WITH-RULES", int64(0))
	runExplain("FindEmptyCompatible_NOT_EXISTS_branch_no_rules",
		"EXP-NO-RULES", anyZoneQuery, "EXP-NO-RULES", int64(0))

	// FindSourceFIFO uses the same advisory clause but filters on
	// loaded bins instead of empty ones. Same planner concerns apply.
	fifoQuery := fmt.Sprintf(`
		%s
		WHERE b.payload_code = $1
		  AND b.manifest_confirmed = true
		  AND b.status NOT IN ('staged', 'maintenance')
		  AND b.claimed_by IS NULL
		  AND b.locked = false
		  %s
		ORDER BY b.loaded_at ASC NULLS LAST, b.id ASC
		LIMIT 1
	`, bins.BinJoinQuery, bins.PayloadBinTypeAdvisoryClause)

	// Pick a payload code that has loaded bins in the seeded data.
	// EXP-PL-7 is in the rules-enforced 1..50 range and gets 70%
	// of bins assigned to it via the (g*7) % 100 distribution
	// hitting it for many g values.
	runExplain("FindSourceFIFO_IN_branch_rules_exist",
		"EXP-PL-7", fifoQuery, "EXP-PL-7")
	runExplain("FindSourceFIFO_NOT_EXISTS_branch_no_rules",
		"EXP-PL-77", fifoQuery, "EXP-PL-77")

	// Sanity check: the synthetic data actually exercises the real
	// FindEmptyCompatible function, not just hand-rolled SQL.
	t.Run("sanity_real_FindEmptyCompatible", func(t *testing.T) {
		bin, err := bins.FindEmptyCompatible(db.DB, "EXP-WITH-RULES", "", 0)
		switch {
		case err == nil:
			t.Logf("rules-enforced: returned bin %d (type=%s, label=%s)",
				bin.ID, bin.BinTypeCode, bin.Label)
		case errors.Is(err, sql.ErrNoRows):
			t.Log("rules-enforced: no empty bin matched (data distribution didn't seed an empty of bin types 1 or 2 — not a query bug)")
		default:
			t.Fatalf("rules-enforced: unexpected error: %v", err)
		}

		bin, err = bins.FindEmptyCompatible(db.DB, "EXP-NO-RULES", "", 0)
		switch {
		case err == nil:
			t.Logf("no-rules: returned bin %d (type=%s, label=%s)",
				bin.ID, bin.BinTypeCode, bin.Label)
		case errors.Is(err, sql.ErrNoRows):
			t.Log("no-rules: no empty bin matched (unexpected — fallback should match any empty)")
		default:
			t.Fatalf("no-rules: unexpected error: %v", err)
		}
	})
}
