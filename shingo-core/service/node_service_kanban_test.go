//go:build docker

package service

import (
	"shingocore/internal/testdb"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/domain"
	"shingocore/store"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// Save-time rules for a kanban.
//
// Each case names the state it is checking and asserts on the MESSAGE, not just
// on refused-or-not: every one of these rules exists to tell an operator which
// setting is the problem, and a rule that refuses without saying what refused is
// half-built. Matching on the identifier the message must contain is the cheapest
// way to keep that true.

// mgGroup makes an NGRP with n direct storage children and returns both.
func mgGroup(t *testing.T, db *store.DB, name string, children int) (int64, []*nodes.Node) {
	t.Helper()
	grpID, err := nodes.CreateGroup(db.DB, name)
	testutil.MustNoErr(t, err, "CreateGroup")
	testdb.MarkGroup(t, db, grpID)
	var kids []*nodes.Node
	for i := 0; i < children; i++ {
		k := &nodes.Node{Name: name + "-P" + string(rune('A'+i)), Enabled: true, ParentID: &grpID}
		testutil.MustNoErr(t, db.CreateNode(k), "create child")
		kids = append(kids, k)
	}
	return grpID, kids
}

func mgBinType(t *testing.T, db *store.DB, code string) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, db.QueryRow(
		`INSERT INTO bin_types (code) VALUES ($1) RETURNING id`, code).Scan(&id), "insert bin type")
	return id
}

func mgRefusedBy(chk KanbanCheck, substr string) bool {
	for _, r := range chk.Refusals {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func mgWarnedAbout(chk KanbanCheck, substr string) bool {
	for _, wm := range chk.Warnings {
		if strings.Contains(wm, substr) {
			return true
		}
	}
	return false
}

// projectOrder no-ops on a blank StationID, so a top-up order minted without a
// station would run on the floor and appear on no board anywhere. Refused rather
// than warned because nothing downstream can detect it afterwards.
func TestKanban_RefusesSupportsWithoutStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-NO-STATION", 2)
	bt := mgBinType(t, db, "NO-STATION-TOTE")

	// No supports: a kanban that declares what it holds needs no station.
	chk, err := svc.SetKanbanLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "level with no supports")
	if chk.Err() != nil {
		t.Fatalf("a kanban with no supports and no station was refused: %v", chk.Refusals)
	}

	chk, err = svc.SetKanbanSupports(grpID, []int64{kids[0].ID})
	testutil.MustNoErr(t, err, "supports")
	if !mgRefusedBy(chk, "station") {
		t.Fatalf("refusals = %v, want one about the missing station", chk.Refusals)
	}
	if got, err := svc.ListKanbanSupports(grpID); err != nil {
		t.Fatal(err)
	} else if len(got) != 0 {
		t.Errorf("the refused supports were written: %+v", got)
	}

	testdb.KanbanStation(t, db, grpID, "EDGE-1")
	chk, err = svc.SetKanbanSupports(grpID, []int64{kids[0].ID})
	testutil.MustNoErr(t, err, "supports with a station")
	if chk.Err() != nil {
		t.Fatalf("supports with a station were refused: %v", chk.Refusals)
	}

	// Clearing the station under them is the same state, refused the same way.
	chk, err = svc.CheckKanbanSettings(grpID, KanbanSettings{})
	testutil.MustNoErr(t, err, "CheckKanbanSettings")
	if !mgRefusedBy(chk, "station") {
		t.Fatalf("refusals = %v, want one about the missing station", chk.Refusals)
	}
}

// Kanbans are depth-1. A lane child means carriers can be buried, and
// a level counted over buried carriers is a number whose meaning changes with
// what is parked in front of it.
func TestKanban_RefusesNonFlatGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-DEEP", 1)
	_, err := nodes.AddLane(db.DB, grpID, "GRP-DEEP-LANE-1")
	testutil.MustNoErr(t, err, "AddLane")

	chk, err := svc.CheckKanbanSettings(grpID, KanbanSettings{
		MaintenanceStation: "EDGE-1",
	})
	testutil.MustNoErr(t, err, "CheckKanbanSettings")
	if !mgRefusedBy(chk, "GRP-DEEP-LANE-1") {
		t.Fatalf("refusals = %v, want one naming the lane", chk.Refusals)
	}
}

// A declared level whose carrier type no enabled position accepts is a
// permanent, silent shortfall: the keeper would ask forever and every placement
// would be refused.
func TestKanban_RefusesLevelNoChildAccepts(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-WRONG-TYPE", 2)
	big := mgBinType(t, db, "45x58x32")
	small := mgBinType(t, db, "45x48x24")

	// Both positions take only the small carrier.
	for _, k := range kids {
		testutil.MustNoErr(t, db.SetNodeProperty(k.ID, "bin_type_mode", "specific"), "bin_type_mode")
		testutil.MustNoErr(t, db.SetNodeBinTypes(k.ID, []int64{small}), "SetNodeBinTypes")
	}

	chk, err := svc.SetKanbanLevel(grpID, big, 4)
	testutil.MustNoErr(t, err, "SetKanbanLevel big")
	if !mgRefusedBy(chk, "45x58x32") {
		t.Fatalf("refusals = %v, want one naming the carrier type no position accepts", chk.Refusals)
	}
	// Refused means NOT WRITTEN. A refusal that still persisted the row would be
	// a warning wearing a refusal's message.
	levels, err := svc.ListKanbanLevels(grpID)
	testutil.MustNoErr(t, err, "ListKanbanLevels")
	if len(levels) != 0 {
		t.Fatalf("levels after a refused save = %+v, want none written", levels)
	}

	// The type the positions DO accept goes through.
	chk, err = svc.SetKanbanLevel(grpID, small, 1)
	testutil.MustNoErr(t, err, "SetKanbanLevel small")
	if chk.Err() != nil {
		t.Fatalf("an accepted carrier type was refused: %v", chk.Refusals)
	}
}

// An unrestricted position takes any carrier, so nothing about a declared type
// can be impossible there. Same predicate the resolver uses — an empty effective
// set means no restriction, and a config screen that read it as "accepts
// nothing" would refuse what the floor allows.
func TestKanban_UnrestrictedChildAcceptsAnyLevel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-OPEN", 3)
	bt := mgBinType(t, db, "OPEN-TOTE")

	chk, err := svc.SetKanbanLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "SetKanbanLevel")
	if chk.Err() != nil {
		t.Fatalf("a level on a group of unrestricted positions was refused: %v", chk.Refusals)
	}
}

// A level save leaves a group's bin_type_mode alone, and the group's mode does not
// reach its children: a child's types resolve by the child's own mode and the
// nearest ancestor with rows, so an ancestor's list governs the children whatever
// the group says. The save's refusal asks those children
// (ValidateKanban), which is what catches a level nothing can hold.
func TestKanban_ALevelSaveLeavesTheModeAlone(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	parentID, _ := mgGroup(t, db, "GRP-MODE-PARENT", 0)
	grpID, kids := mgGroup(t, db, "GRP-MODE", 2)
	testutil.MustNoErr(t, db.SetNodeParent(grpID, parentID), "SetNodeParent")
	bt := mgBinType(t, db, "MODE-TOTE")
	testutil.MustNoErr(t, db.SetNodeProperty(parentID, "bin_type_mode", "specific"), "parent mode")
	testutil.MustNoErr(t, db.SetNodeBinTypes(parentID, []int64{bt}), "parent types")

	inherits := func(when string) {
		t.Helper()
		got, err := db.GetEffectiveBinTypes(kids[0].ID)
		testutil.MustNoErr(t, err, "GetEffectiveBinTypes")
		if len(got) != 1 || got[0].ID != bt {
			t.Errorf("%s: the child's types = %+v, want the ancestor's one", when, got)
		}
	}
	inherits("before the save")

	chk, err := svc.SetKanbanLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "SetKanbanLevel")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if got := db.GetNodeProperty(grpID, "bin_type_mode"); got != "" {
		t.Errorf("bin_type_mode after a level save = %q, want unset", got)
	}
	inherits("after the save")

	testutil.MustNoErr(t, db.SetNodeProperty(grpID, "bin_type_mode", "all"), "mode all")
	inherits("with the group's mode at all")

	// A mode somebody chose stays through a save.
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, "bin_type_mode", "specific"), "choose specific")
	testutil.MustNoErr(t, db.SetNodeBinTypes(grpID, []int64{bt}), "SetNodeBinTypes")
	_, err = svc.SetKanbanLevel(grpID, bt, 2)
	testutil.MustNoErr(t, err, "SetKanbanLevel again")
	if got := db.GetNodeProperty(grpID, "bin_type_mode"); got != "specific" {
		t.Errorf("bin_type_mode after a second level = %q, want the chosen %q", got, "specific")
	}
}

// Σwant filling every position leaves nowhere for a carrier coming back in.
// A WARNING, not a refusal: the group can gain positions or re-enable children,
// and the runtime guard is the resolver refusing a push at level — not this
// arithmetic.
func TestKanban_WarnsWhenLevelFillsEveryPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-FULL", 2)
	bt := mgBinType(t, db, "FULL-TOTE")

	testdb.KanbanStation(t, db, grpID, "EDGE-1")

	chk, err := svc.SetKanbanLevel(grpID, bt, 2)
	testutil.MustNoErr(t, err, "SetKanbanLevel")
	if chk.Err() != nil {
		t.Fatalf("a full-house level was REFUSED; it must only warn: %v", chk.Refusals)
	}
	if !mgWarnedAbout(chk, "GRP-FULL") {
		t.Fatalf("warnings = %v, want one about leaving no free position", chk.Warnings)
	}
	// It saved.
	levels, err := svc.ListKanbanLevels(grpID)
	testutil.MustNoErr(t, err, "ListKanbanLevels")
	if len(levels) != 1 || levels[0].Want != 2 {
		t.Errorf("levels = %+v, want the warned-about level written anyway", levels)
	}
}

// A supported position with no carrier types set cannot type the empty pull that
// goes to it. A warning because it is a data gap rather than a contradiction —
// and because it is the normal state today.
func TestKanban_WarnsOnUntypedSupportedPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-SUP", 4)
	press := &nodes.Node{Name: "PRESS-UNTYPED", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press")
	testdb.KanbanStation(t, db, grpID, "EDGE-1")

	chk, err := svc.SetKanbanSupports(grpID, []int64{press.ID})
	testutil.MustNoErr(t, err, "SetKanbanSupports")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if !mgWarnedAbout(chk, "PRESS-UNTYPED") {
		t.Fatalf("warnings = %v, want one naming the untyped position", chk.Warnings)
	}
	got, err := svc.ListKanbanSupports(grpID)
	testutil.MustNoErr(t, err, "ListKanbanSupports")
	if len(got) != 1 {
		t.Errorf("supports = %+v, want the warned-about support written anyway", got)
	}
}

// ── The holds-bins guard ───────────────────────────────────────────────────
//
// Separate from the save-time rules and reading a different thing: the rules
// read CONFIGURATION and are true regardless of the floor, these read the FLOOR.
// They are also DELTA checks — they fire on the transition, not the state, so a
// group that has been reserved for a month does not re-ask every save.

func mgPutBin(t *testing.T, db *store.DB, label string, binTypeID, nodeID int64) {
	t.Helper()
	testutil.MustNoErr(t, db.CreateBin(&bins.Bin{
		Label: label, BinTypeID: binTypeID, NodeID: &nodeID, Status: domain.BinStatusAvailable,
	}), "CreateBin "+label)
}

// Reserving a group that still holds carriers hides them from every process not
// on the list. Refused, naming them — and force is the operator saying
// they already know.
func TestKanbanGuard_ReserveWithResidents(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-HOLDING", 2)
	bt := mgBinType(t, db, "HOLD-TOTE")
	mgPutBin(t, db, "BIN-HELD-1", bt, kids[0].ID)

	g, err := svc.CheckKanbanSettingsChange(grpID,
		KanbanSettings{StrictSourcing: true}, false)
	testutil.MustNoErr(t, err, "CheckKanbanSettingsChange")
	if !strings.Contains(g.Blocked, "BIN-HELD-1") {
		t.Fatalf("blocked = %q, want it to name the carrier standing there", g.Blocked)
	}

	// force is the whole point of the guard being a refusal rather than a
	// prohibition.
	g, err = svc.CheckKanbanSettingsChange(grpID,
		KanbanSettings{StrictSourcing: true}, true)
	testutil.MustNoErr(t, err, "CheckKanbanSettingsChange forced")
	if g.Blocked != "" {
		t.Errorf("forced change still blocked: %q", g.Blocked)
	}
}

// The guard is a DELTA check. A save that leaves both switches where they were
// asks nothing, however many carriers are standing there.
func TestKanbanGuard_NoTransitionAsksNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-STEADY", 2)
	bt := mgBinType(t, db, "STEADY-TOTE")
	mgPutBin(t, db, "BIN-STEADY-1", bt, kids[0].ID)

	testdb.KanbanStrict(t, db, grpID, true)

	g, err := svc.CheckKanbanSettingsChange(grpID, KanbanSettings{
		StrictSourcing: true, MaintenanceStation: "EDGE-2",
	}, false)
	testutil.MustNoErr(t, err, "CheckKanbanSettingsChange")
	if g.Blocked != "" {
		t.Errorf("a save with no transition was blocked: %q", g.Blocked)
	}
}

// Turning the reserve ON reports the orders already sourcing here. Reported,
// never blocking: they are admitted and looking, the fence does not reach back
// and cancel them, and nothing in this program cancels anything.
func TestKanbanGuard_StrictOnReportsDrain(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-DRAIN", 2)
	o := &orders.Order{
		EdgeUUID: "drain-1", StationID: "EDGE-1", OrderType: "retrieve",
		Status: "queued", SourceNode: "GRP-DRAIN", Quantity: 1,
	}
	testutil.MustNoErr(t, db.CreateOrder(o), "CreateOrder")

	g, err := svc.CheckKanbanSettingsChange(grpID,
		KanbanSettings{StrictSourcing: true}, false)
	testutil.MustNoErr(t, err, "CheckKanbanSettingsChange")
	if g.Blocked != "" {
		t.Fatalf("an empty group was blocked: %q", g.Blocked)
	}
	if len(g.Drain) != 1 || !strings.Contains(g.Drain[0], "GRP-DRAIN") {
		t.Fatalf("drain = %v, want the one queued order sourcing from the group", g.Drain)
	}
}

// Narrowing supports takes the carriers away from exactly the processes dropped.
// Widening strands nobody and asks nothing.
func TestKanbanGuard_SupportsNarrowingOnly(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-SCOPE", 2)
	bt := mgBinType(t, db, "SCOPE-TOTE")
	mgPutBin(t, db, "BIN-SCOPE-1", bt, kids[0].ID)

	pressA := &nodes.Node{Name: "PRESS-SCOPE-A", Enabled: true}
	pressB := &nodes.Node{Name: "PRESS-SCOPE-B", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(pressA), "create A")
	testutil.MustNoErr(t, db.CreateNode(pressB), "create B")
	testutil.MustNoErr(t, db.SetKanbanSupports(grpID, []int64{pressA.ID, pressB.ID}), "seed supports")

	// Widening: nothing to ask.
	pressC := &nodes.Node{Name: "PRESS-SCOPE-C", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(pressC), "create C")
	g, err := svc.CheckKanbanSupportsChange(grpID,
		[]int64{pressA.ID, pressB.ID, pressC.ID}, false)
	testutil.MustNoErr(t, err, "widen")
	if g.Blocked != "" {
		t.Errorf("widening the supported set was blocked: %q", g.Blocked)
	}

	// Narrowing: named, and it names both the carrier and the process dropped.
	g, err = svc.CheckKanbanSupportsChange(grpID, []int64{pressA.ID}, false)
	testutil.MustNoErr(t, err, "narrow")
	if !strings.Contains(g.Blocked, "BIN-SCOPE-1") || !strings.Contains(g.Blocked, "PRESS-SCOPE-B") {
		t.Errorf("blocked = %q, want it to name the carrier and the dropped process", g.Blocked)
	}
}

// Narrowing allowed types is scoped to the carriers ACTUALLY affected. Reciting
// every resident when one type is dropped is a refusal an operator learns to
// force past without reading.
func TestKanbanGuard_TypeNarrowingNamesOnlyStranded(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-NARROW", 2)
	big := mgBinType(t, db, "NARROW-BIG")
	small := mgBinType(t, db, "NARROW-SMALL")
	mgPutBin(t, db, "BIN-BIG-1", big, kids[0].ID)
	mgPutBin(t, db, "BIN-SMALL-1", small, kids[1].ID)

	// Keep only the small type: the big carrier is the one stranded.
	g, err := svc.CheckKanbanTypesChange(grpID, []int64{small}, false)
	testutil.MustNoErr(t, err, "narrow to small")
	if !strings.Contains(g.Blocked, "BIN-BIG-1") {
		t.Fatalf("blocked = %q, want it to name the stranded carrier", g.Blocked)
	}
	if strings.Contains(g.Blocked, "BIN-SMALL-1") {
		t.Errorf("blocked = %q, must not recite the carrier this change does not affect", g.Blocked)
	}

	// The empty set means NO RESTRICTION — the resolver's own reading — so it
	// strands nothing and is not a narrowing at all.
	g, err = svc.CheckKanbanTypesChange(grpID, nil, false)
	testutil.MustNoErr(t, err, "clear restriction")
	if g.Blocked != "" {
		t.Errorf("clearing the restriction was treated as a narrowing: %q", g.Blocked)
	}

	// Keeping both asks nothing.
	g, err = svc.CheckKanbanTypesChange(grpID, []int64{big, small}, false)
	testutil.MustNoErr(t, err, "keep both")
	if g.Blocked != "" {
		t.Errorf("keeping every resident's type was blocked: %q", g.Blocked)
	}
}
