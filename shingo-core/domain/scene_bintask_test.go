package domain

import (
	"reflect"
	"testing"
)

// The property shapes the plants' scene copies hold (2026-11-17), with the
// rest of a location's properties beside them.
func TestScenePoint_BinTasks(t *testing.T) {
	t.Parallel()
	keyed := `[{"key":"points","type":"json","stringValue":"[{\"x\":1,\"y\":2}]"},` +
		`{"key":"binTask","type":"json","stringValue":"[\n {\"JackLoad\":{\"operation\":\"JackLoad\",\"recognize\":true}},` +
		`\n {\"jackload_4548\":{\"operation\":\"JackLoad\",\"recfile\":\"4548.shelf\"}},` +
		`\n {\"Wait\":{\"operation\":\"Wait\"}},\n {\"noop\":{}},\n {\"odd\":7}\n]"}]`
	cases := []struct {
		name  string
		props string
		want  []SceneBinTask
	}{
		{"keys in the scene's order, each with its operation", keyed, []SceneBinTask{
			{"JackLoad", "JackLoad"}, {"jackload_4548", "JackLoad"}, {"Wait", "Wait"}, {"noop", ""}, {"odd", ""}}},
		{"a binTask property with no value (Hopkinsville)", `[{"key":"binTask","type":"json"}]`, nil},
		{"no binTask property", `[{"key":"points","type":"json","stringValue":"[]"}]`, nil},
		{"no properties", `[]`, nil},
		{"properties that do not read", `{"not":"an array"}`, nil},
		{"a binTask value that does not read", `[{"key":"binTask","type":"json","stringValue":"JackLoad"}]`, nil},
	}
	for _, c := range cases {
		sp := &ScenePoint{PropertiesJSON: c.props}
		if got := sp.BinTasks(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: BinTasks = %+v, want %+v", c.name, got, c.want)
		}
	}
	sp := &ScenePoint{PropertiesJSON: keyed}
	if !sp.ListsBinTask("jackload_4548") || sp.ListsBinTask("jackload_3230") || sp.ListsBinTask("") {
		t.Error("ListsBinTask: want jackload_4548 listed, jackload_3230 and the blank name not")
	}
}
