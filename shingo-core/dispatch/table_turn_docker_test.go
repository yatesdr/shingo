//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/fleet"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
)

// turnDoorBackend is a fleet that can turn a table and reports each
// location's Wait key as the test sets it: waits[location] is the Wait key's
// raw parameters, a location missing from it has no Wait key, and one missing
// from exists is not a bin location at all.
type turnDoorBackend struct {
	*testdb.MockBackend
	waits  map[string]string
	exists map[string]bool
	err    error
}

func (b *turnDoorBackend) CheckLocationTasks(locations []string) ([]fleet.LocationTasks, error) {
	if b.err != nil {
		return nil, b.err
	}
	var out []fleet.LocationTasks
	for _, l := range locations {
		lt := fleet.LocationTasks{Location: l, Exists: b.exists[l]}
		if w, ok := b.waits[l]; ok {
			lt.TaskNames = []string{"Wait"}
			lt.TaskArgs = map[string]json.RawMessage{"Wait": json.RawMessage(w)}
		}
		out = append(out, lt)
	}
	return out, nil
}

const (
	waitRecognizes = `{"operation":"Wait","recfile":"shelf/Hop_Carrier.shelf","recognize":true}`
	waitPlain      = `{"operation":"Wait"}`
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
		waits: map[string]string{
			f.recognizing.Name: waitRecognizes, f.plain.Name: waitPlain, f.slot.Name: waitRecognizes,
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
