package domain

import (
	"encoding/json"
	"slices"
)

// SceneBinTask is one binTask key a bin location lists in Core's copy of the
// RDS scene, with the operation RDS gives that key ("" when it gives none).
// The name is the key a block may send; the operation is RDS's own word for
// what the key does, shown beside it and never read for meaning.
type SceneBinTask struct {
	Name      string
	Operation string
}

// BinTasks reads the binTask keys the point lists, in the scene's order: the
// one reading of the property, shared by dispatch's carrier keys, the Bins
// page's key pickers and the Nodes page.
//
// PropertiesJSON is the sync's array of {key, type, stringValue}. The binTask
// property's stringValue is JSON again: an array of objects, each one key, the
// task's name, over its parameters (Springfield's copy, 2026-11-17:
// [{"JackLoad":{"operation":"JackLoad","recognize":true,…}},{"JackUnload":…},…]).
// A point with no binTask property, one whose property has no value (ten
// Hopkinsville locations, 2026-11-17), and one that does not read list none.
// Pinned by TestScenePoint_BinTasks.
func (sp *ScenePoint) BinTasks() []SceneBinTask {
	var props []struct {
		Key         string `json:"key"`
		StringValue string `json:"stringValue"`
	}
	if err := json.Unmarshal([]byte(sp.PropertiesJSON), &props); err != nil {
		return nil
	}
	for _, p := range props {
		if p.Key != "binTask" {
			continue
		}
		var tasks []map[string]json.RawMessage
		if err := json.Unmarshal([]byte(p.StringValue), &tasks); err != nil {
			return nil
		}
		var out []SceneBinTask
		for _, task := range tasks {
			names := make([]string, 0, len(task))
			for name := range task {
				names = append(names, name)
			}
			slices.Sort(names) // one key per object in every scene read; sorted, should one carry two
			for _, name := range names {
				var params struct {
					Operation string `json:"operation"`
				}
				_ = json.Unmarshal(task[name], &params) // parameters that do not read give no operation
				out = append(out, SceneBinTask{Name: name, Operation: params.Operation})
			}
		}
		return out
	}
	return nil
}

// ListsBinTask reports whether the point lists the binTask key name.
func (sp *ScenePoint) ListsBinTask(name string) bool {
	return slices.ContainsFunc(sp.BinTasks(), func(t SceneBinTask) bool { return t.Name == name })
}
