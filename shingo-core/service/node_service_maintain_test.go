//go:build docker

package service

import (
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
)

// Save-time rules for a maintained group.
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

func mgRefusedBy(chk MaintainedGroupCheck, substr string) bool {
	for _, r := range chk.Refusals {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func mgWarnedAbout(chk MaintainedGroupCheck, substr string) bool {
	for _, wm := range chk.Warnings {
		if strings.Contains(wm, substr) {
			return true
		}
	}
	return false
}

// A group cannot be both a loader's staging group and a maintained group: two
// owners would hold one level, each reading the other's work as the level moving
// by itself.
func TestMaintainedGroup_RefusesLoaderStagingGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-DOUBLE-OWNED", 2)
	_, err := db.CreateLoader(loaders.Loader{
		Name: "CT-L", Role: "produce", Layout: "shared_window", Replenishment: "operator",
		BufferDest: "GRP-DOUBLE-OWNED",
	})
	testutil.MustNoErr(t, err, "CreateLoader")

	chk, err2 := svc.CheckMaintainedGroupSettings(grpID, MaintainedGroupSettings{
		MaintainEnabled: true, MaintenanceStation: "EDGE-1",
	})
	testutil.MustNoErr(t, err2, "CheckMaintainedGroupSettings")
	if !mgRefusedBy(chk, "CT-L") {
		t.Fatalf("refusals = %v, want one naming the loader CT-L", chk.Refusals)
	}
}

// projectOrder no-ops on a blank StationID, so a top-up order minted without a
// station would run on the floor and appear on no board anywhere. Refused rather
// than warned because nothing downstream can detect it afterwards.
func TestMaintainedGroup_RefusesEnabledWithoutStation(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-NO-STATION", 2)

	chk, err := svc.CheckMaintainedGroupSettings(grpID, MaintainedGroupSettings{MaintainEnabled: true})
	testutil.MustNoErr(t, err, "CheckMaintainedGroupSettings enabled")
	if !mgRefusedBy(chk, "station") {
		t.Fatalf("refusals = %v, want one about the missing station", chk.Refusals)
	}

	// Not maintained: the station is nobody's business, and a group being
	// configured must be able to sit in that state.
	chk, err = svc.CheckMaintainedGroupSettings(grpID, MaintainedGroupSettings{MaintainEnabled: false})
	testutil.MustNoErr(t, err, "CheckMaintainedGroupSettings disabled")
	if chk.Err() != nil {
		t.Fatalf("a disabled group with no station was refused: %v", chk.Refusals)
	}
}

// Maintained groups are depth-1. A lane child means carriers can be buried, and
// a level counted over buried carriers is a number whose meaning changes with
// what is parked in front of it.
func TestMaintainedGroup_RefusesNonFlatGroup(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-DEEP", 1)
	_, err := nodes.AddLane(db.DB, grpID, "GRP-DEEP-LANE-1")
	testutil.MustNoErr(t, err, "AddLane")

	chk, err := svc.CheckMaintainedGroupSettings(grpID, MaintainedGroupSettings{
		MaintainEnabled: true, MaintenanceStation: "EDGE-1",
	})
	testutil.MustNoErr(t, err, "CheckMaintainedGroupSettings")
	if !mgRefusedBy(chk, "GRP-DEEP-LANE-1") {
		t.Fatalf("refusals = %v, want one naming the lane", chk.Refusals)
	}
}

// A declared level whose carrier type no enabled position accepts is a
// permanent, silent shortfall: the keeper would ask forever and every placement
// would be refused.
func TestMaintainedGroup_RefusesLevelNoChildAccepts(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, kids := mgGroup(t, db, "GRP-WRONG-TYPE", 2)
	big := mgBinType(t, db, "45x58x32")
	small := mgBinType(t, db, "45x48x24")

	// Both positions take only the small carrier.
	for _, k := range kids {
		testutil.MustNoErr(t, db.SetNodeProperty(k.ID, nodes.PropBinTypeMode, "specific"), "bin_type_mode")
		testutil.MustNoErr(t, db.SetNodeBinTypes(k.ID, []int64{small}), "SetNodeBinTypes")
	}

	chk, err := svc.SetMaintainLevel(grpID, big, 4)
	testutil.MustNoErr(t, err, "SetMaintainLevel big")
	if !mgRefusedBy(chk, "45x58x32") {
		t.Fatalf("refusals = %v, want one naming the carrier type no position accepts", chk.Refusals)
	}
	// Refused means NOT WRITTEN. A refusal that still persisted the row would be
	// a warning wearing a refusal's message.
	levels, err := svc.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	if len(levels) != 0 {
		t.Fatalf("levels after a refused save = %+v, want none written", levels)
	}

	// The type the positions DO accept goes through.
	chk, err = svc.SetMaintainLevel(grpID, small, 1)
	testutil.MustNoErr(t, err, "SetMaintainLevel small")
	if chk.Err() != nil {
		t.Fatalf("an accepted carrier type was refused: %v", chk.Refusals)
	}
}

// An unrestricted position takes any carrier, so nothing about a declared type
// can be impossible there. Same predicate the resolver uses — an empty effective
// set means no restriction, and a config screen that read it as "accepts
// nothing" would refuse what the floor allows.
func TestMaintainedGroup_UnrestrictedChildAcceptsAnyLevel(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-OPEN", 3)
	bt := mgBinType(t, db, "OPEN-TOTE")

	chk, err := svc.SetMaintainLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "SetMaintainLevel")
	if chk.Err() != nil {
		t.Fatalf("a level on a group of unrestricted positions was refused: %v", chk.Refusals)
	}
}

// The episode key is `mnt|<group>|<type code>`, so a code carrying the separator
// would parse back into different components than it was built from. Refused at
// the only point where a person can still choose a different code.
func TestMaintainedGroup_RefusesPipeInBinTypeCode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-PIPE", 2)
	bad := mgBinType(t, db, "45x58|32")

	chk, err := svc.SetMaintainLevel(grpID, bad, 1)
	testutil.MustNoErr(t, err, "SetMaintainLevel")
	if !mgRefusedBy(chk, "45x58|32") {
		t.Fatalf("refusals = %v, want one naming the carrier code with the separator", chk.Refusals)
	}
}

// Declaring a level on a group whose bin_type_mode was never set writes it
// explicitly. Inherit-by-default would let an ancestor's allowed-bins list
// silently govern a group that has just been told to hold specific types.
func TestMaintainedGroup_FirstLevelPinsBinTypeMode(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-MODE", 2)
	bt := mgBinType(t, db, "MODE-TOTE")

	if got := db.GetNodeProperty(grpID, nodes.PropBinTypeMode); got != "" {
		t.Fatalf("bin_type_mode on a fresh group = %q, want unset", got)
	}
	chk, err := svc.SetMaintainLevel(grpID, bt, 1)
	testutil.MustNoErr(t, err, "SetMaintainLevel")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if got := db.GetNodeProperty(grpID, nodes.PropBinTypeMode); got != nodes.BinTypeModeAll {
		t.Errorf("bin_type_mode after first level = %q, want %q", got, nodes.BinTypeModeAll)
	}

	// A mode somebody chose is never overwritten.
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropBinTypeMode, "specific"), "choose specific")
	testutil.MustNoErr(t, db.SetNodeBinTypes(grpID, []int64{bt}), "SetNodeBinTypes")
	_, err = svc.SetMaintainLevel(grpID, bt, 2)
	testutil.MustNoErr(t, err, "SetMaintainLevel again")
	if got := db.GetNodeProperty(grpID, nodes.PropBinTypeMode); got != "specific" {
		t.Errorf("bin_type_mode after a second level = %q, want the chosen %q", got, "specific")
	}
}

// Σwant filling every position leaves nowhere for a carrier coming back in.
// A WARNING, not a refusal: the group can gain positions or re-enable children,
// and the runtime guard is the resolver refusing a push at level — not this
// arithmetic.
func TestMaintainedGroup_WarnsWhenLevelFillsEveryPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-FULL", 2)
	bt := mgBinType(t, db, "FULL-TOTE")

	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintainEnabled, "on"), "enable")
	testutil.MustNoErr(t, db.SetNodeProperty(grpID, nodes.PropMaintenanceStation, "EDGE-1"), "station")

	chk, err := svc.SetMaintainLevel(grpID, bt, 2)
	testutil.MustNoErr(t, err, "SetMaintainLevel")
	if chk.Err() != nil {
		t.Fatalf("a full-house level was REFUSED; it must only warn: %v", chk.Refusals)
	}
	if !mgWarnedAbout(chk, "GRP-FULL") {
		t.Fatalf("warnings = %v, want one about leaving no free position", chk.Warnings)
	}
	// It saved.
	levels, err := svc.ListMaintainLevels(grpID)
	testutil.MustNoErr(t, err, "ListMaintainLevels")
	if len(levels) != 1 || levels[0].Want != 2 {
		t.Errorf("levels = %+v, want the warned-about level written anyway", levels)
	}
}

// A supported position with no carrier types set cannot type the empty pull that
// goes to it. A warning because it is a data gap rather than a contradiction —
// and because it is the normal state today.
func TestMaintainedGroup_WarnsOnUntypedSupportedPosition(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	svc := NewNodeService(db)

	grpID, _ := mgGroup(t, db, "GRP-SUP", 4)
	press := &nodes.Node{Name: "PRESS-UNTYPED", Enabled: true}
	testutil.MustNoErr(t, db.CreateNode(press), "create press")

	chk, err := svc.SetMaintainSupports(grpID, []int64{press.ID})
	testutil.MustNoErr(t, err, "SetMaintainSupports")
	if chk.Err() != nil {
		t.Fatalf("refused: %v", chk.Refusals)
	}
	if !mgWarnedAbout(chk, "PRESS-UNTYPED") {
		t.Fatalf("warnings = %v, want one naming the untyped position", chk.Warnings)
	}
	got, err := svc.ListMaintainSupports(grpID)
	testutil.MustNoErr(t, err, "ListMaintainSupports")
	if len(got) != 1 {
		t.Errorf("supports = %+v, want the warned-about support written anyway", got)
	}
}
