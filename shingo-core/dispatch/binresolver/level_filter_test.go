package binresolver

import (
	"errors"
	"testing"

	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// level_filter_test.go — MG4-1. THE LEVEL IS A CAP, and this is the half that
// makes it one.
//
// The keeper tops UP to the declared number. Without a filter refusing a store
// PAST it, the level is only a floor: a group told to hold four would accept a
// fifth, a sixth, and every carrier anybody wanted to put down — which is how a
// press empty bank becomes the place the plant parks its overflow.

func levelGroup(id int64, name string) *nodes.Node {
	return &nodes.Node{ID: id, Name: name, Enabled: true, IsSynthetic: true}
}

// levelStore builds a fake with one group, one free child slot, and whatever
// levels and counts the case wants.
func levelStore(groupID int64, levels []nodes.MaintainLevel, counts map[groupKey]int) (*fakeStore, *nodes.Node) {
	f := newFakeStore()
	grp := levelGroup(groupID, "LVL-GRP")
	child := &nodes.Node{ID: groupID + 1, Name: "LVL-SLOT", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID] = grp
	f.nodes[child.ID] = child
	f.children[grp.ID] = []*nodes.Node{child}
	f.maintainLevels = map[int64][]nodes.MaintainLevel{groupID: levels}
	f.emptyCounts = counts
	return f, grp
}

// A GROUP AT ITS LEVEL REFUSES THE STORE — queue-on-full, not an error.
func TestLevelFilter_AtLevelRefuses(t *testing.T) {
	t.Parallel()
	bt := int64(7)
	f, grp := levelStore(100,
		[]nodes.MaintainLevel{{GroupNodeID: 100, BinTypeID: bt, BinTypeCode: "LVL-45x58", Want: 4}},
		map[groupKey]int{{100, "LVL-45x58"}: 4})

	r := &GroupResolver{DB: f}
	_, err := r.ResolveStore(grp, "", &bt, reservations.Anyone)
	if err == nil {
		t.Fatal("a group holding 4 against a level of 4 accepted a fifth carrier. The level " +
			"is a CAP; without this the keeper's number is only a floor")
	}
	// The wording matters: it is the phrase classifyResolutionError reads as
	// CAPACITY, which is what makes this queue-on-full rather than a failure.
	if got := err.Error(); got != "no available slot in node group LVL-GRP" {
		t.Errorf("err = %q, want the capacity phrasing. A different sentence would classify "+
			"as structural and FAIL the push instead of parking it", got)
	}
}

// BELOW LEVEL, the store proceeds exactly as before.
func TestLevelFilter_BelowLevelIsUnaffected(t *testing.T) {
	t.Parallel()
	bt := int64(7)
	f, grp := levelStore(200,
		[]nodes.MaintainLevel{{GroupNodeID: 200, BinTypeID: bt, BinTypeCode: "LVL-45x58", Want: 4}},
		map[groupKey]int{{200, "LVL-45x58"}: 3})

	r := &GroupResolver{DB: f}
	got, err := r.ResolveStore(grp, "", &bt, reservations.Anyone)
	if err != nil || got == nil || got.Node == nil {
		t.Fatalf("got %v, err %v — a group below its level takes the carrier", got, err)
	}
}

// PER-TYPE WHEN THE TYPE IS KNOWN. A group full of one declared type must still
// accept another: the levels are separate declarations and the cap is per
// declaration.
func TestLevelFilter_PerTypeWhenTheTypeIsKnown(t *testing.T) {
	t.Parallel()
	big, small := int64(7), int64(8)
	f, grp := levelStore(300, []nodes.MaintainLevel{
		{GroupNodeID: 300, BinTypeID: big, BinTypeCode: "LVL-BIG", Want: 4},
		{GroupNodeID: 300, BinTypeID: small, BinTypeCode: "LVL-SMALL", Want: 2},
	}, map[groupKey]int{
		{300, "LVL-BIG"}:   4, // met
		{300, "LVL-SMALL"}: 0, // short
	})

	r := &GroupResolver{DB: f}

	if _, err := r.ResolveStore(grp, "", &big, reservations.Anyone); err == nil {
		t.Error("the MET type was accepted — its own declaration is satisfied")
	}
	if got, err := r.ResolveStore(grp, "", &small, reservations.Anyone); err != nil || got == nil {
		t.Errorf("the SHORT type was refused (%v). One satisfied declaration must not fence "+
			"the group against every other type it was told to hold", err)
	}
}

// AN UNDECLARED TYPE IS NOT REFUSED. Declaring a level says "hold at least
// these"; it does not say "and nothing else may ever stand here".
func TestLevelFilter_UndeclaredTypeIsNotFenced(t *testing.T) {
	t.Parallel()
	declared, other := int64(7), int64(99)
	f, grp := levelStore(400,
		[]nodes.MaintainLevel{{GroupNodeID: 400, BinTypeID: declared, BinTypeCode: "LVL-D", Want: 1}},
		map[groupKey]int{{400, "LVL-D"}: 1})

	r := &GroupResolver{DB: f}
	if got, err := r.ResolveStore(grp, "", &other, reservations.Anyone); err != nil || got == nil {
		t.Errorf("an UNDECLARED type was refused (%v). A maintained group is a group with a "+
			"level on some types, not a group closed to every other", err)
	}
}

// GROUP-TOTAL WHEN THE TYPE IS UNKNOWN, and deliberately the LOOSER reading.
//
// An untyped store cannot say which declaration it would fill, so the only
// honest cap is the sum. Refusing whenever ANY single declaration is met would
// turn one satisfied type into a fence against every other, and an untyped push
// has done nothing to deserve that.
func TestLevelFilter_UntypedUsesTheGroupTotal(t *testing.T) {
	t.Parallel()
	big, small := int64(7), int64(8)
	levels := []nodes.MaintainLevel{
		{GroupNodeID: 500, BinTypeID: big, BinTypeCode: "LVL-BIG", Want: 4},
		{GroupNodeID: 500, BinTypeID: small, BinTypeCode: "LVL-SMALL", Want: 2},
	}

	// One declaration met, the other short: total 4 of 6. The looser reading
	// takes the carrier.
	f, grp := levelStore(500, levels, map[groupKey]int{
		{500, "LVL-BIG"}: 4, {500, "LVL-SMALL"}: 0,
	})
	if got, err := (&GroupResolver{DB: f}).ResolveStore(grp, "", nil, reservations.Anyone); err != nil || got == nil {
		t.Errorf("untyped store refused at 4 of a 6 total (%v) — the strict reading would "+
			"let one satisfied type fence the whole group", err)
	}

	// Total met: refused.
	f2, grp2 := levelStore(500, levels, map[groupKey]int{
		{500, "LVL-BIG"}: 4, {500, "LVL-SMALL"}: 2,
	})
	if _, err := (&GroupResolver{DB: f2}).ResolveStore(grp2, "", nil, reservations.Anyone); err == nil {
		t.Error("untyped store accepted at 6 of a 6 total — the cap is the sum when the " +
			"caller cannot say which declaration it fills")
	}
}

// A GROUP WITH NO DECLARED LEVEL IS UNTOUCHED, which is every group in every
// plant today.
func TestLevelFilter_UnmaintainedGroupIsANoOp(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	grp := levelGroup(600, "PLAIN-GRP")
	child := &nodes.Node{ID: 601, Name: "PLAIN-SLOT", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID] = grp
	f.nodes[child.ID] = child
	f.children[grp.ID] = []*nodes.Node{child}

	if got, err := (&GroupResolver{DB: f}).ResolveStore(grp, "", nil, reservations.Anyone); err != nil || got == nil {
		t.Fatalf("a group with no declared level was affected: got %v err %v", got, err)
	}
}

// A READ FAILURE REFUSES, and the error PROPAGATES rather than becoming
// "not full".
//
// The direction is chosen. Allowing on a failed read overfills a group past a
// cap somebody set, and nothing later corrects it; refusing means the push parks
// and retries. And swallowing the error into a boolean is the MG3-1a collapse in
// a new place — a read that did not happen reported as an answer.
func TestLevelFilter_ReadFailureRefusesAndPropagates(t *testing.T) {
	t.Parallel()
	bt := int64(7)
	for _, tc := range []struct {
		name  string
		spoil func(*fakeStore)
	}{
		{"the level read fails", func(f *fakeStore) { f.maintainLevelsErr = errors.New("boom") }},
		{"the count read fails", func(f *fakeStore) { f.emptyCountErr = errors.New("boom") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, grp := levelStore(700,
				[]nodes.MaintainLevel{{GroupNodeID: 700, BinTypeID: bt, BinTypeCode: "LVL-X", Want: 4}},
				map[groupKey]int{{700, "LVL-X"}: 0})
			tc.spoil(f)

			got, err := (&GroupResolver{DB: f}).ResolveStore(grp, "", &bt, reservations.Anyone)
			if err == nil || got != nil {
				t.Fatalf("got %v err %v — a level that could not be read is not a level that "+
					"said yes", got, err)
			}
			if err.Error() == "no available slot in node group LVL-GRP" {
				t.Error("a failed read was reported as CAPACITY. That is the MG3-1a collapse " +
					"in a new place: a read that did not happen, reported as an answer about " +
					"the plant")
			}
		})
	}
}
