package binresolver

import (
	"errors"
	"testing"
	"time"

	"shingocore/domain"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// slot_child_carrier_rule_test.go — a group's flat slot children ask the part's
// carrier rule, as the lane mouth asks it in SQL (ground rule 6). The scan reads
// the rule once, at its first flat child holding bins (Store.LoadBinTypeRule);
// a read failure skips the flat children.

// slotChildGroup is a group of two flat slot children: the older carrier, of a
// type the part may not ride (8), at the first; the part's own type (7) at the
// second.
func slotChildGroup() (*fakeStore, *nodes.Node, *bins.Bin) {
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	a, b := directChild(11, "slot-a"), directChild(12, "slot-b")
	f.children[group.ID] = []*nodes.Node{a, b}
	now := time.Now()
	foreign := availBin(1000, "P1", now.Add(-time.Hour))
	foreign.BinTypeID = 8
	own := availBin(1001, "P1", now)
	own.BinTypeID = 7
	f.bins[a.ID] = []*bins.Bin{foreign}
	f.bins[b.ID] = []*bins.Bin{own}
	f.rules = map[string]domain.BinTypeRule{"P1": domain.NewBinTypeRule([]int64{7}, nil)}
	return f, group, own
}

func TestGroupScan_ASlotChildAsksThePartsCarrierRule(t *testing.T) {
	t.Parallel()
	f, group, own := slotChildGroup()
	got, err := (&GroupResolver{DB: f}).ResolveRetrieve(group, "P1", reservations.Anyone, nil)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Bin != own {
		t.Errorf("group scan took bin %d, want %d: the older carrier is a type P1 may not ride", got.Bin.ID, own.ID)
	}
	if f.ruleReads != 1 {
		t.Errorf("rule reads = %d, want 1: once per scan, at the first flat child holding bins", f.ruleReads)
	}
}

func TestGroupScan_ReadsTheRuleOnlyWhereAFlatChildHoldsBins(t *testing.T) {
	t.Parallel()
	f := newFakeStore()
	group := ngrpNode(1, "grp")
	lane, empty := laneChild(10, "lane"), directChild(11, "empty-slot")
	f.children[group.ID] = []*nodes.Node{lane, empty}
	slot := slotInLane(100, "lane-1")
	f.nodes[slot.ID] = slot
	mouth := availBin(1000, "P1", time.Now())
	attachSlot(mouth, slot)
	f.sourceInLane[lane.ID] = mouth
	got, err := (&GroupResolver{DB: f}).ResolveRetrieve(group, "P1", reservations.Anyone, nil)
	if err != nil || got.Bin != mouth {
		t.Fatalf("resolve = %+v, %v; want the lane's mouth bin", got, err)
	}
	if f.ruleReads != 0 {
		t.Errorf("rule reads = %d, want 0: the lane mouth asks the rule in SQL, and no flat child holds a bin", f.ruleReads)
	}
}

func TestGroupScan_ARuleReadFailureSkipsTheFlatChildren(t *testing.T) {
	t.Parallel()
	f, group, _ := slotChildGroup()
	f.ruleErr = errors.New("payload_bin_types unreadable")
	got, err := (&GroupResolver{DB: f}).ResolveRetrieve(group, "P1", reservations.Anyone, nil)
	if err == nil {
		t.Fatalf("resolve took bin %d at %s, want none: a slot child is never judged without the rule",
			got.Bin.ID, got.Node.Name)
	}
	if f.ruleReads != 1 {
		t.Errorf("rule reads = %d, want 1: a failed read is not retried per child", f.ruleReads)
	}
}
