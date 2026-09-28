package binresolver

import (
	"testing"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/payloads"
	"shingocore/store/reservations"
)

// vacated_store_test.go — ResolveStoreVacated, the vacated-slot rule's NGRP arm.
//
// It must refuse everything ResolveStore would refuse EXCEPT the one bin a
// committed lift takes off the child. Each case below changes one fact from the
// admitted fixture and expects the refusal ResolveStore's own fence gives.

const (
	vsGroup  = int64(500)
	vsChild  = int64(501)
	vsBin    = int64(90)
	vsType   = int64(7)
	vsTypeC  = "VS-T"
	vsAsker  = int64(42)
	vsOthers = int64(43)
)

func vacatedStore() (*fakeStore, *nodes.Node, *nodes.Node) {
	f := newFakeStore()
	grp := &nodes.Node{ID: vsGroup, Name: "VS-GRP", Enabled: true, IsSynthetic: true, NodeTypeCode: protocol.NodeClassNGRP}
	child := &nodes.Node{ID: vsChild, Name: "VS-C1", Enabled: true, ParentID: &grp.ID}
	f.nodes[grp.ID], f.nodes[child.ID] = grp, child
	f.children[grp.ID] = []*nodes.Node{child}
	f.bins[child.ID] = []*bins.Bin{{ID: vsBin, NodeID: &child.ID}}
	f.emptiesAt = map[groupKey]int{}
	f.activeByOrder = map[string]int64{}
	return f, grp, child
}

func askAs() reservations.DigAsker { return reservations.AskerFor(vsAsker, vsAsker) }

func resolveVacated(f *fakeStore, grp, child *nodes.Node, payload string, stated BinTypeStatement) error {
	r := &GroupResolver{DB: f}
	_, err := r.ResolveStoreVacated(grp, child, []int64{vsBin}, payload, stated, askAs())
	return err
}

func TestResolveStoreVacated_AdmitsTheVacatedChild(t *testing.T) {
	t.Parallel()
	f, grp, child := vacatedStore()
	r := &GroupResolver{DB: f}
	got, err := r.ResolveStoreVacated(grp, child, []int64{vsBin}, "", NoBinType, askAs())
	if err != nil || got == nil || got.Node.ID != child.ID {
		t.Fatalf("got %v err %v — the only bin on the child is the one the lift takes, so it is free", got, err)
	}
}

// P6 — each fence ResolveStore asks, asked here too.
func TestResolveStoreVacated_Refusals(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name  string
		setup func(f *fakeStore, child *nodes.Node)
		pay   string
		typ   BinTypeStatement
	}{
		{"a second bin no lift takes (N holds 2 bins)", func(f *fakeStore, c *nodes.Node) {
			f.bins[c.ID] = append(f.bins[c.ID], &bins.Bin{ID: vsBin + 1, NodeID: &c.ID})
		}, "", NoBinType},
		{"another order inbound", func(f *fakeStore, c *nodes.Node) {
			f.activeByDelivery[c.Name] = 1
			f.activeByOrder[c.Name] = vsOthers
		}, "", NoBinType},
		{"claimed by another order", func(f *fakeStore, c *nodes.Node) {
			o := vsOthers
			c.ClaimedBy = &o
		}, "", NoBinType},
		{"disabled", func(f *fakeStore, c *nodes.Node) { c.Enabled = false }, "", NoBinType},
		{"a lane slot", func(f *fakeStore, c *nodes.Node) { c.NodeTypeCode = protocol.NodeClassLANE }, "", NoBinType},
		{"payload fence", func(f *fakeStore, c *nodes.Node) {
			f.effPayloads[c.ID] = []*payloads.Payload{{Code: "SYN-OTHER"}}
		}, "SYN-PART", NoBinType},
		{"bin-type fence", func(f *fakeStore, c *nodes.Node) {
			f.effBinTypes[c.ID] = []*bins.BinType{{ID: vsType + 2}}
		}, "", KnownBinType(vsType)},
		{"a payload into an empties-only group", func(f *fakeStore, c *nodes.Node) {
			f.maintainLevels = map[int64][]nodes.MaintainLevel{vsGroup: {{GroupNodeID: vsGroup, BinTypeID: vsType, BinTypeCode: vsTypeC, Want: 9}}}
		}, "SYN-PART", KnownBinType(vsType)},
	}
	for _, c := range cases {
		f, grp, child := vacatedStore()
		c.setup(f, child)
		if err := resolveVacated(f, grp, child, c.pay, c.typ); err == nil {
			t.Errorf("%s: admitted — ResolveStore's own fence refuses this, and so must the vacated arm", c.name)
		}
	}
	// The asker's own traffic is not traffic.
	f, grp, child := vacatedStore()
	f.activeByDelivery[child.Name] = 1
	f.activeByOrder[child.Name] = vsAsker
	if err := resolveVacated(f, grp, child, "", NoBinType); err != nil {
		t.Errorf("the only order inbound is the asker itself, and it was counted: %v", err)
	}
}

// The maintained level, all three lifts. Levels count EMPTY carriers of a type,
// so one-out-one-in is level-neutral only when the lifted carrier is an empty of
// the same type as the one the store puts down.
func TestResolveStoreVacated_LevelCombinations(t *testing.T) {
	t.Parallel()
	level := []nodes.MaintainLevel{{GroupNodeID: vsGroup, BinTypeID: vsType, BinTypeCode: vsTypeC, Want: 4}}
	cases := []struct {
		name      string
		emptiesAt map[groupKey]int
		admit     bool
	}{
		{"lifted an EMPTY of the same type — neutral, admitted", map[groupKey]int{{vsChild, vsTypeC}: 1}, true},
		{"lifted a FULL — the store would push past the level", map[groupKey]int{}, false},
		{"lifted an empty of ANOTHER type — this type's level is unmoved", map[groupKey]int{{vsChild, "VS-U"}: 1}, false},
	}
	for _, c := range cases {
		f, grp, child := vacatedStore()
		f.maintainLevels = map[int64][]nodes.MaintainLevel{vsGroup: level}
		f.emptyCounts = map[groupKey]int{{vsGroup, vsTypeC}: 4}
		f.emptiesAt = c.emptiesAt
		err := resolveVacated(f, grp, child, "", KnownBinType(vsType))
		if (err == nil) != c.admit {
			t.Errorf("%s: err = %v, want admitted=%v", c.name, err, c.admit)
		}
	}
}
