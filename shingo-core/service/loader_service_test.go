//go:build docker

package service

import (
	"errors"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// TestLoaderService_SetHome_RejectsSyntheticWindow guards the structural fix for
// the Springfield "lane 14" loader-window incident: a loader window/position
// must be a real physical slot, never a synthetic container (a node group or an
// empty lane). Assigning a container produced a loader that dispatched into a
// location with no slots ("synthetic node has no children").
func TestLoaderService_SetHome_RejectsSyntheticWindow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	loaderID, err := db.CreateLoader(loaders.Loader{
		Name: "TEST-LOADER", Role: "produce", Layout: "dedicated_positions",
		Replenishment: loaders.ReplenishmentThreshold,
	})
	testutil.MustNoErr(t, err, "create loader")

	// A synthetic container node (lane/group) must be rejected as a window.
	lane := &nodes.Node{Name: "FAKE-LANE", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "create synthetic lane")
	if err := svc.SetHome(loaderID, lane.ID, "", "", 0); err == nil {
		t.Fatal("SetHome accepted a synthetic container node as a loader window; want rejection")
	}

	// A real physical slot node must be accepted.
	slot := &nodes.Node{Name: "REAL-SLOT", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(slot), "create physical slot")
	testutil.MustNoErr(t, svc.SetHome(loaderID, slot.ID, "", "", 0), "SetHome physical slot")
}

// MG3-3: inbound_source is resolve-checked at SAVE TIME.
//
// It was an unchecked string: typed by an operator, stored verbatim, and read
// months later by the source finder — which resolves it, gets nothing, and falls
// through to whatever its tier gates allow. A typo produced no error at save, no
// error at read, and a silent change of sourcing behaviour nobody could connect
// to the edit that caused it.
//
// It matters more now. A maintained group is named by exactly this field, and
// phase 3 makes naming it consequential: the group either serves this claim or
// fences it, and both are decisions about a node. A field that might name
// nothing at all cannot carry either.
func TestLoaderService_InboundSourceIsResolveChecked(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	real := &nodes.Node{Name: "MG33-REAL-GROUP", Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(real), "create the group")

	t.Run("a node that exists is accepted", func(t *testing.T) {
		if _, err := svc.Create("MG33-OK", "produce", "dedicated_positions",
			loaders.ReplenishmentThreshold, "", "MG33-REAL-GROUP", false); err != nil {
			t.Fatalf("a real node was rejected: %v", err)
		}
	})

	t.Run("blank is valid and means no scoped source", func(t *testing.T) {
		// The overwhelmingly common case, and the reason this is a resolve-check
		// rather than a required field.
		if _, err := svc.Create("MG33-BLANK", "produce", "dedicated_positions",
			loaders.ReplenishmentThreshold, "", "", false); err != nil {
			t.Fatalf("a blank inbound source was rejected: %v", err)
		}
	})

	t.Run("a typo is refused at save", func(t *testing.T) {
		_, err := svc.Create("MG33-TYPO", "produce", "dedicated_positions",
			loaders.ReplenishmentThreshold, "", "MG33-NO-SUCH-GROUP", false)
		if err == nil {
			t.Fatal("a claim naming a node that does not exist was saved. Save time is the " +
				"only place this is cheap: the operator is standing there and knows what " +
				"they meant, and at read time the finder has an order to place and no way " +
				"to ask")
		}
		if !errors.Is(err, ErrInboundSourceUnresolved) {
			t.Errorf("err = %v, want ErrInboundSourceUnresolved so the endpoint can render "+
				"it as a 400 rather than a 500", err)
		}
	})

	t.Run("update is checked too", func(t *testing.T) {
		id, err := svc.Create("MG33-UPD", "produce", "dedicated_positions",
			loaders.ReplenishmentThreshold, "", "", false)
		testutil.MustNoErr(t, err, "create")
		if err := svc.Update(LoaderUpdate{ID: id, Name: "MG33-UPD", Layout: "dedicated_positions",
			Replenishment: loaders.ReplenishmentThreshold,
			InboundSource: "MG33-STILL-NO-SUCH-GROUP"}); err == nil {
			t.Error("Update accepted an unresolvable inbound source. A field guarded only on " +
				"create is guarded on the path nobody uses twice")
		}
	})
}

// A source is a node group or a loader position — never a lane, and an
// unloader's fulls never come from an empties bank. Refused at save, where the
// operator can fix it, rather than discovered when a bin put back in a sibling
// lane, or a full refused by the bank, is never found again.
func TestLoaderService_FlowEndpointsAreTheRightKindOfNode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewLoaderService(db, nil)

	ngrp, err := db.GetNodeTypeByCode(protocol.NodeClassNGRP)
	testutil.MustNoErr(t, err, "NGRP type")
	laneType, err := db.GetNodeTypeByCode(protocol.NodeClassLANE)
	testutil.MustNoErr(t, err, "LANE type")
	group := &nodes.Node{Name: "FE-GRP", NodeTypeID: &ngrp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(group), "group")
	lane := &nodes.Node{Name: "FE-LANE", NodeTypeID: &laneType.ID, ParentID: &group.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(lane), "lane")
	bank := &nodes.Node{Name: "FE-BANK", NodeTypeID: &ngrp.ID, Enabled: true, IsSynthetic: true}
	testutil.MustNoErr(t, db.CreateNode(bank), "bank")
	bt := &bins.BinType{Code: "FE-T", Description: "tote"}
	testutil.MustNoErr(t, db.CreateBinType(bt), "bin type")
	testutil.MustNoErr(t, db.SetMaintainLevel(store.MaintainLevel{GroupNodeID: bank.ID, BinTypeID: bt.ID, Want: 2}),
		"the bank's level")

	if _, err := svc.Create("FE-IN-LANE", "consume", "dedicated_positions",
		loaders.ReplenishmentOperator, "", lane.Name, false); !errors.Is(err, ErrSourceIsALane) {
		t.Errorf("an inbound source naming a lane: err = %v, want ErrSourceIsALane", err)
	}
	if _, err := svc.Create("FE-OUT-LANE", "consume", "dedicated_positions",
		loaders.ReplenishmentOperator, lane.Name, "", false); !errors.Is(err, ErrSourceIsALane) {
		t.Errorf("an outbound destination naming a lane: err = %v, want ErrSourceIsALane", err)
	}
	if _, err := svc.Create("FE-FULLS-BANK", "consume", "dedicated_positions",
		loaders.ReplenishmentOperator, "", bank.Name, false); !errors.Is(err, ErrFullsFromAnEmptiesBank) {
		t.Errorf("an unloader's fulls from a maintained group: err = %v, want ErrFullsFromAnEmptiesBank", err)
	}
	// A loader's EMPTIES coming from a bank is what a bank is for.
	if _, err := svc.Create("FE-EMPTIES-BANK", "produce", "dedicated_positions",
		loaders.ReplenishmentThreshold, "", bank.Name, false); err != nil {
		t.Errorf("a loader drawing empties from a maintained group was refused: %v", err)
	}
	if _, err := svc.Create("FE-GROUP-OK", "consume", "dedicated_positions",
		loaders.ReplenishmentOperator, "", group.Name, false); err != nil {
		t.Errorf("an ordinary group as an unloader's inbound source was refused: %v", err)
	}
}
