//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// turnDoorBackend is a fleet that can turn a table and reports each
// location's binTask keys as the test sets them: tasks[location] is the keys
// the location lists, each with its raw parameters, and a location missing
// from exists is not a bin location at all. reads counts the fleet reads.
type turnDoorBackend struct {
	*testdb.MockBackend
	tasks  map[string]map[string]string
	exists map[string]bool
	err    error
	reads  int
}

func (b *turnDoorBackend) CheckLocationTasks(locations []string) ([]fleet.LocationTasks, error) {
	b.reads++
	if b.err != nil {
		return nil, b.err
	}
	var out []fleet.LocationTasks
	for _, l := range locations {
		lt := fleet.LocationTasks{Location: l, Exists: b.exists[l]}
		for _, key := range slices.Sorted(maps.Keys(b.tasks[l])) {
			if lt.TaskArgs == nil {
				lt.TaskArgs = map[string]json.RawMessage{}
			}
			lt.TaskNames = append(lt.TaskNames, key)
			lt.TaskArgs[key] = json.RawMessage(b.tasks[l][key])
		}
		out = append(out, lt)
	}
	return out, nil
}

const (
	waitRecognizes = `{"operation":"Wait","recfile":"shelf/Hop_Carrier.shelf","recognize":true}`
	waitPlain      = `{"operation":"Wait"}`
	jackLoad       = `{"operation":"JackLoad","recfile":"shelf/Hop_Carrier.shelf","recognize":true}`
	jackUnload     = `{"operation":"JackUnload"}`
)

// turnDoorFixture is a dispatcher on a turn-capable fleet with one physical
// node per Wait-key shape, a group, a lane and a lane slot.
type turnDoorFixture struct {
	db                                     *store.DB
	d                                      *Dispatcher
	be                                     *turnDoorBackend
	recognizing, plain, noWait, notInFleet *nodes.Node
	group, lane, slot                      *nodes.Node
}

func newTurnDoorFixture(t *testing.T) *turnDoorFixture {
	t.Helper()
	db := testdb.Open(t)
	f := &turnDoorFixture{db: db}
	mk := func(n *nodes.Node) *nodes.Node {
		testutil.MustNoErr(t, db.CreateNode(n), n.Name)
		got, err := db.GetNode(n.ID)
		return testutil.Must(t, got, err, "read "+n.Name)
	}
	f.recognizing = mk(&nodes.Node{Name: "TT-REC", Enabled: true})
	f.plain = mk(&nodes.Node{Name: "TT-PLAIN", Enabled: true})
	f.noWait = mk(&nodes.Node{Name: "TT-NOWAIT", Enabled: true})
	f.notInFleet = mk(&nodes.Node{Name: "TT-GONE", Enabled: true})
	laneType, err := db.GetNodeTypeByCode("LANE")
	testutil.MustNoErr(t, err, "LANE type")
	groupID := testdb.MarkedGroup(t, db, "TT-GRP")
	g, err := db.GetNode(*groupID)
	f.group = testutil.Must(t, g, err, "read group")
	f.lane = mk(&nodes.Node{Name: "TT-LANE", IsSynthetic: true, Enabled: true, NodeTypeID: &laneType.ID, ParentID: groupID})
	f.slot = mk(&nodes.Node{Name: "TT-SLOT", Enabled: true, ParentID: &f.lane.ID})

	f.be = &turnDoorBackend{
		MockBackend: testdb.NewSuccessBackend(),
		tasks: map[string]map[string]string{
			f.recognizing.Name: {"Wait": waitRecognizes},
			f.plain.Name:       {"Wait": waitPlain},
			f.noWait.Name:      {"JackLoad": jackLoad, "JackUnload": jackUnload},
			f.slot.Name:        {"Wait": waitRecognizes},
		},
		exists: map[string]bool{f.recognizing.Name: true, f.plain.Name: true, f.noWait.Name: true, f.slot.Name: true},
	}
	f.d, _ = newTestDispatcher(t, db, f.be)
	f.d.SetTableTurner(f.be, nil)
	return f
}

// TestTableTurnDoor_RefusesATurnThatCannotWork pins every refusal of the
// property door (PLAN §3), each against the one fact that makes it fail.
func TestTableTurnDoor_RefusesATurnThatCannotWork(t *testing.T) {
	f := newTurnDoorFixture(t)
	for _, tc := range []struct {
		name  string
		node  *nodes.Node
		key   string
		value string
		want  string
	}{
		{"not a number", f.recognizing, PropTableTurnRad, "ninety", "0 < |angle| < π"},
		{"zero", f.recognizing, PropTableTurnRad, "0", "0 < |angle| < π"},
		{"π", f.recognizing, PropTableTurnRad, "3.141592653589793", "0 < |angle| < π"},
		{"past π", f.recognizing, PropTableTurnRad, "-3.5", "0 < |angle| < π"},
		{"not finite", f.recognizing, PropTableTurnRad, "NaN", "0 < |angle| < π"},
		{"infinite", f.recognizing, PropTableTurnRad, "+Inf", "0 < |angle| < π"},
		{"an unknown side", f.recognizing, PropTableTurnSide, "after", "must be"},
		{"a group", f.group, PropTableTurnRad, "1.57", "synthetic"},
		{"a lane", f.lane, PropTableTurnRad, "1.57", "synthetic"},
		{"a lane slot", f.slot, PropTableTurnRad, "1.57", "slot in lane TT-LANE"},
		{"lift on a Wait that does not recognize", f.plain, PropTableTurnRad, "1.57", "does not recognize"},
		{"lift with no Wait key", f.noWait, PropTableTurnRad, "1.57", "no Wait key"},
		{"lift on a node the fleet does not have", f.notInFleet, PropTableTurnRad, "1.57", "no bin location"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := f.d.CheckTableTurnProperty(tc.node.ID, tc.key, tc.value, false)
			if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("CheckTableTurnProperty(%s, %s=%q) = %v, want a refusal mentioning %q",
					tc.node.Name, tc.key, tc.value, err, tc.want)
			}
		})
	}

	t.Run("a fleet that cannot turn a table", func(t *testing.T) {
		d := NewDispatcher(f.db, f.be, &mockEmitter{}, "core", "shingo.dispatch", nil)
		err := d.CheckTableTurnProperty(f.recognizing.ID, PropTableTurnRad, "1.57", false)
		if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "cannot turn") {
			t.Fatalf("no turner wired: got %v, want a refusal", err)
		}
	})
	t.Run("set-down on a Wait that recognizes", func(t *testing.T) {
		testutil.MustNoErr(t, f.db.SetNodeProperty(f.recognizing.ID, PropTableTurnSide, TableTurnSideSetDown), "side")
		defer func() {
			testutil.MustNoErr(t, f.db.DeleteNodeProperty(f.recognizing.ID, PropTableTurnSide), "reset side")
		}()
		err := f.d.CheckTableTurnProperty(f.recognizing.ID, PropTableTurnRad, "1.57", false)
		if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "recognizes the cart") {
			t.Fatalf("an angle with the set-down side on a recognizing Wait: got %v, want a refusal", err)
		}
		// And the other way round: the side moved to set-down under a standing angle.
		testutil.MustNoErr(t, f.db.DeleteNodeProperty(f.recognizing.ID, PropTableTurnSide), "reset side")
		testutil.MustNoErr(t, f.db.SetNodeProperty(f.recognizing.ID, PropTableTurnRad, "1.57"), "angle")
		defer func() {
			testutil.MustNoErr(t, f.db.DeleteNodeProperty(f.recognizing.ID, PropTableTurnRad), "reset angle")
		}()
		err = f.d.CheckTableTurnProperty(f.recognizing.ID, PropTableTurnSide, TableTurnSideSetDown, false)
		if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "recognizes the cart") {
			t.Fatalf("the side moved to set-down under an angle on a recognizing Wait: got %v, want a refusal", err)
		}
	})
	t.Run("a fleet whose Wait key cannot be read", func(t *testing.T) {
		f.be.err = errors.New("rds down")
		defer func() { f.be.err = nil }()
		err := f.d.CheckTableTurnProperty(f.recognizing.ID, PropTableTurnRad, "1.57", false)
		if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "could not read") {
			t.Fatalf("unreadable Wait key: got %v, want a refusal", err)
		}
	})
}

// TestTableTurnDoor_AdmitsAndCaches is the other half: a lift-side angle on a
// recognizing Wait key is admitted, clearing is always admitted, and the cache
// every reader asks follows the store on reload.
func TestTableTurnDoor_AdmitsAndCaches(t *testing.T) {
	f := newTurnDoorFixture(t)
	for _, tc := range []struct {
		name  string
		key   string
		value string
		del   bool
	}{
		{"90° before the lift", PropTableTurnRad, "1.5707963267948966", false},
		{"-90°", PropTableTurnRad, "-1.57", false},
		{"the lift side, said out loud", PropTableTurnSide, TableTurnSideLift, false},
		{"the side cleared (back to the lift)", PropTableTurnSide, "", true},
		{"cleared by an empty value", PropTableTurnRad, "", false},
		{"cleared by a delete", PropTableTurnRad, "", true},
	} {
		if err := f.d.CheckTableTurnProperty(f.recognizing.ID, tc.key, tc.value, tc.del); err != nil {
			t.Errorf("%s: refused: %v", tc.name, err)
		}
	}
	// The set-down side wants a Wait key that does NOT recognize: a loaded robot
	// pausing there has nothing under it to find. One key holds the side, so a
	// node turns before the lift or before the set-down, never both.
	testutil.MustNoErr(t, f.db.SetNodeProperty(f.plain.ID, PropTableTurnSide, TableTurnSideSetDown), "side")
	if err := f.d.CheckTableTurnProperty(f.plain.ID, PropTableTurnRad, "1.57", false); err != nil {
		t.Errorf("a set-down angle on a Wait that does not recognize: refused: %v", err)
	}
	// A non-turn property is not the door's business, on any node.
	if err := f.d.CheckTableTurnProperty(f.slot.ID, "lane_gate_point", "X", false); err != nil {
		t.Errorf("a non-turn key was refused: %v", err)
	}

	if _, ok := f.d.turnAt(f.recognizing.Name); ok {
		t.Fatal("a node with no angle reads as a turn node")
	}
	testutil.MustNoErr(t, f.db.SetNodeProperty(f.recognizing.ID, PropTableTurnRad, "1.57"), "set angle")
	testutil.MustNoErr(t, f.d.ReloadTableTurns(), "reload")
	got, ok := f.d.turnAt(f.recognizing.Name)
	if !ok || got.rad != 1.57 || got.side != TableTurnSideLift {
		t.Fatalf("after reload: turnAt = %+v %v, want 1.57 before the lift (the side's default)", got, ok)
	}
	testutil.MustNoErr(t, f.db.SetNodeProperty(f.recognizing.ID, PropTableTurnRad, ""), "clear angle")
	testutil.MustNoErr(t, f.d.ReloadTableTurns(), "reload")
	if _, ok := f.d.turnAt(f.recognizing.Name); ok {
		t.Fatal("a cleared angle still reads as a turn node")
	}
}

// TestTableTurnDoor_ChecksEveryWaitKeyTheLocationLists: with a family keyed to
// wait_4548, the door checks Wait and wait_4548 wherever the location lists
// them, each against the side: before the lift each must recognize, before the
// set-down none may. A family's key the location does not list is not checked.
// A location that does not list Wait is refused on either side, whatever
// family key it lists, as is one that lists no wait key at all.
func TestTableTurnDoor_ChecksEveryWaitKeyTheLocationLists(t *testing.T) {
	f := newTurnDoorFixture(t)
	fam := keyFamily(t, f.db, "TT-4548")
	testutil.MustNoErr(t, f.db.SetCarrierFamilyKeys(fam, "jackload_4548", "wait_4548"), "key 4548")
	_, err := f.d.ReloadCarrierKeys()
	testutil.MustNoErr(t, err, "load the keyed families")
	// TT-KEYS stands at 90°; each case gives it its keys and writes a side.
	n := &nodes.Node{Name: "TT-KEYS", Enabled: true}
	testutil.MustNoErr(t, f.db.CreateNode(n), n.Name)
	testutil.MustNoErr(t, f.db.SetNodeProperty(n.ID, PropTableTurnRad, "1.57"), "angle")
	f.be.exists[n.Name] = true

	lift, setDown := TableTurnSideLift, TableTurnSideSetDown
	noWaitKey := map[string]string{"JackLoad": jackLoad, "JackUnload": jackUnload}
	for _, tc := range []struct {
		name string
		side string
		keys map[string]string
		want string // "" is admitted; otherwise the refusal says this
	}{
		{"lift: Wait and wait_4548 both recognize", lift,
			map[string]string{"Wait": waitRecognizes, "wait_4548": waitRecognizes}, ""},
		{"lift: wait_4548 does not recognize", lift,
			map[string]string{"Wait": waitRecognizes, "wait_4548": waitPlain}, "TT-KEYS's wait_4548 key does not recognize"},
		{"lift: Wait does not recognize beside a recognizing wait_4548", lift,
			map[string]string{"Wait": waitPlain, "wait_4548": waitRecognizes}, "TT-KEYS's Wait key does not recognize"},
		{"lift: wait_4548 not listed is not checked", lift,
			map[string]string{"Wait": waitRecognizes}, ""},
		{"lift: wait_4548 listed without Wait", lift,
			map[string]string{"wait_4548": waitRecognizes}, "TT-KEYS has no Wait key"},
		{"set-down: wait_4548 listed without Wait", setDown,
			map[string]string{"wait_4548": waitPlain}, "TT-KEYS has no Wait key"},
		{"set-down: wait_4548 recognizes", setDown,
			map[string]string{"Wait": waitPlain, "wait_4548": waitRecognizes}, "TT-KEYS's wait_4548 key recognizes the cart"},
		{"set-down: Wait recognizes beside a plain wait_4548", setDown,
			map[string]string{"Wait": waitRecognizes, "wait_4548": waitPlain}, "TT-KEYS's Wait key recognizes the cart"},
		{"set-down: neither recognizes", setDown,
			map[string]string{"Wait": waitPlain, "wait_4548": waitPlain}, ""},
		{"lift: no wait key listed", lift, noWaitKey, "TT-KEYS has no Wait key"},
		{"set-down: no wait key listed", setDown, noWaitKey, "TT-KEYS has no Wait key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.be.tasks[n.Name] = tc.keys
			err := f.d.CheckTableTurnProperty(n.ID, PropTableTurnSide, tc.side, false)
			if tc.want == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want a refusal saying %q", err, tc.want)
			}
		})
	}
}

// TestTableTurnDoor_ReadsTheFamiliesWhenTheSetNeverLoaded: a dispatcher that
// never loaded the keyed families (a start whose load failed) reads them before
// it checks, so a family's wait key the location lists is not skipped.
func TestTableTurnDoor_ReadsTheFamiliesWhenTheSetNeverLoaded(t *testing.T) {
	f := newTurnDoorFixture(t)
	if f.d.familyKeys.Load() != nil {
		t.Fatal("the fixture's dispatcher already holds the keyed families; this test needs one that never loaded them")
	}
	fam := keyFamily(t, f.db, "TT-4548")
	testutil.MustNoErr(t, f.db.SetCarrierFamilyKeys(fam, "", "wait_4548"), "key 4548")
	f.be.tasks[f.recognizing.Name]["wait_4548"] = waitPlain
	err := f.d.CheckTableTurnProperty(f.recognizing.ID, PropTableTurnRad, "1.57", false)
	if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "TT-REC's wait_4548 key does not recognize") {
		t.Fatalf("a lift-side angle where the unloaded family's wait key does not recognize: got %v, want a refusal naming it", err)
	}
}

// setTurn gives a node a turn on side, written around the door, and reloads
// the turn nodes as the property door's write does.
func (f *turnDoorFixture) setTurn(t *testing.T, n *nodes.Node, side string) {
	t.Helper()
	testutil.MustNoErr(t, f.db.SetNodeProperty(n.ID, PropTableTurnRad, "1.57"), "angle at "+n.Name)
	testutil.MustNoErr(t, f.db.SetNodeProperty(n.ID, PropTableTurnSide, side), "side at "+n.Name)
	testutil.MustNoErr(t, f.d.ReloadTableTurns(), "reload the turn nodes")
}

// storedFamilyKeys is a family's two keys as the table holds them.
func storedFamilyKeys(t *testing.T, db *store.DB, id int64) familyKeys {
	t.Helper()
	fams, err := db.ListCarrierFamilies()
	testutil.MustNoErr(t, err, "list the families")
	for _, fam := range fams {
		if fam.ID == id {
			return familyKeys{lift: fam.LiftBinTask, wait: fam.WaitBinTask}
		}
	}
	t.Fatalf("family %d is not in the table", id)
	return familyKeys{}
}

// TestTableTurnDoor_AFamilyWaitKeySaveIsCheckedAtEveryTurnNode: a family's wait
// key is saved only when every turn node, on its own side, still passes the
// door's check with it. A key a lift-side node lists without recognize, or a
// set-down-side node lists with it, is refused naming the node and the key, and
// nothing is written; a key no turn node lists saves after one fleet read for
// both nodes; a fleet that cannot answer refuses.
func TestTableTurnDoor_AFamilyWaitKeySaveIsCheckedAtEveryTurnNode(t *testing.T) {
	f := newTurnDoorFixture(t)
	fam := keyFamily(t, f.db, "TT-4548")
	f.setTurn(t, f.recognizing, TableTurnSideLift)
	f.setTurn(t, f.plain, TableTurnSideSetDown)
	f.be.tasks[f.recognizing.Name]["wait_4548"] = waitPlain
	f.be.tasks[f.plain.Name]["wait_big"] = waitRecognizes
	_, err := f.d.ReloadCarrierKeys()
	testutil.MustNoErr(t, err, "load the keyed families")
	unchanged := func(when string, want familyKeys) {
		t.Helper()
		if got := storedFamilyKeys(t, f.db, fam); got != want {
			t.Errorf("%s: the table holds %+v, want %+v (nothing written)", when, got, want)
		}
		if got := f.d.carrierKeys()[fam]; got != want {
			t.Errorf("%s: the keyed set holds %+v, want %+v", when, got, want)
		}
	}

	for _, tc := range []struct{ wait, want string }{
		{"wait_4548", "TT-REC's wait_4548 key does not recognize"},
		{"wait_big", "TT-PLAIN's wait_big key recognizes the cart"},
	} {
		err := f.d.SaveCarrierFamilyKeys(fam, "jackload_4548", tc.wait)
		if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), tc.want) ||
			!strings.Contains(err.Error(), `"`+tc.wait+`" is not saved`) {
			t.Errorf("saving %s: got %v, want a refusal of the key saying %q", tc.wait, err, tc.want)
		}
		unchanged("refused "+tc.wait, familyKeys{})
	}

	f.be.reads = 0
	testutil.MustNoErr(t, f.d.SaveCarrierFamilyKeys(fam, "jackload_4548", "wait_other"), "a key no turn node lists")
	if f.be.reads != 1 {
		t.Errorf("the save read the fleet %d times, want once for both turn nodes", f.be.reads)
	}
	unchanged("saved wait_other", familyKeys{lift: "jackload_4548", wait: "wait_other"})

	f.be.err = errors.New("rds down")
	err = f.d.SaveCarrierFamilyKeys(fam, "jackload_4548", "wait_third")
	f.be.err = nil
	if !errors.Is(err, ErrTableTurnRefused) || !strings.Contains(err.Error(), "could not read") {
		t.Errorf("saving while the fleet cannot answer: got %v, want a refusal", err)
	}
	unchanged("fleet down", familyKeys{lift: "jackload_4548", wait: "wait_other"})
}

// TestTableTurnDoor_ASaveThatCannotMoveATurnReadsNoFleet: a family save on a
// plant with no turn node, and a save that changes only the lift key on a plant
// with one, make no fleet read. The wait key changing on that same plant does
// read, so the zero counts are the code's and not the counter's.
func TestTableTurnDoor_ASaveThatCannotMoveATurnReadsNoFleet(t *testing.T) {
	f := newTurnDoorFixture(t)
	fam := keyFamily(t, f.db, "TT-4548")
	f.be.tasks[f.recognizing.Name]["wait_4548"] = waitPlain // would refuse, were it read
	_, err := f.d.ReloadCarrierKeys()
	testutil.MustNoErr(t, err, "load the keyed families")

	testutil.MustNoErr(t, f.d.SaveCarrierFamilyKeys(fam, "", "wait_4548"), "a wait key on a plant with no turn node")
	if f.be.reads != 0 {
		t.Errorf("a plant with no turn node: %d fleet reads, want none", f.be.reads)
	}
	f.setTurn(t, f.recognizing, TableTurnSideLift)
	testutil.MustNoErr(t, f.d.SaveCarrierFamilyKeys(fam, "jackload_4548", "wait_4548"), "the lift key alone")
	if f.be.reads != 0 {
		t.Errorf("a lift key changed alone: %d fleet reads, want none", f.be.reads)
	}
	if got := storedFamilyKeys(t, f.db, fam); got != (familyKeys{lift: "jackload_4548", wait: "wait_4548"}) {
		t.Errorf("after both saves the table holds %+v", got)
	}
	testutil.MustNoErr(t, f.d.SaveCarrierFamilyKeys(fam, "jackload_4548", "wait_other"), "a wait key no node lists")
	if f.be.reads != 1 {
		t.Errorf("a wait key changed on a plant with a turn node: %d fleet reads, want one", f.be.reads)
	}
}
