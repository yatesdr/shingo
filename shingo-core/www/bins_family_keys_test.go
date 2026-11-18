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

// optionLines is a picker as "value|label", with a trailing "*" on the
// selected option.
func optionLines(opts []keyOption) []string {
	out := make([]string, len(opts))
	for i, o := range opts {
		out[i] = o.Value + "|" + o.Label
		if o.Selected {
			out[i] += "*"
		}
	}
	return out
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
			labels[i] = k.label()
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

// TestBinTypeRows_EachTypeShowsItsFamilysKeys: every type of a family shows
// the family's keys and names the family's other types (a bare marker among
// them); the defaults are labelled with what Core sends (seerrds); a type
// whose family the page did not read offers nothing to save.
func TestBinTypeRows_EachTypeShowsItsFamilysKeys(t *testing.T) {
	t.Parallel()
	scene := gatherSceneKeys(fkScenePoints(t)[:2])
	types := []*domain.BinType{
		{ID: 1, Code: "C4548-A", FamilyID: 10, FamilyCode: "C4548"},
		{ID: 2, Code: "C4548-A-BARE", FamilyID: 10, FamilyCode: "C4548", Bare: true},
		{ID: 3, Code: "C4548-B", FamilyID: 10, FamilyCode: "C4548"},
		{ID: 4, Code: "C3230", FamilyID: 20, FamilyCode: "C3230"},
		{ID: 5, Code: "UNREAD", FamilyID: 30, FamilyCode: "UNREAD"},
	}
	families := map[int64]pageFamilyKeys{10: {lift: "jackload_4548", wait: "wait_4548"}, 20: {}}
	rows := binTypeRows(types, families, scene)
	if len(rows) != len(types) {
		t.Fatalf("%d rows for %d types", len(rows), len(types))
	}
	sel := func(opts []keyOption) string {
		for _, o := range opts {
			if o.Selected {
				return o.Label
			}
		}
		return "(none)"
	}
	want := []struct{ lift, wait, others string }{
		{"jackload_4548 · JackLoad", "wait_4548 · Wait", "[C4548-A-BARE C4548-B]"},
		{"jackload_4548 · JackLoad", "wait_4548 · Wait", "[C4548-A C4548-B]"},
		{"jackload_4548 · JackLoad", "wait_4548 · Wait", "[C4548-A C4548-A-BARE]"},
		{"JackLoad (default)", "Wait (default)", "[]"},
	}
	for i, w := range want {
		r := rows[i]
		if got := [3]string{sel(r.Lift), sel(r.Wait), fmt.Sprint(r.FamilyOthers)}; got != [3]string{w.lift, w.wait, w.others} {
			t.Errorf("%s: lift %q, wait %q, others %s; want %q, %q, %s", r.Code, got[0], got[1], got[2], w.lift, w.wait, w.others)
		}
	}
	if r := rows[4]; r.Lift != nil || r.Wait != nil {
		t.Errorf("%s, whose family was not read, offers keys %q / %q", r.Code, optionLines(r.Lift), optionLines(r.Wait))
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
