//go:build docker

package service

import (
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/plantclaims"
)

// kanban_save_docker_test.go — the service's half of the one write: the rules
// over the state a save would leave, and a plain group left plain.

// TestSaveKanban_ThePlainGroupsSaveWritesNoRow: the modal posts its settings for
// every group it saves, so all-default settings on a plain group are nothing, and
// its overflow stays the property the doors read.
func TestSaveKanban_ThePlainGroupsSaveWritesNoRow(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, _ := mgGroup(t, db, "SKS-PLAIN", 2)
	mgGroup(t, db, "SKS-SPILL", 1)

	strict, station, overflow, cover := false, "", "", 0
	save := KanbanSave{GroupNodeID: grpID, Strict: &strict, Station: &station, Overflow: &overflow, Cover: &cover,
		Sources: &[]int64{}, Supports: &[]int64{}}
	chk, err := svc.SaveKanban(save)
	testutil.MustNoErr(t, err, "all-default save")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if k, err := db.GetKanban(grpID); err != nil || k != nil {
		t.Fatalf("an all-default save made a row: %+v (%v)", k, err)
	}

	overflow = "SKS-SPILL"
	chk, err = svc.SaveKanban(save)
	testutil.MustNoErr(t, err, "plain overflow")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if k, err := db.GetKanban(grpID); err != nil || k != nil {
		t.Fatalf("a plain overflow made a row: %+v (%v)", k, err)
	}
	if got := db.GetNodeProperty(grpID, nodes.PropOverflowDestination); got != "SKS-SPILL" {
		t.Errorf("property = %q", got)
	}
	cfg, err := svc.GetMaintainedGroup(grpID)
	testutil.MustNoErr(t, err, "config")
	if cfg.OverflowDestination != "SKS-SPILL" {
		t.Errorf("the modal reads overflow %q, want the property", cfg.OverflowDestination)
	}
}

// TestKanbanSave_APlainGroupsOverflowIsAProperty: the modal's whole flush on a plain
// group with lanes refuses nothing and makes no row, its overflow going to the
// property with its audit row. A row with no levels on the same shape (strict only)
// saves too; a level on it is still refused.
func TestKanbanSave_APlainGroupsOverflowIsAProperty(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, _ := mgGroup(t, db, "SKL-LANES", 1)
	_, err := nodes.AddLane(db.DB, grpID, "SKL-LANES-L1")
	testutil.MustNoErr(t, err, "lane")
	mgGroup(t, db, "SKL-SPILL", 1)

	strict, station, overflow, cover := false, "", "SKL-SPILL", 0
	settings := MaintainedGroupSettings{OverflowDestination: overflow}
	chk, err := svc.CheckMaintainedGroupSettings(grpID, settings)
	testutil.MustNoErr(t, err, "the settings' check")
	if chk.Err() != nil {
		t.Fatalf("the check refused a plain group's overflow: %v", chk.Refusals)
	}
	for what, save := range map[string]KanbanSave{
		"settings": {Strict: &strict, Station: &station, Overflow: &overflow},
		"cover":    {Cover: &cover},
		"supports": {Supports: &[]int64{}},
		"sources":  {Sources: &[]int64{}},
	} {
		save.GroupNodeID, save.Actor = grpID, "tester"
		chk, err := svc.SaveKanban(save)
		testutil.MustNoErr(t, err, what)
		if chk.Err() != nil || len(chk.Warnings) != 0 {
			t.Errorf("%s on a plain lane group: refused %v, warned %v", what, chk.Refusals, chk.Warnings)
		}
	}
	if k, err := db.GetKanban(grpID); err != nil || k != nil {
		t.Fatalf("the flush made a row: %+v (%v)", k, err)
	}
	if got := db.GetNodeProperty(grpID, nodes.PropOverflowDestination); got != "SKL-SPILL" {
		t.Errorf("property = %q, want the overflow", got)
	}
	var audited int
	testutil.MustNoErr(t, db.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type='node' AND entity_id=$1
		AND action=$2 AND new_value='SKL-SPILL' AND actor='tester'`,
		grpID, "property:"+nodes.PropOverflowDestination).Scan(&audited), "audit")
	if audited != 1 {
		t.Errorf("%d audit rows for the overflow, want 1", audited)
	}

	// A strict row with no levels behaves as plain: the same flush saves.
	strict = true
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Strict: &strict})
	testutil.MustNoErr(t, err, "strict")
	if chk.Err() != nil {
		t.Fatalf("a strict row on a lane group: %v", chk.Refusals)
	}
	overflow = ""
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Strict: &strict, Station: &station, Overflow: &overflow})
	testutil.MustNoErr(t, err, "a row's settings")
	if chk.Err() != nil {
		t.Errorf("a row with no levels refused its settings: %v", chk.Refusals)
	}
	bt := mgBinType(t, db, "SKL-T")
	want := 1
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &want}}})
	testutil.MustNoErr(t, err, "a level")
	if !mgRefusedBy(chk, "SKL-LANES-L1") {
		t.Errorf("a level on a lane group: refusals %v, want one naming the lane", chk.Refusals)
	}
}

// TestKanbanSave_TheModalReadsAndWritesTheBase: with the cover running, the
// configuration reads the declared number while every reader of the plant reads the
// effective want, so the modal writing back what it read changes nothing.
func TestKanbanSave_TheModalReadsAndWritesTheBase(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, _ := mgGroup(t, db, "SKB", 8)
	bt := mgBinType(t, db, "SKB-T")
	press := mgSupportNode(t, db, "SKB-PRESS")
	want, cover, station := 1, 4, "EDGE-1"
	chk, err := svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Station: &station, Cover: &cover, Supports: &[]int64{press},
		Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &want}}})
	testutil.MustNoErr(t, err, "the kanban")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	var part int64
	testutil.MustNoErr(t, db.QueryRow(`INSERT INTO payloads (code) VALUES ('SKB-P') RETURNING id`).Scan(&part), "part")
	_, err = db.DB.Exec(`INSERT INTO payload_bin_types (payload_id, bin_type_id) VALUES ($1,$2)`, part, bt)
	testutil.MustNoErr(t, err, "the part's carrier")
	testutil.MustNoErr(t, plantclaims.ReplaceProcess(db.DB, "SKB-PROC",
		[]plantclaims.StyleRow{{ProcessID: "SKB-PROC", StyleID: "S", ConfigGen: 1, IsActive: true}},
		[]plantclaims.ClaimRow{{ProcessID: "SKB-PROC", StyleID: "S", CoreNodeName: "SKB-PRESS", Role: protocol.ClaimRoleProduce,
			SwapMode: protocol.SwapMode("press_index"), PayloadCode: "SKB-P"}}, 0), "the press")

	effective, err := db.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "the plant's read")
	if len(effective) != 1 || effective[0].Want != 5 || effective[0].Base != 1 {
		t.Fatalf("the plant reads %+v, want 5 of base 1", effective)
	}
	cfg, err := svc.GetMaintainedGroup(grpID)
	testutil.MustNoErr(t, err, "the modal's read")
	if len(cfg.Levels) != 1 || cfg.Levels[0].Want != 1 {
		t.Fatalf("the modal reads %+v, want the declared 1", cfg.Levels)
	}
	back := cfg.Levels[0].Want
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &back}}})
	testutil.MustNoErr(t, err, "the modal's save")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	stored, err := db.ListDeclaredLevels(grpID)
	testutil.MustNoErr(t, err, "the declared read")
	if len(stored) != 1 || stored[0].Want != 1 {
		t.Errorf("after the modal's save the declared level is %+v, want 1", stored)
	}
}

func TestSaveKanban_OverflowMustBeANodeOnAKanban(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, _ := mgGroup(t, db, "SKO-K", 2)
	spillID, _ := mgGroup(t, db, "SKO-SPILL", 1)
	bt := mgBinType(t, db, "SKO-T")
	chk, err := svc.SetMaintainLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "level")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}

	for name, overflow := range map[string]string{"no such node": "SKO-NOWHERE", "itself": "SKO-K"} {
		chk, err := svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Overflow: &overflow})
		testutil.MustNoErr(t, err, name)
		if chk.Err() == nil {
			t.Errorf("%s: overflow %q saved; a kanban's overflow is another node", name, overflow)
		}
	}
	self := grpID
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Levels: []KanbanLevelSave{{BinTypeID: bt, Overflow: &self}}})
	testutil.MustNoErr(t, err, "level overflow onto itself")
	if chk.Err() == nil {
		t.Error("a type's overflow saved as the kanban itself")
	}

	spill := "SKO-SPILL"
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Overflow: &spill, Levels: []KanbanLevelSave{{BinTypeID: bt, Overflow: &spillID}}})
	testutil.MustNoErr(t, err, "good overflow")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	cfg, err := svc.GetMaintainedGroup(grpID)
	testutil.MustNoErr(t, err, "config")
	if cfg.OverflowDestination != "SKO-SPILL" || cfg.Levels[0].OverflowNode != "SKO-SPILL" {
		t.Errorf("config = %+v", cfg)
	}
	if got := db.GetNodeProperty(grpID, nodes.PropOverflowDestination); got != "" {
		t.Errorf("a kanban wrote the property: %q", got)
	}

	// A level line carrying only its overflow needs the level to exist.
	other := mgBinType(t, db, "SKO-OTHER")
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Levels: []KanbanLevelSave{{BinTypeID: other, Overflow: &spillID}}})
	testutil.MustNoErr(t, err, "overflow of no level")
	if !mgRefusedBy(chk, "SKO-OTHER") {
		t.Errorf("refusals = %v, want one naming the type with no level", chk.Refusals)
	}
}

func TestSaveKanban_Sources(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, kids := mgGroup(t, db, "SKR-K", 2)
	testdb.KanbanStation(t, db, grpID, "EDGE-1")
	a, _ := mgGroup(t, db, "SKR-A", 1)
	b, _ := mgGroup(t, db, "SKR-B", 1)
	lineside, lsKids := mgGroup(t, db, "SKR-LINESIDE", 1)
	testdb.KanbanStation(t, db, lineside, "EDGE-1")
	chk, err := svc.SetMaintainSupports(lineside, []int64{lsKids[0].ID})
	testutil.MustNoErr(t, err, "supports of the other kanban")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	chk, err = svc.SetMaintainSupports(grpID, []int64{kids[0].ID})
	testutil.MustNoErr(t, err, "supports")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}

	saveSources := func(ids ...int64) MaintainedGroupCheck {
		t.Helper()
		chk, err := svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Sources: &ids})
		testutil.MustNoErr(t, err, "sources")
		return chk
	}
	if chk := saveSources(grpID); !mgRefusedBy(chk, "itself") {
		t.Errorf("a source that is the kanban: refusals = %v", chk.Refusals)
	}
	if chk := saveSources(a, a); !mgRefusedBy(chk, "twice") {
		t.Errorf("a source twice: refusals = %v", chk.Refusals)
	}
	if got, _ := db.ListKanbanSources(grpID); len(got) != 0 {
		t.Errorf("a refused list was written: %+v", got)
	}

	if chk := saveSources(b, a); chk.Err() != nil || mgWarnedAbout(chk, "SKR-A") || mgWarnedAbout(chk, "SKR-B") {
		t.Errorf("two plain sources: %+v, want saved with nothing said of them", chk)
	}
	got, err := db.ListKanbanSources(grpID)
	testutil.MustNoErr(t, err, "read sources")
	if len(got) != 2 || got[0].SourceNodeID != b || got[0].Rank != 1 || got[1].SourceNodeID != a || got[1].Rank != 2 {
		t.Errorf("sources = %+v, want the list order as dense ranks", got)
	}

	// A lineside kanban's keeper never takes from another with supports: said, not
	// refused, and the list saves. A kanban with no supports of its own has no
	// keeper asking, so it is not told.
	chk = saveSources(lineside)
	if chk.Err() != nil || !mgWarnedAbout(chk, "SKR-LINESIDE") {
		t.Errorf("a lineside source: %+v, want saved with a warning", chk)
	}
	if got, _ := db.ListKanbanSources(grpID); len(got) != 1 || got[0].SourceNodeID != lineside {
		t.Errorf("the warned list did not save: %+v", got)
	}
	cover := 3
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Cover: &cover})
	testutil.MustNoErr(t, err, "cover")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	cfg, err := svc.GetMaintainedGroup(grpID)
	testutil.MustNoErr(t, err, "config")
	if cfg.RunningCover != 3 || len(cfg.Sources) != 1 {
		t.Errorf("config = %+v", cfg)
	}
	negative := -1
	chk, err = svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Cover: &negative})
	testutil.MustNoErr(t, err, "negative cover")
	if !mgRefusedBy(chk, "negative") {
		t.Errorf("a negative cover: refusals = %v", chk.Refusals)
	}
}

// The actor reaches the audit row, and the fixtures' direct writes are not the
// production write.
func TestSaveKanban_NamesItsActor(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)
	grpID, _ := mgGroup(t, db, "SKA-K", 2)
	bt := mgBinType(t, db, "SKA-T")
	want := 1
	chk, err := svc.SaveKanban(KanbanSave{GroupNodeID: grpID, Actor: "ann",
		Levels: []KanbanLevelSave{{BinTypeID: bt, Want: &want}}})
	testutil.MustNoErr(t, err, "save")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	rows, err := db.ListEntityAudit("node", grpID)
	testutil.MustNoErr(t, err, "audit")
	if len(rows) != 1 || rows[0].Action != "kanban:level:SKA-T" || rows[0].Actor != "ann" || rows[0].NewValue != "1" {
		t.Errorf("audit = %+v", rows)
	}
}

// mgSupportNode makes a process position a kanban can support.
func mgSupportNode(t *testing.T, db *store.DB, name string) int64 {
	t.Helper()
	n := &nodes.Node{Name: name, Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(n), "supported position")
	return n.ID
}
