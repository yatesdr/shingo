package service

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// flow_preset_member_test.go — a recipe's source and destination are roles
// where Core picks, so a recipe naming a member of a node group there is
// refused at its save, as a claim naming one is; the group itself saves
// (owner, 2026-11-11).

// TestPresets_ARecipeNamingAMemberWhereCorePicksIsRefused takes a shape the
// press runs and saves it twice: once with Core's list putting its outbound
// destination in a group, once with that node in no group.
func TestPresets_ARecipeNamingAMemberWhereCorePicksIsRefused(t *testing.T) {
	t.Parallel()
	svc, seeded, _ := presetFixture(t)
	view, err := svc.PresetsFor(seeded.ProcessID)
	testutil.MustNoErr(t, err, "svc.PresetsFor")
	var key, dest string
	var cells []domain.FlowCell
	for _, c := range view.Candidates {
		for _, cell := range c.Shape {
			if cell.OutboundDestination != "" && dest == "" {
				key, dest, cells = c.ShapeKey, cell.OutboundDestination, c.Shape
			}
		}
	}
	if dest == "" {
		t.Fatal("no candidate shape names an outbound destination")
	}
	// Core's list holds every node the shape names.
	list := map[string]protocol.NodeInfo{"PM-GRP": {Name: "PM-GRP", NodeType: protocol.NodeClassNGRP}}
	for _, c := range cells {
		for _, n := range []string{c.CoreNodeName, c.PairedCoreNode, c.SecondPairedCoreNode, c.InboundSource,
			c.OutboundDestination, c.InboundStaging, c.OutboundStaging, c.PartialDestination} {
			if n != "" {
				list[n] = protocol.NodeInfo{Name: n}
			}
		}
	}

	svc.SetCoreNodeKindsResolver(func() domain.CoreNodeKinds {
		return domain.CoreNodeKindsOf(list, map[string][]string{"PM-GRP": {dest}}, true)
	})
	_, err = svc.CreateFromCandidate(seeded.ProcessID, "Member", key, "test")
	if !errors.Is(err, domain.ErrRefusedLeg) {
		t.Fatalf("a recipe sending a cell to %q, a member of a group: err %v, want the leg refusal", dest, err)
	}

	svc.SetCoreNodeKindsResolver(func() domain.CoreNodeKinds { return domain.CoreNodeKindsOf(list, nil, true) })
	_, err = svc.CreateFromCandidate(seeded.ProcessID, "In no group", key, "test")
	testutil.MustNoErr(t, err, "the same recipe with "+dest+" in no group")
}
