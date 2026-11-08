package store

import (
	"testing"

	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// TestMigrate_V44_ABoundCartWithNoPartTakesItsClaimsPartAssumed: v44 records,
// for every bound cart whose seat records no part, the part of the consume
// claim its runtime points at, source assumed, not known (decision 57): what
// the bind would have recorded. A known cart, a known-empty cart, a loader's
// seat, a produce seat, a claim naming no part and an empty seat are left as
// they were, and a second run changes nothing.
func TestMigrate_V44_ABoundCartWithNoPartTakesItsClaimsPartAssumed(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, err := db.CreateProcess("V44-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "process")
	sid, err := db.CreateStyle("V44-STYLE", "", pid)
	testutil.MustNoErr(t, err, "style")
	type seat struct {
		name, role, mode, claimPart string
		bound                       bool
		part                        string
		known                       bool
		source                      string
		wantPart, wantSource        string
		wantKnown                   bool
	}
	seats := []seat{
		{name: "V44-UNSAID", role: "consume", mode: "simple", claimPart: "PART-A", bound: true,
			wantPart: "PART-A", wantSource: "assumed"},
		{name: "V44-KNOWN", role: "consume", mode: "simple", claimPart: "PART-A", bound: true,
			part: "PART-K", known: true, source: "landing", wantPart: "PART-K", wantKnown: true, wantSource: "landing"},
		{name: "V44-EMPTY", role: "consume", mode: "simple", claimPart: "PART-A", bound: true,
			known: true, source: "operator", wantKnown: true, wantSource: "operator"},
		{name: "V44-LOADER", role: "consume", mode: "manual_swap", claimPart: "PART-A", bound: true},
		{name: "V44-PRODUCE", role: "produce", mode: "simple", claimPart: "PART-P", bound: true},
		{name: "V44-NO-PART", role: "consume", mode: "simple", bound: true},
		{name: "V44-NO-CART", role: "consume", mode: "simple", claimPart: "PART-A", source: "departed", wantSource: "departed"},
	}
	ids := map[string]int64{}
	for _, s := range seats {
		nodeID, err := db.CreateProcessNode(processes.NodeInput{ProcessID: pid, CoreNodeName: s.name, Code: s.name, Name: s.name, Enabled: true})
		testutil.MustNoErr(t, err, "node "+s.name)
		ids[s.name] = nodeID
		res, err := db.Exec(`INSERT INTO style_node_claims (style_id, core_node_name, role, swap_mode, payload_code)
			VALUES (?, ?, ?, ?, ?)`, sid, s.name, s.role, s.mode, s.claimPart)
		testutil.MustNoErr(t, err, "claim "+s.name)
		claimID, err := res.LastInsertId()
		testutil.MustNoErr(t, err, "claim id "+s.name)
		var bin any
		if s.bound {
			bin = 9000 + nodeID
		}
		_, err = db.Exec(`INSERT INTO process_node_runtime_states (process_node_id, active_claim_id, active_bin_id,
			lineside_payload_code, lineside_payload_known, lineside_source) VALUES (?, ?, ?, ?, ?, ?)`,
			nodeID, claimID, bin, s.part, s.known, s.source)
		testutil.MustNoErr(t, err, "seat "+s.name)
	}
	v44 := edgeMigrationVersion(t, "runtime_assumed_part_backfill")
	for run := 1; run <= 2; run++ {
		for _, m := range edgeMigrations() {
			if m.Version != v44 {
				continue
			}
			tx, err := db.Begin()
			testutil.MustNoErr(t, err, "begin")
			testutil.MustNoErr(t, m.Fn(tx), "v44")
			testutil.MustNoErr(t, tx.Commit(), "commit")
		}
		for _, s := range seats {
			var part, source string
			var known bool
			testutil.MustNoErr(t, db.QueryRow(`SELECT lineside_payload_code, lineside_payload_known, lineside_source
				FROM process_node_runtime_states WHERE process_node_id = ?`, ids[s.name]).Scan(&part, &known, &source), "read "+s.name)
			if part != s.wantPart || known != s.wantKnown || source != s.wantSource {
				t.Errorf("run %d: %s reads %q known %t from %q, want %q known %t from %q", run, s.name, part, known, source,
					s.wantPart, s.wantKnown, s.wantSource)
			}
		}
	}
}
