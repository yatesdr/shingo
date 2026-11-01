//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/loaders"
)

// node_service_kanban_save_rules_test.go — what the Nodes page's kanban
//  save says about two configurations that cannot work: levels past the
// enabled positions of a kanban, and levels on a live unloader's
// outbound with no overflow destination. The first is a warning (the card shows
// what cannot be covered), the second a refusal; their neighbours
// (an overflow set) save as before.
//
// Each case saves a level through SetKanbanLevel (the level door of
// www/handlers_kanbans.go) and records whether it saved, what it
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
	chk, err := svc.SetKanbanLevel(grpID, btID, want)
	testutil.MustNoErr(t, err, "SetKanbanLevel")
	levels, err := svc.ListKanbanLevels(grpID)
	testutil.MustNoErr(t, err, "ListKanbanLevels")
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

// TestKanbanSave_WhatTheChooserCannotRepay: the words a kanban's save says
// about its sources and overflow, per type, none of them a refusal.
func TestKanbanSave_WhatTheChooserCannotRepay(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	sized := func(code string, w, l float64) int64 {
		var id int64
		testutil.MustNoErr(t, db.QueryRow(`INSERT INTO bin_types (code, width_in, length_in) VALUES ($1,$2,$3) RETURNING id`,
			code, w, l).Scan(&id), "type "+code)
		return id
	}
	small, big, wide := sized("WCR-SMALL", 45, 48), sized("WCR-BIG", 48, 45), sized("WCR-WIDE", 32, 30)
	declare := func(g, bt int64, want int) {
		testutil.MustNoErr(t, db.SetKanbanLevel(store.KanbanLevel{GroupNodeID: g, BinTypeID: bt, Want: want}), "level")
	}
	said := func(got mgSaveOutcome) string { return strings.Join(got.warnings, "; ") }

	k, _ := mgGroup(t, db, "WCR-K", 8)
	lender, _ := mgGroup(t, db, "WCR-LENDER", 4)
	declare(lender, small, 2)
	plainA, _ := mgGroup(t, db, "WCR-PLAIN-A", 4)
	plainB, _ := mgGroup(t, db, "WCR-PLAIN-B", 4)
	zone32, _ := mgGroup(t, db, "WCR-Z32", 4)
	declare(zone32, wide, 2)

	testdb.KanbanSources(t, db, k, lender, zone32)
	got := mgSaveLevel(t, svc, k, small, 1)
	if !got.saved || strings.Contains(said(got), "repaid only") || strings.Contains(said(got), "no source") {
		t.Errorf("a source declaring the type: saved=%v warnings=%v; want nothing said, of it or of the other floor's zone", got.saved, got.warnings)
	}

	got = mgSaveLevel(t, svc, k, big, 1)
	if !got.saved || !strings.Contains(said(got), "WCR-LENDER declares no level for WCR-BIG — repaid only as WCR-BIG's overflow") {
		t.Errorf("a source on the floor with no level of the type: saved=%v warnings=%v", got.saved, got.warnings)
	}
	if strings.Contains(said(got), "WCR-Z32") {
		t.Errorf("a source of another floor was warned of: %v", got.warnings)
	}

	testdb.KanbanSources(t, db, k, plainA, plainB)
	got = mgSaveLevel(t, svc, k, small, 1)
	if !strings.Contains(said(got), "WCR-PLAIN-A and WCR-PLAIN-B declare no level for WCR-SMALL — nothing can repay both; one drains") {
		t.Errorf("two sources of a floor with no level: warnings=%v", got.warnings)
	}

	testdb.KanbanSources(t, db, k, zone32)
	testdb.KanbanKeeper(t, db, k, "EDGE-WCR")
	got = mgSaveLevel(t, svc, k, small, 1)
	if !got.saved || !strings.Contains(said(got), "WCR-K declares WCR-SMALL with no source or overflow for it") {
		t.Errorf("a type with no way out: saved=%v warnings=%v", got.saved, got.warnings)
	}
	testdb.KanbanOverflow(t, db, k, "WCR-PLAIN-A")
	got = mgSaveLevel(t, svc, k, small, 1)
	if strings.Contains(said(got), "no source or overflow") {
		t.Errorf("with an overflow: warnings=%v", got.warnings)
	}
}

func TestKanbanSave_LevelsAndTheUnloaderOutbound(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	on := func(grpID int64) {
		testdb.KanbanStation(t, db, grpID, "EDGE-1")
	}

	t.Run("levels summing past the enabled positions of a kanban", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-PAST", 2)
		on(grpID)
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-PAST-TOTE"), 3)
		if !got.saved || len(got.warnings) != 1 || !strings.Contains(got.warnings[0], "SR-PAST declares 3 carriers across 2") {
			t.Errorf("3 wanted across 2 positions: saved=%v refused=%v warnings=%v; want saved, a warning "+
				"naming the group and both numbers", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("a level on a live unloader's outbound with no overflow", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OUT", 3)
		mgUnloaderSendingTo(t, svc, "SR-OUT-UNL", "SR-OUT")
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OUT-TOTE"), 1)
		said := strings.Join(got.warnings, "; ")
		if !got.saved || len(got.refused) != 0 || !strings.Contains(said, "SR-OUT-UNL sends its empties to SR-OUT") {
			t.Errorf("a level on SR-OUT-UNL's outbound: saved=%v refused=%v warnings=%v; want saved, with a warning "+
				"naming the unloader and the group", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("a level on a live unloader's outbound with an overflow", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OVF", 3)
		mgGroup(t, db, "SR-OVF-SPILL", 1)
		mgUnloaderSendingTo(t, svc, "SR-OVF-UNL", "SR-OVF")
		testdb.KanbanOverflow(t, db, grpID, "SR-OVF-SPILL")
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OVF-TOTE"), 1)
		if !got.saved || len(got.refused) != 0 || len(got.warnings) != 0 {
			t.Errorf("a level on SR-OVF-UNL's outbound, overflow set: saved=%v refused=%v warnings=%v; "+
				"want saved with nothing said", got.saved, got.refused, got.warnings)
		}
	})
}
