//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// A MAINTAINED GROUP MUST NOT SOURCE FROM ITSELF.
//
// Found by the MG2 sim campaign, on the first scenario. The design had named
// the rule before the keeper was built — "sourcing tiers for top-offs ... never
// from the group itself" — and the keeper as first shipped did not have it.
//
// WHAT WENT WRONG, from the run that found it. A six-position group standing at
// 2 carriers against a level of 4:
//
//	order 1  retrieve_empty  dispatched  src=PRB-P03  dst=PRB-P01  bin=3
//	order 2  retrieve_empty  dispatched  src=PRB-P04  dst=PRB-P02  bin=4
//
// Both top-off asks sourced the group's OWN remaining carriers and carried them
// from one of its positions to another. Two robot trips that moved nothing into
// the group.
//
// AND IT IS SELF-FEEDING, which is what makes it a defect rather than waste. A
// claimed carrier stops counting as `resident`, so the gap re-opens, so the
// keeper asks again — with fewer unclaimed carriers each round. The group
// shuffles itself and never reaches its level.
//
// preferZone is what made it easy to hit: the plant-wide empty search prefers
// the destination's zone, and a maintained group's own positions share its
// zone, so the group's own carriers ranked FIRST.
func TestMaintainer_DoesNotSourceFromItsOwnGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "SELFSRC", 6, map[string]int{"SELF-STD": 4})
	bt := types["SELF-STD"]

	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "positions")
	for i := 0; i < 4; i++ {
		landCarrier(t, db, bt, children[i].ID, fmt.Sprintf("SELF-BIN-%d", i+1))
	}

	// ONE ELIGIBLE CARRIER OUTSIDE, present from the start.
	//
	// It is what makes this test non-vacuous, and it has to be here BEFORE the
	// asks are made rather than added later: a parked ask retries on an event,
	// and inserting a bin row straight into the database fires none. With the
	// candidate present the ordinary flow runs — ask created, scanner sources it
	// — and the assertion is about which carrier it chose.
	outsideGrp, err := nodes.CreateGroup(db.DB, "SELFSRC-ELSEWHERE")
	testutil.MustNoErr(t, err, "elsewhere group")
	outsideSlot := &nodes.Node{Name: "SELFSRC-FAR", Enabled: true, ParentID: &outsideGrp}
	testutil.MustNoErr(t, db.CreateNode(outsideSlot), "create SELFSRC-FAR")
	landCarrier(t, db, bt, outsideSlot.ID, "SELF-BIN-OUTSIDE")

	m := eng.Maintainer()
	m.Tick()

	// Two carriers leave. The keeper is now short two, and the two still
	// standing there are of exactly the type it wants — the bait.
	_, err = db.Exec(`DELETE FROM bins WHERE label IN ('SELF-BIN-1','SELF-BIN-2')`)
	testutil.MustNoErr(t, err, "two carriers leave")

	m.Tick()
	st := evidence(t, m, "short two, two resident", "SELFSRC", "SELF-STD")
	if st.Created != 2 {
		t.Fatalf("setup: created=%d, want 2", st.Created)
	}

	assertGroupCarriersUntouched(t, db, grpID, "immediately after the asks were made")

	// ── THE VACUITY CHECK, and it is not optional ────────────────────────────
	//
	// The claim assertions above all pass if the typed empty search simply
	// THROWS: the source finder treats any error as "no empty found", so a query
	// that never runs looks exactly like a correct exclusion. Not hypothetical —
	// the first version of this fix named the CTE `subtree` when nodetree calls
	// it `descendants`, and every claim assertion here was green against SQL
	// that errored on every call.
	//
	// So require the keeper to actually SOURCE the one eligible carrier. Only a
	// working exclusion can both skip the inside and find the outside.
	waitFor(t, 20*time.Second, func() bool {
		var claimed int
		_ = db.QueryRow(`SELECT COUNT(*) FROM bins WHERE label = 'SELF-BIN-OUTSIDE'
			AND claimed_by IS NOT NULL`).Scan(&claimed)
		return claimed == 1
	}, "the outside carrier to be claimed by a top-off ask")

	// And the inside ones are STILL untouched, which is the whole point: the
	// exclusion removed the group and nothing else.
	assertGroupCarriersUntouched(t, db, grpID, "after an outside carrier was sourced")

	// AND THE COUNT HELD. The whole consequence of the defect was `resident`
	// collapsing as the group's own carriers got claimed, so the count is the
	// assertion that matters most.
	m.Tick()
	st = evidence(t, m, "count held", "SELFSRC", "SELF-STD")
	if st.Resident != 2 {
		t.Errorf("resident=%d, want 2. The group's own carriers were claimed by its own "+
			"top-off asks, which is the self-feeding loop: fewer residents means a bigger "+
			"gap means more asks means fewer residents", st.Resident)
	}

}

// waitFor polls until cond holds or the deadline passes. Polling rather than a
// fixed sleep: the fulfillment scanner is event-driven, so the interesting
// moment arrives when it arrives.
func waitFor(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s. If the group's own carriers are also "+
		"untouched, the typed empty query is FAILING rather than excluding — every caller "+
		"reads an error as 'no empty found', so a broken query is invisible", limit, what)
}

func assertGroupCarriersUntouched(t *testing.T, db *store.DB, grpID int64, when string) {
	t.Helper()
	rows, err := db.Query(`
		SELECT b.label, COALESCE(n.name,'?'), COALESCE(b.claimed_by::text,'')
		  FROM bins b JOIN nodes n ON n.id = b.node_id
		 WHERE n.parent_id = $1`, grpID)
	testutil.MustNoErr(t, err, "read group carriers")
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var label, node, claimedBy string
		testutil.MustNoErr(t, rows.Scan(&label, &node, &claimedBy), "scan")
		seen++
		if claimedBy != "" {
			t.Errorf("%s: carrier %s at %s is claimed by order %s. A top-off ask sourced from "+
				"the group it was filling — the trip moves nothing in, and the claim drops the "+
				"count that decides whether to ask again", when, label, node, claimedBy)
		}
	}
	testutil.MustNoErr(t, rows.Err(), "rows")
	if seen == 0 {
		t.Errorf("%s: no carriers left in the group at all", when)
	}
}
