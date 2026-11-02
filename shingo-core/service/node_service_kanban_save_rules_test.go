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

// node_service_kanban_save_rules_test.go — what the Nodes page's kanban save says: levels past
// the slots of a kanban are a warning (the card shows what cannot be covered), and a level on a
// live unloader's outbound saves with nothing said, since its returns always land.
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
	family, err := db.EnsureCarrierFamily("WCR-45")
	testutil.MustNoErr(t, err, "the family")
	testutil.MustNoErr(t, db.SetBinTypeFamily(small, family), "small")
	testutil.MustNoErr(t, db.SetBinTypeFamily(big, family), "big")
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
	if !got.saved || strings.Contains(said(got), "never repays") || strings.Contains(said(got), "no source") {
		t.Errorf("a source declaring the type: saved=%v warnings=%v; want nothing said, of it or of the other family's zone", got.saved, got.warnings)
	}

	got = mgSaveLevel(t, svc, k, big, 1)
	if !got.saved || !strings.Contains(said(got), "WCR-LENDER declares no level for WCR-BIG — a cart of it never repays that source") {
		t.Errorf("a source of the family with no level of the type: saved=%v warnings=%v", got.saved, got.warnings)
	}
	if strings.Contains(said(got), "WCR-Z32") {
		t.Errorf("a source of another family was warned of: %v", got.warnings)
	}

	testdb.KanbanSources(t, db, k, plainA, plainB)
	got = mgSaveLevel(t, svc, k, small, 1)
	if !strings.Contains(said(got), "WCR-PLAIN-A declares no level for WCR-SMALL") {
		t.Errorf("a plain source: warnings=%v", got.warnings)
	}

	testdb.KanbanSources(t, db, k, zone32)
	testdb.KanbanKeeper(t, db, k, "EDGE-WCR")
	got = mgSaveLevel(t, svc, k, small, 1)
	if !got.saved || !strings.Contains(said(got), "WCR-K declares WCR-SMALL with no source of its family") {
		t.Errorf("a type with no way out: saved=%v warnings=%v", got.saved, got.warnings)
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
		if !got.saved || !strings.Contains(strings.Join(got.warnings, "; "), "SR-PAST has room for 2 of") {
			t.Errorf("3 wanted across 2 positions: saved=%v refused=%v warnings=%v; want saved, a warning "+
				"naming the group and both numbers", got.saved, got.refused, got.warnings)
		}
	})

	t.Run("a level on a live unloader's outbound", func(t *testing.T) {
		grpID, _ := mgGroup(t, db, "SR-OUT", 3)
		mgUnloaderSendingTo(t, svc, "SR-OUT-UNL", "SR-OUT")
		got := mgSaveLevel(t, svc, grpID, mgBinType(t, db, "SR-OUT-TOTE"), 1)
		said := strings.Join(got.warnings, "; ")
		if !got.saved || len(got.refused) != 0 || strings.Contains(said, "SR-OUT-UNL") {
			t.Errorf("a level on SR-OUT-UNL's outbound: saved=%v refused=%v warnings=%v; want saved with nothing said of the unloader: its returns always land",
				got.saved, got.refused, got.warnings)
		}
	})
}
