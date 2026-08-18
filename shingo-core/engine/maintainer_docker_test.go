//go:build docker

package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// The level keeper, end to end against a real database.
//
// These drive Tick() directly rather than waiting on the ticker: the cadence is
// not what is under test, the subtraction and the two edges are.

// mntFixture builds a maintained group with `slots` free positions and one
// declared level, and returns the group id and bin type id.
func mntFixture(t *testing.T, db *store.DB, group string, slots int, typeCode string, want int) (int64, int64) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, group)
	testutil.MustNoErr(t, err, "CreateGroup")
	for i := 0; i < slots; i++ {
		n := &nodes.Node{
			Name: group + "-P" + string(rune('A'+i)), Enabled: true, ParentID: &grpID,
		}
		testutil.MustNoErr(t, db.CreateNode(n), "create position")
	}
	var btID int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, typeCode).Scan(&btID), "bin type")

	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "test-core"), "station")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
		GroupNodeID: grpID, BinTypeID: btID, Want: want,
	}), "declare level")
	return grpID, btID
}

func mntState(t *testing.T, m *Maintainer, group, typeCode string) MaintainerGroupState {
	t.Helper()
	for _, s := range m.Snapshot() {
		if s.GroupNode == group && s.BinTypeCode == typeCode {
			return s
		}
	}
	t.Fatalf("no snapshot row for %s/%s in %+v", group, typeCode, m.Snapshot())
	return MaintainerGroupState{}
}

// The subtraction chain, and that a second tick does not re-ask.
//
// RE-RUNNING MUST BE HARMLESS — that property is what buys the absence of a
// debounce, a warm-up and an in-memory below-since map. If a second tick asked
// again, the keeper would rebuild the 241-duplicates incident at a new grain
// within one minute.
func TestMaintainer_SubtractsWhatItAlreadyAsked(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	mntFixture(t, db, "MNT-STEADY", 4, "MNT-45x58", 2)

	m := eng.Maintainer()
	m.Tick()

	st := mntState(t, m, "MNT-STEADY", "MNT-45x58")
	if st.Want != 2 || st.Resident != 0 {
		t.Fatalf("first tick: want=%d resident=%d, expected 2/0", st.Want, st.Resident)
	}
	if st.OriginID == "" {
		t.Fatal("first tick minted no episode despite a gap")
	}
	if st.Created != 2 {
		t.Fatalf("created=%d, want 2 — one ask per free typed slot, not one serial ask", st.Created)
	}

	// SECOND TICK: the asks it just made are now visible to CountLiveRootsByOrigin,
	// so the gap is closed by `asked` and nothing new is created.
	firstOrigin := st.OriginID
	m.Tick()
	st = mntState(t, m, "MNT-STEADY", "MNT-45x58")
	if st.Asked != 2 {
		t.Errorf("asked=%d, want 2 — the keeper cannot see the orders it created", st.Asked)
	}
	if st.Created != 0 {
		t.Errorf("second tick created %d more asks; re-running must be harmless", st.Created)
	}
	if st.Gap > 0 {
		t.Errorf("gap=%d after asking, want <= 0", st.Gap)
	}
	if st.OriginID != firstOrigin {
		t.Errorf("episode re-minted: %s -> %s; the open-key index is the duplicate guard",
			firstOrigin, st.OriginID)
	}
}

// THE SETTLE EDGE. An episode closes only when the level is met AND nothing is
// outstanding. Closing on the rising edge alone re-opens the duplicate window.
func TestMaintainer_ClosesOnTheSettleEdgeNotTheRisingEdge(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := mntFixture(t, db, "MNT-SETTLE", 4, "MNT-SET", 1)

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "MNT-SETTLE", "MNT-SET")
	if st.Created != 1 || st.OriginID == "" {
		t.Fatalf("setup: created=%d origin=%q, want one ask and an open episode", st.Created, st.OriginID)
	}
	origin := st.OriginID

	// A carrier ARRIVES — the level is met — but the ask is still live. The
	// rising edge is here and the episode must NOT close.
	children, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "list children")
	_, err = db.Exec(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'MNT-SET-BIN-1',$2,'available')`,
		btID, children[0].ID)
	testutil.MustNoErr(t, err, "land a carrier")

	m.Tick()
	st = mntState(t, m, "MNT-SETTLE", "MNT-SET")
	if st.Resident < 1 {
		t.Fatalf("resident=%d, want >= 1 — the fixture did not land a countable carrier", st.Resident)
	}
	if st.OriginID != origin {
		t.Fatalf("episode closed on the rising edge with an ask still live — that is the " +
			"duplicate window: level touches want, episode closes, level dips, a new origin " +
			"mints with a zero ask count and re-asks for carriers already coming")
	}

	// The ask goes terminal. NOW both conditions hold and the episode settles.
	_, err = db.Exec(`UPDATE orders SET status = $1 WHERE origin_id = $2`,
		string(protocol.StatusConfirmed), origin)
	testutil.MustNoErr(t, err, "terminalise the ask")

	m.Tick()
	st = mntState(t, m, "MNT-SETTLE", "MNT-SET")
	if st.OriginID != "" {
		t.Errorf("episode still open after the level settled and the ask went terminal: %s", st.OriginID)
	}
	openNow, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	for _, o := range openNow {
		if o.OriginID == origin {
			t.Errorf("episode %s is still open in the database", origin)
		}
	}
}

// CONFIG-WITHDRAWN closes as threshold_removed, not recovered. The need did not
// get its carriers — it stopped being watched.
func TestMaintainer_ConfigWithdrawnClosesAsRemoved(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := mntFixture(t, db, "MNT-WITHDRAW", 2, "MNT-WD", 1)

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "MNT-WITHDRAW", "MNT-WD")
	origin := st.OriginID
	if origin == "" {
		t.Fatal("setup: no episode opened")
	}

	// The operator deletes the level line.
	testutil.MustNoErr(t, db.RemoveMaintainLevel(grpID, btID), "remove level")

	m.Tick()
	openNow, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	for _, o := range openNow {
		if o.OriginID == origin {
			t.Fatalf("episode %s survived its level being deleted", origin)
		}
	}
	var reason string
	testutil.MustNoErr(t, db.QueryRow(
		`SELECT close_reason FROM demand_origins WHERE origin_id = $1`, origin).Scan(&reason),
		"read close reason")
	if reason != protocol.CloseReasonThresholdRemoved {
		t.Errorf("close reason = %q, want %q — reporting a satisfied demand when somebody "+
			"deletes a level is the conflation claim_removed exists to prevent",
			reason, protocol.CloseReasonThresholdRemoved)
	}
}

// A group with no maintenance_station is SKIPPED, not run. projectOrder no-ops
// on a blank StationID, so its asks would run on the floor and appear on no
// board anywhere — the phantom-order family.
func TestMaintainer_RefusesAGroupWithNoStation(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, _ := mntFixture(t, db, "MNT-NOSTATION", 2, "MNT-NS", 1)
	// Written directly, bypassing the save-time rules that refuse it.
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, ""), "blank the station")

	m := eng.Maintainer()
	m.Tick()

	for _, s := range m.Snapshot() {
		if s.GroupNode == "MNT-NOSTATION" {
			t.Fatalf("a stationless group was ticked: %+v", s)
		}
	}
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	if len(open) != 0 {
		t.Errorf("a stationless group minted %d episode(s); it must not run at all", len(open))
	}
}

// A group whose maintain_enabled is off is not the keeper's business at all.
// This is the whole live-capability gate: no shadow flag, just config.
func TestMaintainer_IgnoresUnmaintainedGroups(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, _ := mntFixture(t, db, "MNT-OFF", 2, "MNT-OFF-T", 2)
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "off"), "disable")

	m := eng.Maintainer()
	m.Tick()

	if len(m.Snapshot()) != 0 {
		t.Fatalf("an unmaintained group produced snapshot rows: %+v", m.Snapshot())
	}
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	if len(open) != 0 {
		t.Errorf("an unmaintained group minted %d episode(s)", len(open))
	}
}

// RESTART: the keeper holds no in-memory episode state, so a fresh Maintainer
// over the same database must reach the identical conclusion and create nothing
// new. This is the whole restart-duplication class, tested by construction.
func TestMaintainer_RestartMintsNoDuplicate(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	mntFixture(t, db, "MNT-RESTART", 3, "MNT-RS", 2)

	first := eng.Maintainer()
	first.Tick()
	before := mntState(t, first, "MNT-RESTART", "MNT-RS")
	if before.Created == 0 || before.OriginID == "" {
		t.Fatalf("setup: created=%d origin=%q", before.Created, before.OriginID)
	}

	// A brand-new keeper — the equivalent of a Core restart. It has never seen
	// this group and remembers nothing.
	restarted := NewMaintainer(eng, func() time.Time { return time.Now().UTC() })
	restarted.Tick()
	after := mntState(t, restarted, "MNT-RESTART", "MNT-RS")

	if after.OriginID != before.OriginID {
		t.Errorf("restart re-minted the episode: %s -> %s", before.OriginID, after.OriginID)
	}
	if after.Created != 0 {
		t.Errorf("restart created %d duplicate ask(s)", after.Created)
	}
	if after.Asked != before.Created {
		t.Errorf("restarted keeper counts asked=%d, first keeper created=%d — the subtraction "+
			"must be identical after a reboot", after.Asked, before.Created)
	}
	open, err := db.ListOpenEpisodesOfKind(protocol.EpisodeKindMaintain)
	testutil.MustNoErr(t, err, "list open")
	if len(open) != 1 {
		t.Errorf("open maintain episodes = %d, want exactly 1 after a restart", len(open))
	}
}
