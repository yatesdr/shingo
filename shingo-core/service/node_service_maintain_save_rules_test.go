//go:build docker

package service

import (
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// node_service_maintain_save_rules_test.go — what the Nodes page's maintained
// group save says about two configurations that cannot work: levels past the
// enabled positions of a maintained group, and levels on a live unloader's
// outbound with no overflow destination. Both are refused; their neighbours
// (maintenance off; an overflow set) save as before.
//
// Each case saves a level through SetMaintainLevel (the level door of
// www/handlers_maintained_groups.go) and records whether it saved, what it
// refused and what it warned. The positions and the unloader are built first;
// the level is the save under test.

type mgSaveOutcome struct {
	saved    bool
	refused  []string
	warnings []string
}

// mgSaveLevel saves one level and reads back whether it stands.
func mgSaveLevel(t *testing.T, svc *NodeService, grpID, btID int64, want int) mgSaveOutcome {
	t.Helper()
	chk, err := svc.SetMaintainLevel(grpID, btID, want)
	testutil.MustNoErr(t, err, "SetMaintainLevel")
	levels, err := svc.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	saved := false
	for _, l := range levels {
		if l.BinTypeID == btID && l.Want == want {
			saved = true
		}
	}
	return mgSaveOutcome{saved: saved, refused: chk.Refusals, warnings: chk.Warnings}
}

// mgUnloaderSendingTo makes a live unloader whose empties go to dest.
func mgUnloaderSendingTo(t *testing.T, svc *NodeService, name, dest string) {
	t.Helper()
	_, err := svc.db.CreateLoader(loaders.Loader{Name: name, Role: loaders.RoleConsume,
		Layout: loaders.LayoutSharedWindow, Replenishment: loaders.ReplenishmentOperator, OutboundDest: dest})
	testutil.MustNoErr(t, err, "create unloader "+name)
}

func TestMaintainedGroupSave_LevelsAndTheUnloaderOutbound(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	on := func(grpID int64) {
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "EDGE-1"), "station")
	}

	t.Run("levels summing past the enabled positions of a maintained group", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-PAST", 2)
		on(grpID)
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-PAST-TOTE"), 3)
		if got.saved || !mgRefusedBy(MaintainedGroupCheck{Refusals: got.refused}, "SR-PAST declares 3 carriers across 2") {
			t.Errorf("3 wanted across 2 positions: saved=%v refused=%v warnings=%v; want refused, "+
				"naming the group and both numbers", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("levels summing past the positions of a group not maintained", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OFF", 2)
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OFF-TOTE"), 3)
		if !got.saved || len(got.refused) != 0 || len(got.warnings) != 0 {
			t.Errorf("3 wanted across 2 positions, maintenance off: saved=%v refused=%v warnings=%v; "+
				"want saved with nothing said", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("a level on a live unloader's outbound with no overflow", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OUT", 3)
		mgUnloaderSendingTo(t, svc, "SR-OUT-UNL", "SR-OUT")
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OUT-TOTE"), 1)
		if got.saved || !mgRefusedBy(MaintainedGroupCheck{Refusals: got.refused}, "SR-OUT-UNL sends its empties to SR-OUT") {
			t.Errorf("a level on SR-OUT-UNL's outbound: saved=%v refused=%v warnings=%v; want refused, "+
				"naming the unloader and the group", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("a level on a live unloader's outbound with an overflow", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OVF", 3)
		mgGroup(t, db, "SR-OVF-SPILL", 1)
		mgUnloaderSendingTo(t, svc, "SR-OVF-UNL", "SR-OVF")
		testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropOverflowDestination, "SR-OVF-SPILL"), "overflow")
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OVF-TOTE"), 1)
		if !got.saved || len(got.refused) != 0 || len(got.warnings) != 0 {
			t.Errorf("a level on SR-OVF-UNL's outbound, overflow set: saved=%v refused=%v warnings=%v; "+
				"want saved with nothing said", got.saved, got.refused, got.warnings)
		}
	})
}
