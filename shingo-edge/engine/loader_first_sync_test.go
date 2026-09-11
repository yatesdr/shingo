package engine

import (
	"database/sql"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// TestFirstLoaderSync_MovesTheStoredClaim is census 15: the deploy shape — a
// stored manual_swap claim, then the first node-list sync — with the node (a)
// inside Core's loader set and (b) outside it. Springfield's SMN_001 is (b): its
// Core loader was archived 2026-07-30 and its stored claims remain.
//
// In both cases the row moves to style_node_claims_quarantine and the three
// claim-id pointers into it — the runtime's active_claim_id and a changeover
// task's from/to — are NULL. The Edge runs SQLite with foreign keys off, so a
// pointer the move does not clear names a row that is gone. What differs is the
// board afterwards: in (a) the loader operations are admitted through the
// synthesized claim; in (b) they are refused "has no active claim".
//
// COVERAGE PIN. The sim never binds it: no plant file seeds a stored manual_swap
// claim. MUTATION: drop the runtime UPDATE from QuarantineLoaderClaims — both
// cases then fail on active_claim_id.
func TestFirstLoaderSync_MovesTheStoredClaim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		prefix string
		inside bool
	}{
		{"inside the loader set", "QIN", true},
		{"outside the loader set", "QOUT", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			eng := testEngine(t, db)
			unreachableCore(t, eng)

			nodeID, claimID := seedManualSwapClaim(t, db, tc.prefix, protocol.ClaimRoleProduce, "PART-X", "STORAGE-NODE")
			coreNode := tc.prefix + "-MSWAP-NODE"
			_, err := db.EnsureProcessNodeRuntime(nodeID)
			testutil.MustNoErr(t, err, "ensure runtime")
			var processID, styleID int64
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT process_id FROM process_nodes WHERE id=?`, nodeID).Scan(&processID), "read process")
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT style_id FROM style_node_claims WHERE id=?`, claimID).Scan(&styleID), "read style")

			// The three pointers the move must clear, all aimed at the stored claim.
			_, err = db.DB.Exec(`UPDATE process_node_runtime_states SET active_claim_id=? WHERE process_node_id=?`, claimID, nodeID)
			testutil.MustNoErr(t, err, "point the runtime at the claim")
			res, err := db.DB.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id) VALUES (?, ?, ?)`,
				processID, styleID, styleID)
			testutil.MustNoErr(t, err, "insert changeover")
			changeoverID, err := res.LastInsertId()
			testutil.MustNoErr(t, err, "changeover id")
			res, err = db.DB.Exec(`INSERT INTO changeover_node_tasks (process_changeover_id, process_node_id, from_claim_id, to_claim_id)
				VALUES (?, ?, ?, ?)`, changeoverID, nodeID, claimID, claimID)
			testutil.MustNoErr(t, err, "insert node task")
			taskID, err := res.LastInsertId()
			testutil.MustNoErr(t, err, "node task id")

			// A non-empty loader set either way: an empty one is a failed projection
			// and moves nothing (reconcileLoaderClaims).
			sync := sharedLoaderInfo("QOTHER-W1", "produce", "operator", "PART-X", 0, 0)
			if tc.inside {
				sync = sharedLoaderInfo(coreNode, "produce", "operator", "PART-X", 0, 0)
			}
			eng.SetCoreLoaders([]protocol.LoaderInfo{sync})

			var stored, archived int
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM style_node_claims WHERE id=?`, claimID).Scan(&stored), "count stored")
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT COUNT(*) FROM style_node_claims_quarantine WHERE id=?`, claimID).Scan(&archived), "count archived")
			if stored != 0 || archived != 1 {
				t.Fatalf("after the first sync the claim is stored=%d archived=%d, want 0 and 1", stored, archived)
			}

			var active, from, to sql.NullInt64
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT active_claim_id FROM process_node_runtime_states WHERE process_node_id=?`,
				nodeID).Scan(&active), "read runtime")
			testutil.MustNoErr(t, db.DB.QueryRow(`SELECT from_claim_id, to_claim_id FROM changeover_node_tasks WHERE id=?`,
				taskID).Scan(&from, &to), "read node task")
			if active.Valid || from.Valid || to.Valid {
				t.Errorf("pointers into the moved claim survive (active_claim_id=%v from_claim_id=%v to_claim_id=%v): "+
					"foreign keys are off, so each names a row that is gone", active, from, to)
			}

			for _, op := range loaderOnlyOps(eng) {
				err := op.call(nodeID)
				gated := err != nil && (strings.Contains(err.Error(), notALoaderMessage) ||
					strings.Contains(err.Error(), "has no active claim"))
				if tc.inside && gated {
					t.Errorf("%s on a node inside Core's loader set was refused by the loader gate (%v) — once the "+
						"stored claim is gone its board runs on the synthesized one", op.name, err)
				}
				if !tc.inside && (err == nil || !strings.Contains(err.Error(), "has no active claim")) {
					t.Errorf("%s on a node Core no longer calls a loader: error %v, want \"has no active claim\"", op.name, err)
				}
			}
		})
	}
}
