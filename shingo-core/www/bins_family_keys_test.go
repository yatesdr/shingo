package www

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"shingo/protocol/testutil"
	"shingocore/dispatch"
	"shingocore/domain"
)

// sceneLocation is a bin location as the scene sync stores one, listing keys
// (name, operation; "" for none) in order. With no keys its binTask property
// is present with no value, as ten Hopkinsville locations have it (2026-11-17).
func sceneLocation(t *testing.T, name string, synced time.Time, keys ...[2]string) *domain.ScenePoint {
	t.Helper()
	prop := map[string]any{"key": "binTask", "type": "json"}
	if len(keys) > 0 {
		tasks := make([]map[string]map[string]any, len(keys))
		for i, k := range keys {
			params := map[string]any{"recognize": true}
			if k[1] != "" {
				params["operation"] = k[1]
			}
			tasks[i] = map[string]map[string]any{k[0]: params}
		}
		value, err := json.Marshal(tasks)
		testutil.MustNoErr(t, err, "marshal the binTask of "+name)
		prop["stringValue"] = string(value)
	}
	props, err := json.Marshal([]any{map[string]any{"key": "points", "type": "json", "stringValue": "[]"}, prop})
	testutil.MustNoErr(t, err, "marshal the properties of "+name)
	return &domain.ScenePoint{InstanceName: name, ClassName: "GeneralLocation", PropertiesJSON: string(props), SyncedAt: synced}
}

// optionLines is a picker as "value|label · note", with a trailing "*" on the
// selected option.
func optionLines(opts []keyOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Value + "|" + optionText(o)
		if o.Selected {
			out[i] += "*"
		}
	}
	return out
}

// optionText is a row as the picker draws it: the label, and its note after.
func optionText(o keyOption) string {
	if o.Note == "" {
		return o.Label
	}
	return o.Label + " · " + o.Note
}

var fkSynced = time.Date(2026, 11, 17, 12, 16, 0, 0, time.UTC)

// fkScenePoints is a scene copy with every shape a location's keys come in:
// the three plain keys, sized keys, a key with no operation, one key given two
// operations at two locations, a binTask property with no value and a location
// with no binTask property at all.
func fkScenePoints(t *testing.T) []*domain.ScenePoint {
	return []*domain.ScenePoint{
		sceneLocation(t, "FK-1", fkSynced, [2]string{"JackLoad", "JackLoad"}, [2]string{"JackUnload", "JackUnload"},
			[2]string{"Wait", "Wait"}),
		sceneLocation(t, "FK-2", fkSynced.Add(time.Hour), [2]string{"JackLoad", "JackLoad"},
			[2]string{"jackload_4548", "JackLoad"}, [2]string{"wait_4548", "Wait"}),
		sceneLocation(t, "FK-3", fkSynced.Add(-time.Hour)),
		sceneLocation(t, "FK-4", fkSynced, [2]string{"lift_plain", ""}, [2]string{"wait_4548", "Wait2"}),
		{InstanceName: "FK-5", ClassName: "GeneralLocation", PropertiesJSON: "[]", SyncedAt: fkSynced},
	}
}

// TestGatherSceneKeys_EachKeyOnceWithItsOperations: the pickers' options are
// the distinct keys the bin locations list, each with every operation RDS
// gives it, in one order whatever order the locations are read in; the sync
// time is the newest location's.
func TestGatherSceneKeys_EachKeyOnceWithItsOperations(t *testing.T) {
	t.Parallel()
	points := fkScenePoints(t)
	want := []string{"JackLoad · JackLoad", "jackload_4548 · JackLoad", "JackUnload · JackUnload", "lift_plain",
		"Wait · Wait", "wait_4548 · Wait, Wait2"}
	reversed := slices.Clone(points)
	slices.Reverse(reversed)
	for _, order := range [][]*domain.ScenePoint{points, reversed} {
		got := gatherSceneKeys(order)
		labels := make([]string, len(got.Keys))
		for i, k := range got.Keys {
			labels[i] = k.Name
			if len(k.Operations) > 0 {
				labels[i] += " · " + strings.Join(k.Operations, ", ")
			}
		}
		if !reflect.DeepEqual(labels, want) {
			t.Errorf("keys = %q, want %q", labels, want)
		}
		if !got.SyncedAt.Equal(fkSynced.Add(time.Hour)) {
			t.Errorf("synced at %v, want the newest location's %v", got.SyncedAt, fkSynced.Add(time.Hour))
		}
	}
	if got := gatherSceneKeys(nil); len(got.Keys) != 0 || !got.SyncedAt.IsZero() || got.Err != nil {
		t.Errorf("no scene copy reads as %+v, want no keys and no sync time", got)
	}
}

// TestKeyOptions_DefaultFirstThenEveryKey: a picker offers the default first,
// stored blank and labelled with the key it sends, then every key the copy
// lists (a lift picker offers wait keys too: nothing is left out for its name
// or operation). A saved key the copy does not list stays, selected and
// marked "not in RDS"; when the copy could not be read it is not marked.
func TestKeyOptions_DefaultFirstThenEveryKey(t *testing.T) {
	t.Parallel()
	scene := gatherSceneKeys(fkScenePoints(t))
	listed := []string{"JackLoad|JackLoad · JackLoad", "jackload_4548|jackload_4548 · JackLoad",
		"JackUnload|JackUnload · JackUnload", "lift_plain|lift_plain", "Wait|Wait · Wait", "wait_4548|wait_4548 · Wait, Wait2"}
	selected := func(lines []string, value string) []string {
		out := slices.Clone(lines)
		for i, l := range out {
			if strings.HasPrefix(l, value+"|") {
				out[i] += "*"
			}
		}
		return out
	}
	cases := []struct {
		name, def, saved string
		scene            sceneKeyCopy
		want             []string
	}{
		{"blank is the default", "JackLoad", "", scene, append([]string{"|JackLoad (default)*"}, listed...)},
		{"a listed key is selected", "JackLoad", "jackload_4548", scene,
			append([]string{"|JackLoad (default)"}, selected(listed, "jackload_4548")...)},
		{"the wait picker offers every key too", "Wait", "wait_4548", scene,
			append([]string{"|Wait (default)"}, selected(listed, "wait_4548")...)},
		{"a saved key the copy lacks stays, marked", "Wait", "wait_9999", scene,
			append([]string{"|Wait (default)", "wait_9999|wait_9999 · not in RDS*"}, listed...)},
		{"no scene copy: the default and the saved key", "JackLoad", "jackload_4548", sceneKeyCopy{},
			[]string{"|JackLoad (default)", "jackload_4548|jackload_4548 · not in RDS*"}},
		{"an unread copy marks nothing", "JackLoad", "jackload_4548", sceneKeyCopy{Err: errors.New("down")},
			[]string{"|JackLoad (default)", "jackload_4548|jackload_4548*"}},
	}
	for _, c := range cases {
		if got := optionLines(keyOptions(c.def, c.saved, c.scene)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got  %q\n want %q", c.name, got, c.want)
		}
	}
}

// TestBinTypeFamilies_ATabPerFamilyItsKeysOnceItsTypesUnder: the Bin Types
// section is a tab per family, in family code order, counting its types; the
// family's keys are on the tab once (the defaults labelled with what Core sends,
// seerrds); its types are listed under it in code order. A bare marker is no
// line of its own but the line under the type it stands for, a tote carrier's
// included; a marker whose type the page did not read is a line of its own. A
// family whose keys were not read offers nothing to save.
func TestBinTypeFamilies_ATabPerFamilyItsKeysOnceItsTypesUnder(t *testing.T) {
	t.Parallel()
	scene := gatherSceneKeys(fkScenePoints(t)[:2])
	of := func(id int64) *int64 { return &id }
	eight := 8
	types := []*domain.BinType{
		{ID: 1, Code: "C4548-STD", FamilyID: 10, FamilyCode: "C4548"},
		{ID: 2, Code: "C4548-STD-BARE", FamilyID: 10, FamilyCode: "C4548", Bare: true, BareOf: of(1)},
		{ID: 3, Code: "C4548-HI", FamilyID: 10, FamilyCode: "C4548"},
		{ID: 4, Code: "TOTE-8", FamilyID: 20, FamilyCode: "TOTE-CART", TotesPerLayer: &eight},
		{ID: 5, Code: "TOTE-8-BARE", FamilyID: 20, FamilyCode: "TOTE-CART", Bare: true, BareOf: of(4)},
		{ID: 6, Code: "C3230", FamilyID: 30, FamilyCode: "C3230"},
		{ID: 7, Code: "LOST-BARE", FamilyID: 30, FamilyCode: "C3230", Bare: true, BareOf: of(99)},
		{ID: 8, Code: "UNREAD", FamilyID: 40, FamilyCode: "A-UNREAD"},
	}
	families := map[int64]pageFamilyKeys{10: {lift: "jackload_4548", wait: "wait_4548"}, 20: {}, 30: {}}
	tabs := binTypeFamilies(types, families, scene)
	sel := func(f *keyField) string {
		if f == nil {
			return "(not read)"
		}
		for _, o := range f.Options {
			if o.Selected {
				return optionText(o)
			}
		}
		return "(none)"
	}
	lines := func(tab binTypeFamilyTab) []string {
		var out []string
		for _, l := range tab.Types {
			line := l.Code
			if l.Bare {
				line += " (bare)"
			}
			if l.Marker != nil {
				line += " / " + l.Marker.Code
			}
			out = append(out, line)
		}
		return out
	}
	var got []string
	for _, tab := range tabs {
		got = append(got, fmt.Sprintf("%s %s %d %v | %s | %s", tab.Key(), tab.Code, len(tab.Types), lines(tab),
			sel(tab.LiftField), sel(tab.WaitField)))
	}
	want := []string{
		"bt-family-40 A-UNREAD 1 [UNREAD] | (not read) | (not read)",
		"bt-family-30 C3230 2 [C3230 LOST-BARE (bare)] | JackLoad (default) | Wait (default)",
		"bt-family-10 C4548 2 [C4548-HI C4548-STD / C4548-STD-BARE] | jackload_4548 · JackLoad | wait_4548 · Wait",
		"bt-family-20 TOTE-CART 1 [TOTE-8 / TOTE-8-BARE] | JackLoad (default) | Wait (default)",
	}
	if !slices.Equal(got, want) {
		t.Errorf("tabs:\n got  %q\n want %q", got, want)
	}
	if n := binTypeCount(tabs); n != 6 {
		t.Errorf("the section counts %d types, want 6 (two markers drawn under their types)", n)
	}
	if none := binTypeFamilies(nil, families, scene); len(none) != 0 {
		t.Errorf("no types: %d tab(s), want none (a family goes with its last type)", len(none))
	}
}

// TestKeyFields_TheFirstViewKeepsByTheDefaultKeysOperation: each field posts as
// its form name and opens on its own view, which keeps keys by the operations
// the scene copy gives the default key the field sends (seerrds), never a list
// of its own; only the default and the saved key are pinned to every view.
// With the default key not in the copy, the view keeps by nothing.
func TestKeyFields_TheFirstViewKeepsByTheDefaultKeysOperation(t *testing.T) {
	t.Parallel()
	scene := gatherSceneKeys(fkScenePoints(t))
	types := []*domain.BinType{{ID: 1, Code: "C4548", FamilyID: 10}}
	tabs := binTypeFamilies(types, map[int64]pageFamilyKeys{10: {lift: "jackload_9999", wait: "wait_4548"}}, scene)
	lift, wait := tabs[0].LiftField, tabs[0].WaitField
	if lift.Name != "lift_bintask" || lift.View != "Lift keys" || !slices.Equal(lift.ViewOps, []string{"JackLoad"}) {
		t.Errorf("lift field %s / %q keeps by %q, want lift_bintask / Lift keys by [JackLoad]", lift.Name, lift.View, lift.ViewOps)
	}
	if wait.Name != "wait_bintask" || wait.View != "Wait keys" || !slices.Equal(wait.ViewOps, []string{"Wait"}) {
		t.Errorf("wait field %s / %q keeps by %q, want wait_bintask / Wait keys by [Wait]", wait.Name, wait.View, wait.ViewOps)
	}
	pinned := func(f *keyField) []string {
		var out []string
		for _, o := range f.Options {
			if o.Pinned {
				out = append(out, o.Value)
			}
		}
		return out
	}
	if got := pinned(lift); !slices.Equal(got, []string{"", "jackload_9999"}) {
		t.Errorf("lift pins %q, want the default and the saved key not in RDS", got)
	}
	if got := pinned(wait); !slices.Equal(got, []string{"", "wait_4548"}) {
		t.Errorf("wait pins %q, want the default and the saved key", got)
	}
	for _, o := range wait.Options {
		if o.Value == "wait_4548" && !slices.Equal(o.Ops, []string{"Wait", "Wait2"}) {
			t.Errorf("wait_4548 carries operations %q, want every one RDS gives it", o.Ops)
		}
		if o.Value == "lift_plain" && len(o.Ops) != 0 {
			t.Errorf("lift_plain carries operations %q, want none", o.Ops)
		}
	}
	bare := gatherSceneKeys(fkScenePoints(t)[3:4]) // lift_plain and wait_4548, no JackLoad
	if got := viewOps("JackLoad", bare); got == nil || len(got) != 0 {
		t.Errorf("a copy without the default key: the view keeps by %#v, want by nothing ([], not null)", got)
	}
}

// TestFamilyKeysRefusal_ATableTurnRefusalIsShownAsSaid: a save the table-turn
// door refuses reaches the engineer as a conflict, in the door's own words.
// Decided before any read, so a zero Handlers answers it.
func TestFamilyKeysRefusal_ATableTurnRefusalIsShownAsSaid(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("%w: node TT-1 lists wait_4548, which recognizes; a set-down-side turn needs one that does not",
		dispatch.ErrTableTurnRefused)
	status, sentence := familyKeysRefusal(err)
	if status != http.StatusConflict || sentence != err.Error() {
		t.Errorf("refusal = %d %q, want %d %q", status, sentence, http.StatusConflict, err.Error())
	}
}
