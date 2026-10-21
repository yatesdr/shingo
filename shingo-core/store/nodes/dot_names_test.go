//go:build docker

package nodes_test

import (
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/nodes"
)

// TestGetByDotNames_AgreesWithGetByDotName pins the batched resolver to the
// single-name one: over the same rows and the same table of names, every name
// GetByDotName resolves is in GetByDotNames' map as the same node, and every
// name it answers sql.ErrNoRows for is absent. The table covers both arms and
// their corners: dotted, bare, a child asked for by its bare name, a wrong
// parent, a grandchild, a three-part name (split at the first dot only), a node
// whose own name holds a dot, an empty side of the dot, case, untrimmed space,
// an unknown name and a name asked twice. nodes.name is UNIQUE, so a bare name
// cannot match several rows; asking the same name twice is the duplicate a
// caller can send.
//
// The want column states which node each name resolves to, so a fixture that
// stops building what it says fails here rather than agreeing vacuously.
func TestGetByDotNames_AgreesWithGetByDotName(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB

	mk := func(name string, parent *nodes.Node) *nodes.Node {
		t.Helper()
		n := &nodes.Node{Name: name, Enabled: true}
		if parent != nil {
			n.ParentID = &parent.ID
		}
		testutil.MustNoErr(t, nodes.Create(sdb, n), "create "+name)
		return n
	}
	grp := mk("DNG-GRP", nil)
	c1 := mk("DNG-C1", grp)
	lane := mk("DNG-LANE", grp)
	s1 := mk("DNG-S1", lane)
	laneSlot := mk("DNG-LANE.DNG-S2", grp) // a child whose own name holds a dot
	mk("DNG-ODD.DNG-ONE", nil)             // a bare node whose name holds a dot
	oddParent := mk("DNG-ODD", nil)
	oddChild := mk("DNG-ONE", oddParent)

	cases := []struct {
		name string
		want *nodes.Node // nil: unknown
	}{
		{"DNG-GRP", grp},
		{"DNG-C1", c1},         // a child, asked bare
		{"DNG-GRP.DNG-C1", c1}, // a child, asked dotted
		{"DNG-GRP.DNG-LANE", lane},
		{"DNG-LANE.DNG-S1", s1},
		{"DNG-S1", s1},
		{"DNG-LANE.DNG-C1", nil},              // wrong parent
		{"DNG-GRP.DNG-S1", nil},               // a grandchild is not a child
		{"DNG-GRP.DNG-LANE.DNG-S1", nil},      // asks for a child named "DNG-LANE.DNG-S1"
		{"DNG-GRP.DNG-LANE.DNG-S2", laneSlot}, // which is how a dotted child name resolves
		{"DNG-ODD.DNG-ONE", oddChild},         // the dotted arm wins over a node of that name
		{".DNG-C1", nil},                      // empty parent
		{"DNG-GRP.", nil},                     // empty child
		{"dng-grp", nil},                      // names are case-sensitive
		{" DNG-GRP", nil},                     // the store does not trim; the handler does
		{"DNG-NOPE", nil},
		{"DNG-GRP.DNG-NOPE", nil},
		{"DNG-C1", c1}, // asked twice
	}

	names := make([]string, len(cases))
	for i, c := range cases {
		names[i] = c.name
	}
	batch, err := nodes.GetByDotNames(sdb, names)
	testutil.MustNoErr(t, err, "GetByDotNames")

	for _, c := range cases {
		one, err := nodes.GetByDotName(sdb, c.name)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			one = nil
		case err != nil:
			t.Fatalf("GetByDotName(%q): %v", c.name, err)
		}
		got, inBatch := batch[c.name]
		if (one == nil) == inBatch || (one != nil && !reflect.DeepEqual(one, got)) {
			t.Errorf("%q: GetByDotName = %+v, GetByDotNames = %+v (present %v)", c.name, one, got, inBatch)
		}
		switch {
		case c.want == nil && one != nil:
			t.Errorf("%q resolved to node %d %q, want unknown", c.name, one.ID, one.Name)
		case c.want != nil && (one == nil || one.ID != c.want.ID):
			t.Errorf("%q resolved to %+v, want node %d %q", c.name, one, c.want.ID, c.want.Name)
		}
	}
	for asked := range batch {
		found := false
		for _, n := range names {
			found = found || n == asked
		}
		if !found {
			t.Errorf("GetByDotNames answered %q, which was not asked", asked)
		}
	}

	empty, err := nodes.GetByDotNames(sdb, nil)
	testutil.MustNoErr(t, err, "GetByDotNames(nil)")
	if len(empty) != 0 {
		t.Errorf("GetByDotNames(nil) = %v, want empty", empty)
	}
}

// TestListPhysicalChildren_AgreesWithListChildren pins the batched children
// read to the children door's own (ListChildren, synthetic children dropped):
// for every parent asked, the same nodes in the same name order, and a parent
// with no physical child absent. The fixture names children out of insertion
// order, mixes a synthetic lane in, and asks a parent that has only a
// synthetic child and one that has none.
func TestListPhysicalChildren_AgreesWithListChildren(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	sdb := db.DB
	mk := func(name string, parent *nodes.Node, synthetic bool) *nodes.Node {
		t.Helper()
		n := &nodes.Node{Name: name, Enabled: true, IsSynthetic: synthetic}
		if parent != nil {
			n.ParentID = &parent.ID
		}
		testutil.MustNoErr(t, nodes.Create(sdb, n), "create "+name)
		return n
	}
	grp := mk("LPC-GRP", nil, true)
	mk("LPC-Z", grp, false)
	lane := mk("LPC-LANE", grp, true)
	mk("LPC-A", grp, false)
	mk("LPC-S1", lane, false)
	other := mk("LPC-OTHER", nil, false)
	mk("LPC-O1", other, false)
	onlySynthetic := mk("LPC-SYN", nil, true)
	mk("LPC-SYN-LANE", onlySynthetic, true)
	childless := mk("LPC-NONE", nil, false)

	parents := []int64{grp.ID, lane.ID, other.ID, onlySynthetic.ID, childless.ID}
	batch, err := nodes.ListPhysicalChildren(sdb, parents)
	testutil.MustNoErr(t, err, "ListPhysicalChildren")
	for _, p := range parents {
		all, err := nodes.ListChildren(sdb, p)
		testutil.MustNoErr(t, err, "ListChildren")
		var want []*nodes.Node
		for _, c := range all {
			if !c.IsSynthetic {
				want = append(want, c)
			}
		}
		if got, present := batch[p]; !reflect.DeepEqual(got, want) || present != (len(want) > 0) {
			t.Errorf("parent %d: batched %+v (present %v), the door reads %+v", p, got, present, want)
		}
	}
	if names := func() (out []string) {
		for _, c := range batch[grp.ID] {
			out = append(out, c.Name)
		}
		return out
	}(); !reflect.DeepEqual(names, []string{"LPC-A", "LPC-Z"}) {
		t.Errorf("the group's physical children = %v, want [LPC-A LPC-Z]", names)
	}
	if empty, err := nodes.ListPhysicalChildren(sdb, nil); err != nil || len(empty) != 0 {
		t.Errorf("no parents: %v, %v; want empty", empty, err)
	}
}
