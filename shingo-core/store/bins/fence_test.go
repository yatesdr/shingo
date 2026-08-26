//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"shingocore/store/reservations"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// fence_test.go — MG3-1. THE TWO RULES THAT HIDE A CARRIER, at the query.
//
// A carrier is hidden from a plant-wide empty search when its node sits under a
// maintained group G and EITHER:
//
//	(i)  G is strict and the asker is not supported at G — THE FENCE;
//	(ii) G is the group the ask exists to FILL — MG2-11's rule.
//
// They are two rules and not one, and the difference is what the earlier
// contradiction turned on: the fence asks "are you an outsider here?" — a level
// keeper is not, at its own group — while rule (ii) asks "are you filling this
// group?". The keeper is exempt from (i) at its own group and caught by (ii)
// there anyway. Net: the keeper sources from the market and the cells and never
// from any maintained group; a supported press reaches its own group through
// the supports list.
//
// THIS FILE CARRIES MG2-11's TESTS. TestFindEmptyOfTypeOutsideGroup asserted
// rule (ii) against a finder that no longer exists — the rule survived, its
// spelling did not. Behaviour preserved, assertions carried, one spelling.

// fenceFixture builds a maintained group holding one empty carrier of `code`,
// plus an identical carrier OUTSIDE it. Returns the two bin ids.
//
// THE OUTSIDE CARRIER IS NOT DECORATION. Every assertion below is of the form
// "which one came back", and a fixture with only the fenced carrier would let a
// broken query pass by returning nothing for the wrong reason — the MG2-11
// lesson, applied to its own replacement.
func fenceFixture(t *testing.T, db *store.DB, prefix, code string, strict bool) (grpID, inside, outside int64) {
	t.Helper()
	sdb := db.DB

	grpID, err := nodes.CreateGroup(sdb, prefix+"-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	// UPSERT, because the reciprocity case builds TWO groups holding the SAME
	// carrier type — which is the whole shape it is testing, not an accident of
	// the fixture.
	var btID int64
	testutil.MustNoErr(t, sdb.QueryRow(`
		INSERT INTO bin_types (code) VALUES ($1)
		ON CONFLICT (code) DO UPDATE SET code = EXCLUDED.code
		RETURNING id`, code).Scan(&btID), "bin type")

	slot := &nodes.Node{Name: prefix + "-POS", Enabled: true, ParentID: &grpID}
	testutil.MustNoErr(t, nodes.Create(sdb, slot), "create position")
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
		btID, prefix+"-INSIDE", slot.ID).Scan(&inside), "inside carrier")

	far := &nodes.Node{Name: prefix + "-FAR", Enabled: true}
	testutil.MustNoErr(t, nodes.Create(sdb, far), "create outside node")
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available') RETURNING id`,
		btID, prefix+"-OUTSIDE", far.ID).Scan(&outside), "outside carrier")

	if strict {
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropStrictSourcing, "on"), "strict on")
	}
	// The inside carrier has the LOWER id, so an unfenced search prefers it and
	// every assertion below discriminates.
	if inside >= outside {
		t.Fatalf("fixture: inside id %d does not precede outside %d", inside, outside)
	}
	return grpID, inside, outside
}

// supportProcess registers a process node as supported by the group.
func supportProcess(t *testing.T, db *store.DB, grpID int64, name string) {
	t.Helper()
	p := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, nodes.Create(db.DB, p), "create process node")
	testutil.MustNoErr(t, nodes.SetMaintainSupports(db.DB, grpID, []int64{p.ID}), "set supports")
}

// ── The default: SHARING ────────────────────────────────────────────────────

// A ZERO FENCE FENCES NOTHING, and a non-strict group fences nothing either.
//
// This is the ruling that governs everything else: plant-wide sharing stays the
// default everywhere, and the only fenced zones are maintained groups with
// strict_sourcing on. Derek added the sharing deliberately, to keep lines
// running rather than run pure-strict, and phase 3 does not take it away.
func TestFence_SharingIsTheDefault(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, inside, _ := fenceFixture(t, db, "SHARE", "SHARE-45x58", false)

	for _, tc := range []struct {
		name  string
		fence bins.EmptyFence
	}{
		{"no fence at all", bins.EmptyFence{}},
		{"an asker with a process, group NOT strict", bins.EmptyFence{ProcessNode: "SHARE-OUTSIDER"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := bins.FindEmptyOfType(db.DB, "SHARE-45x58", "", 0, tc.fence, reservations.Anyone)
			testutil.MustNoErr(t, err, "find")
			if got == nil || got.ID != inside {
				t.Errorf("got %v, want the group's carrier %d. Sharing is the plant default; "+
					"a group nobody fenced is not a fence", got, inside)
			}
		})
	}
}

// ── Rule (i): the fence ─────────────────────────────────────────────────────

func TestFence_StrictGroupHidesFromOutsidersAndNotFromSupported(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, inside, outside := fenceFixture(t, db, "STRICT", "STRICT-45x58", true)
	supportProcess(t, db, grpID, "STRICT-PRESS-1")

	t.Run("an outsider cannot see it", func(t *testing.T) {
		got, err := bins.FindEmptyOfType(db.DB, "STRICT-45x58", "", 0,
			bins.EmptyFence{ProcessNode: "STRICT-SOMEONE-ELSE"}, reservations.Anyone)
		testutil.MustNoErr(t, err, "find")
		if got == nil || got.ID != outside {
			t.Errorf("got %v, want the carrier OUTSIDE the fence (%d). A strict group's "+
				"empties are reserved for the processes it supports — an outsider's "+
				"plant-wide scan must not see them", got, outside)
		}
	})

	t.Run("a supported press can", func(t *testing.T) {
		got, err := bins.FindEmptyOfType(db.DB, "STRICT-45x58", "", 0,
			bins.EmptyFence{ProcessNode: "STRICT-PRESS-1"}, reservations.Anyone)
		testutil.MustNoErr(t, err, "find")
		if got == nil || got.ID != inside {
			t.Errorf("got %v, want the group's own carrier %d. Dedication binds OUTSIDERS, "+
				"not the press the group exists to serve", got, inside)
		}
	})

	t.Run("a nameless asker is an outsider", func(t *testing.T) {
		// Blank ProcessNode means "supported nowhere", which is the safe reading
		// of an ask that names no process.
		got, err := bins.FindEmptyOfType(db.DB, "STRICT-45x58", "", 0,
			bins.EmptyFence{OriginGroup: "STRICT-NOT-THIS-GROUP"}, reservations.Anyone)
		testutil.MustNoErr(t, err, "find")
		if got == nil || got.ID != outside {
			t.Errorf("got %v, want %d — an ask naming no process cannot be supported "+
				"anywhere, and the safe reading of that is 'not for you'", got, outside)
		}
	})
}

// A GROUP THAT SUPPORTS NOBODY FENCES EVERYBODY, which is right rather than
// degenerate: it is a group in the middle of being configured.
func TestFence_StrictWithNoSupportsFencesEveryone(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, _, outside := fenceFixture(t, db, "NOSUP", "NOSUP-45x58", true)

	got, err := bins.FindEmptyOfType(db.DB, "NOSUP-45x58", "", 0,
		bins.EmptyFence{ProcessNode: "NOSUP-ANYONE"}, reservations.Anyone)
	testutil.MustNoErr(t, err, "find")
	if got == nil || got.ID != outside {
		t.Errorf("got %v, want %d. 'I have not said who this is for' reads as 'not for "+
			"you' — the alternative is a half-configured fence that leaks", got, outside)
	}
}

// ── Rule (ii): MG2-11, carried ──────────────────────────────────────────────

// A TOP-OFF ASK MAY NOT SOURCE FROM THE GROUP IT IS FILLING.
//
// Carried from TestFindEmptyOfTypeOutsideGroup, which asserted this against a
// finder MG3-1 deleted. The measured behaviour it protects: a six-position
// group standing at 2 of a level of 4 dispatched both its top-off asks against
// its OWN carriers, moving them between its own positions. The claims dropped
// `resident`, which re-opened the gap, which asked again — the group shuffled
// itself and never reached its level.
func TestFence_KeeperNeverSourcesFromTheGroupItIsFilling(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, inside, outside := fenceFixture(t, db, "SELF", "SELF-45x58", false)

	// NOT STRICT, deliberately. Filling a group from itself is a null trip
	// whether or not anybody has fenced it, so rule (ii) does not consult
	// strict_sourcing.
	got, err := bins.FindEmptyOfType(db.DB, "SELF-45x58", "", 0,
		bins.EmptyFence{OriginGroup: "SELF-GRP"}, reservations.Anyone)
	testutil.MustNoErr(t, err, "find")
	if got == nil || got.ID != outside {
		t.Errorf("got %v, want the carrier outside the group (%d). Sourcing from the group "+
			"being filled moves a carrier from one of its positions to another and claims "+
			"it on the way, which drops the count that decides whether to ask again",
			got, outside)
	}
	_ = inside
}

// AND NOTHING ELSE IS HIDDEN — the exclusion removes the group and nothing more.
//
// An exclusion that also hid the rest of the plant would park every top-off ask
// forever, which looks like a dry market and is not one.
func TestFence_ExcludesTheGroupAndNothingElse(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, inside, outside := fenceFixture(t, db, "NARROW", "NARROW-45x58", true)

	// Remove the outside candidate: now the ONLY carrier is the fenced one, and
	// the honest answer is none-found rather than a carrier from the fence.
	_, err := db.Exec(`DELETE FROM bins WHERE id = $1`, outside)
	testutil.MustNoErr(t, err, "remove the outside carrier")

	got, ferr := bins.FindEmptyOfType(db.DB, "NARROW-45x58", "", 0,
		bins.EmptyFence{ProcessNode: "NARROW-OUTSIDER"}, reservations.Anyone)
	if got != nil {
		t.Fatalf("returned carrier %d from inside the fence (%d)", got.ID, inside)
	}
	if !errors.Is(ferr, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows. A fenced-out search that ran and matched "+
			"nothing must say so in the family's language — MG3-1a's rule applies to the "+
			"new query too, and a fence CTE is exactly where a broken one would hide", ferr)
	}
}

// ── Reciprocity, and nesting ────────────────────────────────────────────────

// TWO MAINTAINED GROUPS CANNOT DRAIN EACH OTHER, and nothing had to be written
// to arrange it: a keeper topping up A is not in B's supports list either, so
// it is an outsider at B by the same rule that makes a press an outsider at A.
func TestFence_ReciprocityFallsOutOfTheSameRule(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	_, aInside, aOutside := fenceFixture(t, db, "RECIP-A", "RECIP-45x58", true)
	_, bInside, bOutside := fenceFixture(t, db, "RECIP-B", "RECIP-45x58", true)

	// THE UNFENCED CARRIERS GO. fenceFixture lands one outside each group so the
	// other cases can discriminate "took the fenced one" from "found nothing";
	// here they would answer the question for the wrong reason, because a
	// carrier standing in no group at all is sourceable by everybody and proves
	// nothing about reciprocity.
	_, err := db.Exec(`DELETE FROM bins WHERE id IN ($1, $2)`, aOutside, bOutside)
	testutil.MustNoErr(t, err, "remove the unfenced carriers")

	// A keeper filling A, looking for a carrier. B is strict and does not
	// support it; A is its own group. Both are closed to it.
	got, err := bins.FindEmptyOfType(db.DB, "RECIP-45x58", "", 0,
		bins.EmptyFence{OriginGroup: "RECIP-A-GRP"}, reservations.Anyone)
	if got != nil {
		t.Errorf("got carrier %d (A holds %d, B holds %d). A keeper must reach neither its "+
			"own group nor another maintained one — the market and the cells are where it "+
			"sources", got.ID, aInside, bInside)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows", err)
	}
}

// NESTING IS CLOSED BY THE WALK. A carrier inside a group inside a fenced group
// is fenced, because the exclusion is a descendant walk from the fenced roots
// rather than a check against the group's direct children.
func TestFence_ClosesNesting(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	grpID, _, outside := fenceFixture(t, db, "NEST", "NEST-45x58", true)

	// A group inside the fenced group, with a carrier in it.
	innerID, err := nodes.CreateGroup(sdb, "NEST-INNER")
	testutil.MustNoErr(t, err, "inner group")
	_, err = sdb.Exec(`UPDATE nodes SET parent_id = $1 WHERE id = $2`, grpID, innerID)
	testutil.MustNoErr(t, err, "nest the inner group")

	var btID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`SELECT id FROM bin_types WHERE code = 'NEST-45x58'`).Scan(&btID), "bin type")
	innerSlot := &nodes.Node{Name: "NEST-INNER-POS", Enabled: true, ParentID: &innerID}
	testutil.MustNoErr(t, nodes.Create(sdb, innerSlot), "inner position")
	var nested int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'NEST-NESTED',$2,'available') RETURNING id`,
		btID, innerSlot.ID).Scan(&nested), "nested carrier")

	// Remove the group's own direct carrier, so the NESTED one is the only thing
	// the fence could leak.
	_, err = sdb.Exec(`DELETE FROM bins WHERE label = 'NEST-INSIDE'`)
	testutil.MustNoErr(t, err, "remove the direct carrier")

	got, ferr := bins.FindEmptyOfType(sdb, "NEST-45x58", "", 0,
		bins.EmptyFence{ProcessNode: "NEST-OUTSIDER"}, reservations.Anyone)
	testutil.MustNoErr(t, ferr, "find")
	if got == nil || got.ID != outside {
		t.Errorf("got %v, want the carrier outside the fence (%d) — the nested carrier %d "+
			"leaked. Membership is in ANY maintained ancestor, which is what a descendant "+
			"walk from the fenced roots answers and what a direct-children check does not",
			got, outside, nested)
	}
}

// ── The count stays physical ────────────────────────────────────────────────

// NO FENCE EVER ENTERS A COUNT, and there is no way to pass one — the count
// takes no fence argument at all.
//
// The reasons are asymmetric in duration and both are about permanence. A count
// that hid a fenced carrier would report the group short of carriers it is
// standing on, and the keeper would order more — permanent overfill that
// nothing cancels. And it would make the level depend on WHO IS ASKING, which
// is not a property a level has.
func TestFence_CountIsUnfenced(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	grpID, _, _ := fenceFixture(t, db, "PHYS", "PHYS-45x58", true)

	n, err := bins.CountEmptyOfTypeInGroup(db.DB, "PHYS-45x58", grpID)
	testutil.MustNoErr(t, err, "count")
	if n != 1 {
		t.Errorf("count = %d, want 1. The level is PHYSICAL: how many carriers are standing "+
			"there, not how many some particular asker is allowed to take", n)
	}
}
