package testdb

import (
	"encoding/json"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/scene"
)

// SceneBinTask is one binTask key a seeded location lists, and the operation
// RDS gives it ("" for none).
type SceneBinTask struct {
	Name      string
	Operation string
}

// SeedSceneBinTasks writes a bin location (a GeneralLocation scene row in area
// "Area-01") whose binTask property lists keys, in order, shaped as the scene
// sync writes a plant's (domain.ScenePoint.BinTasks reads it). With no keys the
// property is present with no value, as ten Hopkinsville locations have it
// (2026-11-17). Without a helper like this no test can see a sized key: the
// simulator has no scene, and SeedScenePoint writes no properties.
func SeedSceneBinTasks(t *testing.T, db *store.DB, location string, keys ...SceneBinTask) {
	t.Helper()
	prop := map[string]any{"key": "binTask", "type": "json"}
	if len(keys) > 0 {
		tasks := make([]map[string]map[string]any, len(keys))
		for i, k := range keys {
			params := map[string]any{"recognize": true}
			if k.Operation != "" {
				params["operation"] = k.Operation
			}
			tasks[i] = map[string]map[string]any{k.Name: params}
		}
		value, err := json.Marshal(tasks)
		testutil.MustNoErr(t, err, "marshal binTask of "+location)
		prop["stringValue"] = string(value)
	}
	props, err := json.Marshal([]any{map[string]any{"key": "points", "type": "json", "stringValue": "[]"}, prop})
	testutil.MustNoErr(t, err, "marshal properties of "+location)
	testutil.MustNoErr(t, db.UpsertScenePoint(&scene.Point{
		AreaName: "Area-01", InstanceName: location, ClassName: "GeneralLocation", PointName: "AP-" + location,
		PropertiesJSON: string(props),
	}), "seed scene location "+location)
}

// DefaultSceneBinTasks is the three keys every keyed location at both plants
// lists today, each with its operation.
func DefaultSceneBinTasks() []SceneBinTask {
	return []SceneBinTask{{"JackLoad", "JackLoad"}, {"JackUnload", "JackUnload"}, {"Wait", "Wait"}}
}
