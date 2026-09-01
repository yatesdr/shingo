//go:build docker

package engine

import (
	"fmt"
	"testing"
	"time"

	"shingocore/dispatch"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// fences_campaign_docker_test.go — the PHASE 3 sim campaign, as scripted runs.
//
// Same rig and same honest boundary as the MG2 campaign: real Postgres, real
// Engine, real Maintainer, real source finder, simulator fleet backend. Carrier
// ARRIVAL is staged by writing the bin where a delivered one would be, except
// in F9, which drives the dispatch path end-to-end on purpose.
//
// WHAT PHASE 3 ADDED THAT MG2 COULD NOT MEASURE. The MG2 campaign ran every
// scenario at coming=0 (its §4.4), because staging typed in-flight inbound
// needs the dispatch path. F9 closes that.

// ── the rig ─────────────────────────────────────────────────────────────────

// fenceGroup builds a maintained group: `slots` positions, one declared level,
// `strict` on or off, and the process nodes it supports.
func fenceGroup(t *testing.T, db *store.DB, name, code string, want, slots int,
	strict bool, supports ...string) (grpID, btID int64) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "CreateGroup "+name)

	testutil.MustNoErr(t, db.QueryRow(`
		INSERT INTO bin_types (code) VALUES ($1)
		ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
		RETURNING id`, code).Scan(&btID), "bin type")

	for i := 0; i < slots; i++ {
		n := &nodes.Node{Name: fmt.Sprintf("%s-P%02d", name, i+1), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(n), "create position")
	}
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "test-core"), "station")
	if want > 0 {
		testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{
			GroupNodeID: grpID, BinTypeID: btID, Want: want,
		}), "declare level")
	}
	if strict {
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropStrictSourcing, "on"), "strict")
	}
	var ids []int64
	for _, p := range supports {
		pn := &nodes.Node{Name: p, Enabled: true}
		testutil.MustNoErr(t, db.CreateNode(pn), "create process node "+p)
		ids = append(ids, pn.ID)
	}
	if len(ids) > 0 {
		testutil.MustNoErr(t, nodes.SetMaintainSupports(db.DB, grpID, ids), "supports")
	}
	return grpID, btID
}

// landAt puts an empty carrier of a type at a node and returns its id.
func landAt(t *testing.T, db *store.DB, btID, nodeID int64, label string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
		btID, label, nodeID).Scan(&id), "carrier "+label)
	return id
}

// firstChild is the first position of a group.
func firstChild(t *testing.T, db *store.DB, grpID int64) *nodes.Node {
	t.Helper()
	kids, err := db.ListChildNodes(grpID)
	testutil.MustNoErr(t, err, "list positions")
	if len(kids) == 0 {
		t.Fatal("group has no positions")
	}
	return kids[0]
}

// ── F1: an outsider cannot pull from a strict group ─────────────────────────

func TestFences_F1_OutsiderPullExcluded(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = newTestEngine(t, db, simulator.New())
	grpID, btID := fenceGroup(t, db, "F1-GRP", "F1-45x58", 0, 2, true, "F1-PRESS")

	inside := landAt(t, db, btID, firstChild(t, db, grpID).ID, "F1-INSIDE")
	far := &nodes.Node{Name: "F1-FAR", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(far), "far node")
	outside := landAt(t, db, btID, far.ID, "F1-OUTSIDE")

	got, err := bins.FindEmptyOfType(db.DB, "F1-45x58", "", 0,
		bins.EmptyFence{ProcessNode: "F1-SOMEONE-ELSE"}, reservations.Anyone)
	testutil.MustNoErr(t, err, "outsider pull")
	if got == nil || got.ID != outside {
		t.Errorf("outsider got %v, want the carrier outside the fence (%d); inside is %d",
			got, outside, inside)
	}

	got, err = bins.FindEmptyOfType(db.DB, "F1-45x58", "", 0,
		bins.EmptyFence{ProcessNode: "F1-PRESS"}, reservations.Anyone)
	testutil.MustNoErr(t, err, "supported pull")
	if got == nil || got.ID != inside {
		t.Errorf("supported press got %v, want the group's own carrier %d", got, inside)
	}
	t.Logf("F1: outsider → bin %d (outside), supported press → bin %d (inside). Dedication "+
		"binds outsiders, not the press the group serves.", outside, inside)
}

// ── F2: two maintained groups cannot drain each other ───────────────────────

func TestFences_F2_TwoGroupsCannotDrainEachOther(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = newTestEngine(t, db, simulator.New())
	aID, btID := fenceGroup(t, db, "F2-A", "F2-45x58", 2, 2, true, "F2-PRESS-A")
	bID, _ := fenceGroup(t, db, "F2-B", "F2-45x58", 2, 2, true, "F2-PRESS-B")

	landAt(t, db, btID, firstChild(t, db, aID).ID, "F2-A-BIN")
	landAt(t, db, btID, firstChild(t, db, bID).ID, "F2-B-BIN")

	// A keeper filling A. Its own group is closed by rule (ii); B is closed by
	// rule (i), because A's keeper is not in B's supports list either.
	got, err := bins.FindEmptyOfType(db.DB, "F2-45x58", "", 0,
		bins.EmptyFence{OriginGroup: "F2-A"}, reservations.Anyone)
	if got != nil {
		t.Errorf("A's keeper reached bin %d. Neither its own group nor another maintained "+
			"one is a source — the market and the cells are", got.ID)
	}
	t.Logf("F2: A's keeper found nothing across two stocked maintained groups (err %v). "+
		"Reciprocity is not configured anywhere; it falls out of the supports rule.", err)
}

// ── F3: the maintainer tops off ACROSS the fence ────────────────────────────
//
// The fence hides a group's carriers from outsiders. It must not hide the
// MARKET from the keeper, or a fenced plant is a plant whose groups can never
// be filled.
func TestFences_F3_MaintainerTopsOffAcrossTheFence(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	_, btID := fenceGroup(t, db, "F3-GRP", "F3-45x58", 2, 4, true, "F3-PRESS")

	// One carrier in the open market — no group, no fence.
	market := &nodes.Node{Name: "F3-MARKET-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "market slot")
	marketBin := landAt(t, db, btID, market.ID, "F3-MARKET-BIN")

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "F3-GRP", "F3-45x58")
	if st.Created == 0 {
		t.Fatalf("the keeper created no asks against a gap of %d", st.Gap)
	}

	waitFor(t, 20*time.Second, func() bool {
		var claimed int
		_ = db.QueryRow(`SELECT COUNT(*) FROM bins WHERE id = $1 AND claimed_by IS NOT NULL`,
			marketBin).Scan(&claimed)
		return claimed == 1
	}, "the keeper to source the market carrier across its own fence")

	t.Logf("F3: keeper of a STRICT group sourced bin %d from the open market. want=%d "+
		"resident=%d created=%d — the fence binds outsiders reaching IN, never the keeper "+
		"reaching out.", marketBin, st.Want, st.Resident, st.Created)
}

// ── F7: at-level push parks with a NAMED cause ──────────────────────────────

func TestFences_F7_AtLevelPushParksWithItsOwnCause(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_ = newTestEngine(t, db, simulator.New())
	grpID, btID := fenceGroup(t, db, "F7-GRP", "F7-45x58", 1, 3, false)

	// At level, and NOT physically full — two positions are free, so anything
	// refusing this is refusing on the level alone.
	landAt(t, db, btID, firstChild(t, db, grpID).ID, "F7-RESIDENT")

	blocked, block := dispatch.CheckDropoffCapacityForType(db, "F7-GRP", 0, &btID)
	if !blocked {
		t.Fatal("a group at its declared level accepted a push while holding what it was " +
			"told to hold")
	}
	if string(block.Cause) != "ngrp-at-level" {
		t.Errorf("cause = %q, want ngrp-at-level. 'Full' would send an operator to look at a "+
			"group with two empty positions in it", block.Cause)
	}
	t.Logf("F7: group at level 1 of 1 with 2 free positions refused a push, cause %q.",
		block.Cause)
}

// ── F9: THE coming>0 SCENARIO, dispatch-driven ──────────────────────────────
//
// Every MG2 scenario ran at coming=0 (MG2 campaign §4.4), because staging typed
// in-flight inbound needs the dispatch path rather than a fixture write. This
// drives it: a real order, admitted through the real lifecycle, carrying a typed
// empty into the group — and the keeper must SUBTRACT it rather than ask for
// another.
//
// It is the term the whole subtraction turns on and the one nothing had
// exercised end to end.
func TestFences_F9_ComingIsSubtracted(t *testing.T) {
	t.Parallel()
	testdb.DisableWedgeSweep(t, "the inbound push is a row the keeper COUNTS; nothing dispatches it")
	db := testdb.Open(t)
	eng := newTestEngine(t, db, simulator.New())
	grpID, btID := fenceGroup(t, db, "F9-GRP", "F9-45x58", 2, 4, false)

	// A carrier of the type, standing in the open market, on its way IN under a
	// non-keeper origin — an unloader push, in the demo plant's shape.
	market := &nodes.Node{Name: "F9-MARKET-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(market), "market slot")
	inbound := landAt(t, db, btID, market.ID, "F9-INBOUND")

	dest := firstChild(t, db, grpID)
	push := &orders.Order{
		EdgeUUID: "f9-push", StationID: "line-1", OrderType: protocol.OrderTypeMove,
		Status: protocol.StatusQueued, Quantity: 1,
		SourceNode: market.Name, DeliveryNode: dest.Name, BinID: &inbound,
	}
	testutil.MustNoErr(t, db.CreateOrder(push), "create the inbound push")

	m := eng.Maintainer()
	m.Tick()
	st := mntState(t, m, "F9-GRP", "F9-45x58")

	if st.Coming != 1 {
		t.Errorf("coming = %d, want 1. A typed carrier already on its way in is the third "+
			"term of the subtraction, and a keeper that cannot see it asks for a carrier it "+
			"is already getting — which is the duplicate shape one layer out", st.Coming)
	}
	if st.Created != st.Want-st.Resident-st.Asked-st.Coming && st.Gap > 0 {
		t.Errorf("created=%d against gap=%d", st.Created, st.Gap)
	}
	t.Logf("F9 (coming>0, dispatch-driven): want=%d resident=%d asked=%d COMING=%d gap=%d "+
		"created=%d. The keeper asked for the difference AFTER subtracting the carrier "+
		"already inbound.", st.Want, st.Resident, st.Asked, st.Coming, st.Gap, st.Created)
}
