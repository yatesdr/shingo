//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// THE SUBTREE EXCLUSION, at the SQL.
//
// FindEmptyOfTypeOutsideGroup is what stops a maintained group's top-off ask
// from sourcing a carrier already standing in that group. The engine-level
// regression test proves the behaviour in situ; this one pins the query, which
// is where the two ways of getting it wrong live: excluding the wrong set, and
// excluding nothing.
func TestFindEmptyOfTypeOutsideGroup(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	// The maintained group: two positions, each holding a carrier of the type.
	grpID, err := nodes.CreateGroup(sdb, "OUT-GRP")
	testutil.MustNoErr(t, err, "CreateGroup")
	var btID int64
	testutil.MustNoErr(t, sdb.QueryRow(
		`INSERT INTO bin_types (code) VALUES ('OUT-45x58') RETURNING id`).Scan(&btID), "bin type")

	for _, name := range []string{"OUT-P01", "OUT-P02"} {
		slot := &nodes.Node{Name: name, Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, nodes.Create(sdb, slot), "create "+name)
		_, err = sdb.Exec(
			`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,$2,$3,'available')`,
			btID, "BIN-"+name, slot.ID)
		testutil.MustNoErr(t, err, "carrier at "+name)
	}

	// With no exclusion the finder happily returns one of the group's own.
	got, err := bins.FindEmptyOfTypeOutsideGroup(sdb, "OUT-45x58", "", 0, 0)
	testutil.MustNoErr(t, err, "unexcluded find")
	if got == nil {
		t.Fatal("fixture: the unexcluded search found nothing, so the exclusion below " +
			"would pass for the wrong reason")
	}
	t.Logf("unexcluded search returned %s — this is what the keeper used to get", got.Label)

	// EXCLUDED: nothing outside the group, so nothing at all.
	got, err = bins.FindEmptyOfTypeOutsideGroup(sdb, "OUT-45x58", "", grpID, 0)
	if got != nil {
		t.Errorf("returned %s from INSIDE the excluded group. Every carrier of this type is "+
			"in the group, so the only correct answer is none — a top-off ask that sources "+
			"here moves a carrier from one of the group's positions to another and claims it "+
			"on the way, which drops the count that decides whether to ask again", got.Label)
	}
	// STRICTLY sql.ErrNoRows, not merely non-nil. A broken query also returns a
	// non-nil error and a nil bin, and the source finder treats ANY error as
	// "no empty found" — so a test that accepted any error here would pass just
	// as happily against SQL that never runs. That is not hypothetical: the
	// first version of this query said `FROM subtree` when the CTE is named
	// `descendants`, and every caller looked correct while the query threw.
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("err = %v, want sql.ErrNoRows. Any other error means the query FAILED, and "+
			"a failed query is indistinguishable from an empty result at every call site", err)
	}

	// One carrier OUTSIDE, in its own group. That one is eligible.
	outID, err := nodes.CreateGroup(sdb, "OUT-ELSEWHERE")
	testutil.MustNoErr(t, err, "elsewhere group")
	far := &nodes.Node{Name: "OUT-FAR", Enabled: true, ParentID: &outID}
	testutil.MustNoErr(t, nodes.Create(sdb, far), "create OUT-FAR")
	_, err = sdb.Exec(
		`INSERT INTO bins (bin_type_id, label, node_id, status) VALUES ($1,'BIN-OUT-FAR',$2,'available')`,
		btID, far.ID)
	testutil.MustNoErr(t, err, "carrier elsewhere")

	got, err = bins.FindEmptyOfTypeOutsideGroup(sdb, "OUT-45x58", "", grpID, 0)
	testutil.MustNoErr(t, err, "excluded find with an outside candidate")
	if got == nil || got.Label != "BIN-OUT-FAR" {
		t.Fatalf("got %v, want BIN-OUT-FAR. The exclusion must remove the group and nothing "+
			"else — an exclusion that also hid the rest of the plant would park every "+
			"top-off ask forever, which looks like a dry market and is not one", got)
	}

	// A ZERO SUBTREE DELEGATES, and must behave exactly like FindEmptyOfType. One
	// spelling of the unexcluded query, not two that can drift.
	a, aerr := bins.FindEmptyOfTypeOutsideGroup(sdb, "OUT-45x58", "", 0, 0)
	b, berr := bins.FindEmptyOfType(sdb, "OUT-45x58", "", 0)
	testutil.MustNoErr(t, aerr, "delegating find")
	testutil.MustNoErr(t, berr, "direct find")
	if a == nil || b == nil || a.Label != b.Label {
		t.Errorf("zero-subtree gave %v, FindEmptyOfType gave %v — the delegation is what "+
			"keeps one spelling of the unexcluded query", a, b)
	}
}
