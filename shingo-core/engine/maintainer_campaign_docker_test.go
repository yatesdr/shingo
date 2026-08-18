//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// maintainer_campaign_docker_test.go — the MG2 sim campaign, as scripted runs.
//
// ── WHAT THESE PROVE, AND WHAT THEY DO NOT ────────────────────────────────
//
// PROVE: the level keeper's arithmetic and its two edges, against a REAL
// Postgres, through the real store queries, driven by the real Maintainer over
// a real Engine. Every count the subtraction depends on is computed by the same
// SQL production runs. Where a scenario is about the keeper deciding something,
// that decision is the real one.
//
// DO NOT PROVE: fleet behaviour. Carrier ARRIVAL is staged by writing the bin
// where a delivered carrier would be, rather than by driving a robot through
// the simulator to put it there. That is deliberate and it is the honest
// boundary: what the keeper does with a carrier that arrived is Core logic;
// whether a robot can get it there is a dispatch question with its own tests
// and its own soak. A scenario written to prove both would prove neither well,
// and would fail for dispatch reasons while claiming to measure the keeper.
//
// The campaign document records which scenario is which, and what a scripted
// run at this grain leaves for the floor.
//
// EVIDENCE IS LOGGED, NOT INFERRED. Every scenario prints the subtraction it
// observed at each step, so a run's output IS the record: `go test -v` output
// pastes into the campaign doc without anybody re-deriving numbers by hand.

// ── the rig ─────────────────────────────────────────────────────────────────

// campaignGroup builds a maintained group: `slots` free positions, one declared
// level per (type, want) pair given. Returns the group id and the type ids.
func campaignGroup(t *testing.T, db *store.DB, group string, slots int, levels map[string]int) (int64, map[string]int64) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, group)
	testutil.MustNoErr(t, err, "CreateGroup")
	for i := 0; i < slots; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("%s-P%02d", group, i+1), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(n), "create position")
	}
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "test-core"), "station")

	ids := map[string]int64{}
	for code, want := range levels {
		var btID int64
		testutil.MustNoErr(t, db.QueryRow(
			`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&btID), "bin type "+code)
		ids[code] = btID
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
			GroupNodeID: grpID, BinTypeID: btID, Want: want,
		}), "declare level "+code)
	}
	return grpID, ids
}

// landCarrier stages an ARRIVAL: the carrier is written where a delivered one
// would be. See the boundary note at the top of the file.
func landCarrier(t *testing.T, db *store.DB, binTypeID, nodeID int64, label string) {
	t.Helper()
	_, err := db.Exec(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available')`,
		binTypeID, label, nodeID)
	testutil.MustNoErr(t, err, "land carrier "+label)
}

// freePosition returns the id of a position in the group with no bin on it.
func freePosition(t *testing.T, db *store.DB, grpID int64) int64 {
	t.Helper()
	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "list positions")
	for _, c := range children {
		bs, berr := db.ListBinsByNode(c.ID)
		testutil.MustNoErr(t, berr, "bins at position")
		if len(bs) == 0 {
			return c.ID
		}
	}
	t.Fatal("no free position left in the group")
	return 0
}

// evidence prints one line of the subtraction for a (group, type).
func evidence(t *testing.T, m *Maintainer, step, group, typeCode string) MaintainerGroupState {
	t.Helper()
	st := mntState(t, m, group, typeCode)
	t.Logf("%-28s %s/%-10s want=%d resident=%d asked=%d coming=%d gap=%d created=%d origin=%s%s",
		step, group, typeCode, st.Want, st.Resident, st.Asked, st.Coming, st.Gap, st.Created,
		shortOrigin(st.OriginID), parkedNote(st))
	return st
}

func shortOrigin(id string) string {
	if id == "" {
		return "(none)"
	}
	if len(id) > 8 {
		return id[:8]
	}
	return id
}

func parkedNote(st MaintainerGroupState) string {
	if st.OldestAskCause == "" {
		return ""
	}
	return " parked=" + st.OldestAskCause + "/" + st.OldestAskAge
}

// terminaliseAsks marks every ask on an origin terminal — the stand-in for the
// carriers having got there, used where a scenario needs the episode to settle.
func terminaliseAsks(t *testing.T, db *store.DB, originID string) {
	t.Helper()
	_, err := db.Exec(`UPDATE orders SET status = $1 WHERE origin_id = $2`,
		string(protocol.StatusConfirmed), originID)
	testutil.MustNoErr(t, err, "terminalise asks")
}

// ── S1 · steady refill ──────────────────────────────────────────────────────
//
// The base case the whole feature exists for: a press takes a carrier, the level
// goes short, the keeper asks for exactly the difference and no more, and the
// episode closes when the level is met AND nothing is outstanding.
func TestCampaign_S1_SteadyRefill(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "CMP-S1", 6, map[string]int{"S1-STD": 4})
	bt := types["S1-STD"]

	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "positions")
	for i := 0; i < 4; i++ {
		landCarrier(t, db, bt, children[i].ID, fmt.Sprintf("S1-BIN-%d", i+1))
	}

	m := eng.Maintainer()
	m.Tick()
	st := evidence(t, m, "settled at level", "CMP-S1", "S1-STD")
	if st.OriginID != "" || st.Created != 0 {
		t.Fatalf("a group standing at its level opened an episode: origin=%s created=%d",
			st.OriginID, st.Created)
	}

	// A press takes two.
	_, err = db.Exec(`DELETE FROM bins WHERE label IN ('S1-BIN-1','S1-BIN-2')`)
	testutil.MustNoErr(t, err, "press takes two")

	m.Tick()
	st = evidence(t, m, "two taken", "CMP-S1", "S1-STD")
	if st.Created != 2 {
		t.Fatalf("created=%d for a shortfall of 2 — the keeper must ask for the difference "+
			"and no more", st.Created)
	}
	origin := st.OriginID

	m.Tick()
	st = evidence(t, m, "re-tick, nothing new", "CMP-S1", "S1-STD")
	if st.Created != 0 || st.Asked != 2 {
		t.Errorf("second tick created=%d asked=%d — re-running must be harmless, and it is "+
			"harmless only because the keeper subtracts its own live asks", st.Created, st.Asked)
	}

	// The carriers arrive. The level is met, but the asks are still live: the
	// RISING edge. The episode must not close here.
	landCarrier(t, db, bt, freePosition(t, db, grpID), "S1-BIN-5")
	landCarrier(t, db, bt, freePosition(t, db, grpID), "S1-BIN-6")
	m.Tick()
	st = evidence(t, m, "rising edge (asks live)", "CMP-S1", "S1-STD")
	if st.OriginID != origin {
		t.Fatalf("episode closed on the rising edge — that is the duplicate window")
	}

	// The asks go terminal. The SETTLE edge.
	terminaliseAsks(t, db, origin)
	m.Tick()
	st = evidence(t, m, "settle edge", "CMP-S1", "S1-STD")
	if st.OriginID != "" {
		t.Errorf("episode still open after the level settled and every ask went terminal: %s",
			st.OriginID)
	}
}

// ── S2 · dry market: park, then unpark ──────────────────────────────────────
//
// Nothing to source. The ask is made anyway and PARKS — queue-on-full and
// queue-on-dry are the same contract, and nothing anywhere cancels. The property
// under test is that a parked episode does not accumulate duplicates while it
// waits, which is the failure mode a level keeper invites.
func TestCampaign_S2_DryMarketParksThenUnparks(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "CMP-S2", 4, map[string]int{"S2-STD": 2})
	bt := types["S2-STD"]

	m := eng.Maintainer()
	m.Tick()
	st := evidence(t, m, "dry, first tick", "CMP-S2", "S2-STD")
	origin := st.OriginID
	if origin == "" {
		t.Fatal("no episode opened against an empty group")
	}
	firstCreated := st.Created

	// Ten ticks with nothing to source. THE COUNT MUST NOT MOVE.
	for i := 0; i < 10; i++ {
		m.Tick()
	}
	st = evidence(t, m, "after 10 dry ticks", "CMP-S2", "S2-STD")
	if st.Created != 0 {
		t.Errorf("a dry tick created %d more ask(s). Ten minutes of a dry market would be "+
			"twenty orders and an hour would be a hundred — the 241-duplicates shape at a "+
			"new grain", st.Created)
	}
	if st.OriginID != origin {
		t.Errorf("episode re-minted while parked: %s -> %s", origin, st.OriginID)
	}
	live, _, lerr := db.ListOrdersByOrigin(origin, 200)
	testutil.MustNoErr(t, lerr, "list asks")
	if len(live) != firstCreated {
		t.Errorf("origin carries %d orders after 10 dry ticks, want the %d it opened with",
			len(live), firstCreated)
	}
	t.Logf("S2 evidence: %d ask(s) still, after 11 ticks against a dry market", len(live))

	// The market fills. The episode settles without a second round of asks.
	landCarrier(t, db, bt, freePosition(t, db, grpID), "S2-BIN-1")
	landCarrier(t, db, bt, freePosition(t, db, grpID), "S2-BIN-2")
	terminaliseAsks(t, db, origin)
	m.Tick()
	st = evidence(t, m, "unparked + settled", "CMP-S2", "S2-STD")
	if st.OriginID != "" {
		t.Errorf("episode did not settle once the level was met and the asks went terminal")
	}
}

// ── S3 · last-slot race ─────────────────────────────────────────────────────
//
// The gap says four; the group has one position free. The pre-resolve loop is
// bounded by ResolveStore's per-child count+inflight>=1 rule, so it must stop at
// ONE — not four asks racing for one slot, and not a clamp somebody wrote.
func TestCampaign_S3_LastSlotBoundsTheAskCount(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, _ := campaignGroup(t, db, "CMP-S3", 4, map[string]int{"S3-STD": 4})

	// Three of four positions occupied by a DIFFERENT type, so `resident` for the
	// declared type is zero (gap 4) while only one position is free.
	var otherBT int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('S3-OTHER') RETURNING id`).Scan(&otherBT), "other type")
	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "positions")
	for i := 0; i < 3; i++ {
		landCarrier(t, db, otherBT, children[i].ID, fmt.Sprintf("S3-OTHER-%d", i+1))
	}

	m := eng.Maintainer()
	m.Tick()
	st := evidence(t, m, "gap 4, one slot free", "CMP-S3", "S3-STD")
	if st.Gap != 4 {
		t.Fatalf("gap=%d, want 4 — the fixture no longer builds the case", st.Gap)
	}
	if st.Created != 1 {
		t.Errorf("created=%d against ONE free position. The pre-resolve loop must be bounded "+
			"by physical free slots: asking for a carrier with nowhere to put it is an order "+
			"that can only park", st.Created)
	}
}

// ── S4 · dig-child counting ─────────────────────────────────────────────────
//
// A compound child inherits its parent's origin. It must NOT count as a second
// ask, or one buried source would make the keeper believe it had asked twice and
// under-ask by one for the rest of the episode.
func TestCampaign_S4_DigChildDoesNotInflateAsked(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	_, _ = campaignGroup(t, db, "CMP-S4", 4, map[string]int{"S4-STD": 2})

	m := eng.Maintainer()
	m.Tick()
	st := evidence(t, m, "two asks out", "CMP-S4", "S4-STD")
	origin := st.OriginID
	if st.Created != 2 {
		t.Fatalf("setup: created=%d, want 2", st.Created)
	}
	m.Tick()
	askedBefore := evidence(t, m, "baseline asked", "CMP-S4", "S4-STD").Asked

	// One ask hits a buried source and becomes a compound parent; the dig child
	// inherits the origin.
	roots, _, rerr := db.ListOrdersByOrigin(origin, 200)
	testutil.MustNoErr(t, rerr, "list roots")
	if len(roots) == 0 {
		t.Fatal("no ask to make a parent of")
	}
	parent := roots[0]
	child := &orders.Order{
		EdgeUUID: "cmp-s4-dig-child", StationID: "test-core",
		OrderType: protocol.OrderTypeMove, Status: protocol.StatusQueued,
		Quantity: 1, OriginID: origin, ParentOrderID: &parent.ID,
	}
	testutil.MustNoErr(t, db.CreateOrder(child), "create dig child")

	m.Tick()
	st = evidence(t, m, "after a dig child", "CMP-S4", "S4-STD")
	if st.Asked != askedBefore {
		t.Errorf("asked moved %d -> %d when a compound child inherited the origin. The count "+
			"must be of ROOTS: a dig is how ONE ask gets its carrier, not a second ask, and "+
			"counting it would make the keeper under-ask by one for the rest of the episode",
			askedBefore, st.Asked)
	}
}

// ── S5 · restart mid-episode, BOTH windows ─────────────────────────────────
//
// Window A: the episode is open and the asks are out. Window B: the episode is
// open and the asks are NOT out yet (the keeper died between the mint and the
// pre-resolve loop). A restart in either window must reach the same conclusion
// from the same rows, because the keeper holds no episode state at all.
func TestCampaign_S5_RestartInBothWindows(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	_, _ = campaignGroup(t, db, "CMP-S5", 4, map[string]int{"S5-STD": 2})

	// ── Window A: mint + asks, then restart.
	first := eng.Maintainer()
	first.Tick()
	a := evidence(t, first, "window A: before restart", "CMP-S5", "S5-STD")
	if a.Created == 0 {
		t.Fatal("setup: no asks created")
	}
	restartedA := NewMaintainer(eng, func() time.Time { return time.Now().UTC() })
	restartedA.Tick()
	ar := evidence(t, restartedA, "window A: after restart", "CMP-S5", "S5-STD")
	if ar.Created != 0 || ar.OriginID != a.OriginID || ar.Asked != a.Created {
		t.Errorf("window A restart diverged: created=%d origin=%s asked=%d (was origin=%s created=%d)",
			ar.Created, shortOrigin(ar.OriginID), ar.Asked, shortOrigin(a.OriginID), a.Created)
	}

	// ── Window B: an episode open with NO asks against it — the keeper died
	// between the INSERT and the pre-resolve loop. The restarted keeper must
	// finish the job rather than mint a second episode.
	// order_history has an FK onto orders, so the audit rows go first. Erasing
	// the history is right for this fixture rather than a workaround: the window
	// being staged is one where the asks were never created, and an order that
	// was never created has no history either.
	_, err := db.Exec(`DELETE FROM order_history WHERE order_id IN
		(SELECT id FROM orders WHERE origin_id = $1)`, a.OriginID)
	testutil.MustNoErr(t, err, "erase the ask history")
	_, err = db.Exec(`DELETE FROM orders WHERE origin_id = $1`, a.OriginID)
	testutil.MustNoErr(t, err, "erase the asks, keep the episode")

	restartedB := NewMaintainer(eng, func() time.Time { return time.Now().UTC() })
	restartedB.Tick()
	br := evidence(t, restartedB, "window B: after restart", "CMP-S5", "S5-STD")
	if br.OriginID != a.OriginID {
		t.Errorf("window B re-minted the episode: %s -> %s. The open-key index is the guard, "+
			"and an episode with no children is still that key's episode",
			shortOrigin(a.OriginID), shortOrigin(br.OriginID))
	}
	if br.Created == 0 {
		t.Errorf("window B created nothing. An episode open with zero asks and a real gap is " +
			"a job half done, and the tick that finds it is the one that has to finish it")
	}
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	if len(open) != 1 {
		t.Errorf("open maintain episodes = %d, want exactly 1 across both restart windows", len(open))
	}
}

// ── S6 · changeover: the type asked for follows the declaration ────────────
//
// A press changing style changes which carrier type it needs. At Core grain the
// declaration IS the changeover: the level moves from one type to the other, and
// what matters is that the old episode ends honestly and the new one is asked
// for at the right type.
func TestCampaign_S6_ChangeoverMovesTheDemandBetweenTypes(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "CMP-S6", 6, map[string]int{"S6-BIG": 2, "S6-SMALL": 2})

	m := eng.Maintainer()
	m.Tick()
	big := evidence(t, m, "pre-changeover BIG", "CMP-S6", "S6-BIG")
	small := evidence(t, m, "pre-changeover SMALL", "CMP-S6", "S6-SMALL")
	if big.OriginID == "" || small.OriginID == "" {
		t.Fatal("setup: both types should have opened an episode")
	}
	if big.OriginID == small.OriginID {
		t.Fatal("both types share one episode — the grain is (group, TYPE)")
	}

	// The changeover: BIG is no longer wanted here, SMALL doubles.
	testutil.MustNoErr(t, db.RemoveMaintainLevel(grpID, types["S6-BIG"]), "withdraw BIG")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
		GroupNodeID: grpID, BinTypeID: types["S6-SMALL"], Want: 4,
	}), "raise SMALL")

	m.Tick()
	smallAfter := evidence(t, m, "post-changeover SMALL", "CMP-S6", "S6-SMALL")
	if smallAfter.Want != 4 {
		t.Errorf("SMALL want=%d, want 4 — the keeper reads config every tick", smallAfter.Want)
	}
	if smallAfter.OriginID != small.OriginID {
		t.Errorf("raising a level re-minted its episode (%s -> %s). The need did not restart; "+
			"it grew", shortOrigin(small.OriginID), shortOrigin(smallAfter.OriginID))
	}

	var reason string
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT close_reason FROM demand_origins WHERE origin_id = $1`, big.OriginID).Scan(&reason),
		"read BIG close reason")
	t.Logf("changeover: BIG episode %s closed %q", shortOrigin(big.OriginID), reason)
	if reason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("BIG closed %q, want %q. It did not get its carriers — it stopped being "+
			"watched, and calling that recovered reports a satisfied demand every time "+
			"somebody runs a changeover", reason, protocol.CloseReasonThresholdRemoved)
	}
}

// ── S7 · wrong-type pressure (PRE-STRICT BASELINE) ─────────────────────────
//
// The group is FULL of a type nobody declared, and short of the one that is.
// This is the baseline phase 3 has to beat, so what it records is a measurement,
// not a pass/fail on behaviour that has not been built yet.
//
// What IS asserted: the keeper does not count the wrong type as progress. A
// resident count that included any carrier standing there would report this
// group satisfied while every press it serves waits.
func TestCampaign_S7_WrongTypePressureBaseline(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, _ := campaignGroup(t, db, "CMP-S7", 6, map[string]int{"S7-WANTED": 3})

	var wrongBT int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('S7-WRONG') RETURNING id`).Scan(&wrongBT), "wrong type")
	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "positions")
	for i := 0; i < 4; i++ {
		landCarrier(t, db, wrongBT, children[i].ID, fmt.Sprintf("S7-WRONG-%d", i+1))
	}

	m := eng.Maintainer()
	m.Tick()
	st := evidence(t, m, "4 wrong-type resident", "CMP-S7", "S7-WANTED")

	if st.Resident != 0 {
		t.Errorf("resident=%d with zero carriers of the declared type standing there. A count "+
			"that includes the wrong type reports this group satisfied while every press it "+
			"serves waits", st.Resident)
	}
	if st.Gap != 3 {
		t.Errorf("gap=%d, want the full 3 — nothing about the wrong-type carriers reduces the "+
			"need for the right ones", st.Gap)
	}

	// THE MEASUREMENT phase 3 must beat: how much of the group is unusable, and
	// how many asks can be placed against what is left.
	t.Logf("S7 BASELINE — group CMP-S7: 6 positions, 4 held by an undeclared type, level 3 of "+
		"S7-WANTED. gap=%d created=%d. Free positions bound the asks, so the keeper places %d "+
		"of the %d it wants and the remaining %d has nowhere to go until somebody clears the "+
		"wrong-type carriers. NOTHING IN CORE CLEARS THEM TODAY — that is the gap phase 3 "+
		"is for, and this number is what it has to move.",
		st.Gap, st.Created, st.Created, st.Gap, st.Gap-st.Created)
}

// ── S8 · mix churn ─────────────────────────────────────────────────────────
//
// The measurement the strict-sourcing phase is judged against: how many asks the
// keeper places across a run where the declared mix keeps moving. Every ask is
// one robot trip, so the count IS the cost.
func TestCampaign_S8_MixChurnCost(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, types := campaignGroup(t, db, "CMP-S8", 8, map[string]int{"S8-A": 3, "S8-B": 3})

	m := eng.Maintainer()
	asksByType := map[string]int{}
	episodes := map[string]int{}
	seen := map[string]bool{}

	record := func(step string) {
		for _, code := range []string{"S8-A", "S8-B"} {
			st := evidence(t, m, step, "CMP-S8", code)
			asksByType[code] += st.Created
			if st.OriginID != "" && !seen[st.OriginID] {
				seen[st.OriginID] = true
				episodes[code]++
			}
		}
	}

	m.Tick()
	record("mix 3/3")

	// The mix moves three times, the way a shift of changeovers moves it.
	for i, mix := range []struct{ a, b int }{{5, 1}, {1, 5}, {3, 3}} {
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
			GroupNodeID: grpID, BinTypeID: types["S8-A"], Want: mix.a}), "set A")
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
			GroupNodeID: grpID, BinTypeID: types["S8-B"], Want: mix.b}), "set B")
		m.Tick()
		record(fmt.Sprintf("mix %d/%d (flip %d)", mix.a, mix.b, i+1))
	}

	total := asksByType["S8-A"] + asksByType["S8-B"]
	t.Logf("S8 BASELINE — 4 mixes over 8 positions: %d asks total (A=%d, B=%d) across %d "+
		"episode(s) (A=%d, B=%d). Every ask is one robot trip. Phase 3's strict sourcing has "+
		"to come in AT OR UNDER this number for the same mix sequence, or it is buying "+
		"correctness with throughput and the trade has to be stated.",
		total, asksByType["S8-A"], asksByType["S8-B"], episodes["S8-A"]+episodes["S8-B"],
		episodes["S8-A"], episodes["S8-B"])

	// The one property this measurement rests on: the episode per (group, type)
	// survives a mix change rather than churning, or the number above would be
	// measuring re-minting rather than carrier demand.
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	if len(open) > 2 {
		t.Errorf("%d open maintain episodes for a two-type group — the grain is (group, type), "+
			"so a mix change must move a level, not mint a new episode", len(open))
	}
}
