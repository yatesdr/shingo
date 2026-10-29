package domain

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// recipe_groups_test.go — a recipe's groups are checked at save, by name
// (recipe groups, 2026-10-24; SYNTH-round2 rows 1-6 and §2).
//
// THE TWO COPIES CANNOT DRIFT. The cells stay the recipe (the ★ defaults, the
// mint's layout, the pre-fill's lay-on, drift), and the groups sit beside them,
// one per slot: so the save refuses every way the two could say different
// things, and every node a group names is checked like a cell's.
//
// Press 6's shape by node names only: Left Hand of PLN_006 (deck PLN_005),
// Right Hand of PLN_007 (deck PLN_008). A two_robot cell on PLN_009 for the
// field a mode forbids, and a sequential pair for the floor's pair.

var groupsLive = func() map[string]bool {
	out := map[string]bool{}
	for _, n := range []string{"PLN_005", "PLN_006", "PLN_007", "PLN_008", "PLN_009", "PLN_010", "PLN_011",
		"PLN_012", "PLN_013", "PLN_014", "ULN_002", "ULN_005", "SMN_BUF_100", "STG_TOOL_1", "STG_TOOL_2",
		"SEQ_A", "SEQ_B", "SEQ_C", "SEQ_D"} {
		out[n] = true
	}
	return out
}()

const twoSpotCells = `"cells":[
 {"core_node_name":"PLN_006","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_005","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_002"},
 {"core_node_name":"PLN_007","role":"produce","swap_mode":"two_robot_press_index","paired_core_node":"PLN_008","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_005"}]`

const leftHand = `{"of":"PLN_006","name":"Left Hand","spots":[
  {"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
  {"core_node_name":"PLN_010","paired_core_node":["PLN_005"]}]}`

const rightHand = `{"of":"PLN_007","name":"Right Hand","spots":[
  {"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},
  {"core_node_name":"PLN_012","paired_core_node":["PLN_008"]}],
  "advanced":{"reorder_point":1,"auto_reorder":true}}`

func twoSpotWith(groups ...string) string {
	return "{" + twoSpotCells + `,"groups":[` + strings.Join(groups, ",") + "]}"
}

func TestValidateFlowPreset_AGroupedRecipeIsCheckedByName(t *testing.T) {
	t.Parallel()
	for name, flow := range map[string]string{
		"Press 6 as the engineer saves it": twoSpotWith(leftHand, rightHand),
		"no groups at all":                 "{" + twoSpotCells + "}",
		"an empty groups list":             twoSpotWith(),
		"a strict group with a tolerance": twoSpotWith(leftHand,
			strings.Replace(rightHand, `"reorder_point":1`, `"strict_pack":true,"pack_tolerance_pct":10`, 1)),
		// A tied list: PLN_013 is fed only by PLN_014, and nothing else is.
		"a spot with a deck of its own": twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_013","paired_core_node":["PLN_014"]}]}`, rightHand),
		// The floor picks a pair, never half of one.
		"a sequential pair": `{"cells":[
			{"core_node_name":"SEQ_A","role":"consume","swap_mode":"sequential","paired_core_node":"SEQ_B","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_002"},
			{"core_node_name":"SEQ_B","role":"consume","swap_mode":"sequential","paired_core_node":"SEQ_A","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_002"}],
			"groups":[{"of":"SEQ_A","name":"Line","spots":[
			{"core_node_name":"SEQ_A","paired_core_node":["SEQ_B"]},
			{"core_node_name":"SEQ_C","paired_core_node":["SEQ_D"]}]}]}`,
	} {
		if err := ValidateFlowPreset(flow, groupsLive); err != nil {
			t.Errorf("%s: refused: %v", name, err)
		}
	}

	for name, tc := range map[string]struct {
		flow, says string
		is         error
	}{
		"one group for two parts": {flow: twoSpotWith(leftHand),
			says: "this recipe takes 2 parts and has 1 group — set up one group per part"},
		"a group of no part": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"of":"PLN_007"`, `"of":"PLN_009"`, 1)),
			says: "Right Hand is of PLN_009, which is not where a part of this recipe runs — a group is of a part's own spot"},
		"two groups of one part": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"of":"PLN_007"`, `"of":"PLN_006"`, 1)),
			says: "Left Hand and Right Hand are both of PLN_006 — set up one group per part"},
		"the first spot is not the recipe's": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_010","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]}]}`, rightHand),
			says: "Left Hand's first spot is PLN_010; it must be PLN_006, the spot the recipe runs it on"},
		"the recipe's own deck left out": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_011"]}]}`, rightHand),
			says: "PLN_005 is an on-deck spot of PLN_006 in the recipe and missing from Left Hand's on-deck spots for PLN_006"},
		// The owner's rule as a structural fact: an on-deck spot never feeds
		// the other side.
		"a deck in both groups": {flow: twoSpotWith(leftHand, `{"of":"PLN_007","name":"Right Hand","spots":[
			{"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},
			{"core_node_name":"PLN_012","paired_core_node":["PLN_005"]}]}`),
			says: "PLN_005 is in both Left Hand and Right Hand — a node serves one group only"},
		"a spot in both groups": {flow: twoSpotWith(leftHand, `{"of":"PLN_007","name":"Right Hand","spots":[
			{"core_node_name":"PLN_007","paired_core_node":["PLN_008"]},
			{"core_node_name":"PLN_010","paired_core_node":["PLN_008"]}]}`),
			says: "PLN_010 is in both Left Hand and Right Hand — a node serves one group only"},
		"a spot that is also a deck": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_010","paired_core_node":["PLN_005","PLN_006"]}]}`, rightHand),
			says: "PLN_006 is a spot of Left Hand and one of its on-deck spots too — a node has one job in a group"},
		"a list the mode forbids": {flow: `{"cells":[
			{"core_node_name":"PLN_009","role":"produce","swap_mode":"two_robot","inbound_staging":"STG_TOOL_1","inbound_source":"SMN_BUF_100","outbound_destination":"ULN_002"}],
			"groups":[{"of":"PLN_009","name":"Solo","spots":[
			{"core_node_name":"PLN_009","inbound_staging":["STG_TOOL_1"],"paired_core_node":["PLN_005"]}]}]}`,
			says: "Solo lists PLN_005 as paired core node for PLN_009, which a 2‑Robot Swap does not use"},
		"a list for a field the recipe's own row leaves blank": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_010","paired_core_node":["PLN_005"],"inbound_staging":["STG_TOOL_1"]}]}`, rightHand),
			says: "Left Hand lists a staging lane for PLN_010, and PLN_006 has none in the recipe — set one on PLN_006's row first, or take it out"},
		"a part in Advanced": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"reorder_point":1`, `"allowed_payload_codes":["SYN-P6-A"]`, 1)),
			says: "Right Hand's Advanced sets allowed parts", is: ErrFlowPresetHasPayload},
		"an auto-request part in Advanced": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"reorder_point":1`, `"auto_request_payload":"SYN-P6-A"`, 1)),
			says: "Right Hand's Advanced sets auto request part", is: ErrFlowPresetHasPayload},
		// The mint would refuse every style made from it (the claim validator
		// refuses what the mode forbids), so the save says so first.
		"an Advanced field the mode does not use": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"reorder_point":1`, `"auto_push":true`, 1)),
			says: "Right Hand's Advanced sets auto push, which a 2‑Robot Index produce spot does not use — clear it"},
		// The same reason: the claim validator would refuse every minted claim.
		"a pack tolerance out of range": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"reorder_point":1`, `"strict_pack":true,"pack_tolerance_pct":25`, 1)),
			says: "Right Hand's Advanced: Pack tolerance is a percent of the pack from 0 to 20; 25 is out of range"},
		"a spot the process does not have": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_099","paired_core_node":["PLN_005"]}]}`, rightHand),
			says: `Left Hand: core_node_name "PLN_099"`, is: ErrFlowPresetUnknownNode},
		"a deck the process does not have": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_010","paired_core_node":["PLN_098"]}]}`, rightHand),
			says: `Left Hand: paired_core_node "PLN_098"`, is: ErrFlowPresetUnknownNode},
		"a keep-staged node the process does not have": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"reorder_point":1`, `"keep_staged_node":"SMN_GONE"`, 1)),
			says: `Right Hand: keep_staged_node "SMN_GONE"`, is: ErrFlowPresetUnknownNode},
		// D8 of DECISIONS-recipe-groups: a spot the floor is offered and can
		// never take.
		"a spot with no deck": {flow: twoSpotWith(`{"of":"PLN_006","name":"Left Hand","spots":[
			{"core_node_name":"PLN_006","paired_core_node":["PLN_005"]},
			{"core_node_name":"PLN_010"}]}`, rightHand),
			says: "PLN_010 has no on-deck spot in Left Hand — add one, or take PLN_010 out"},
		"a group with no name": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"name":"Right Hand"`, `"name":" "`, 1)),
			says: "the group of PLN_007 has no name — the floor asks for its part by it"},
		"two groups of one name": {flow: twoSpotWith(leftHand, strings.Replace(rightHand, `"name":"Right Hand"`, `"name":"Left Hand"`, 1)),
			says: "two groups are called Left Hand — the floor asks for each part by its group's name"},
	} {
		err := ValidateFlowPreset(tc.flow, groupsLive)
		if err == nil || !strings.Contains(err.Error(), tc.says) || (tc.is != nil && !errors.Is(err, tc.is)) {
			t.Errorf("%s: %v, want a refusal saying %q", name, err, tc.says)
		}
	}
}

// RecipeAdvanced is FlowAdvanced less what a recipe may not say, key for key,
// and LayOver carries every field it has: a field added to one and not the
// other, or added and not laid on, is a group setting the mint drops.
func TestRecipeAdvanced_IsFlowAdvancedLessWhatARecipeMayNotSay(t *testing.T) {
	t.Parallel()
	keys := func(v any) map[string]bool {
		out := map[string]bool{}
		rt := reflect.TypeOf(v)
		for i := 0; i < rt.NumField(); i++ {
			out[strings.Split(rt.Field(i).Tag.Get("json"), ",")[0]] = true
		}
		return out
	}
	want := keys(FlowAdvanced{})
	for _, k := range []string{"allowed_payload_codes", "auto_request_payload", "index_robot_supplies", "sequence", "reorder_point_source"} {
		delete(want, k)
	}
	if got := keys(RecipeAdvanced{}); !reflect.DeepEqual(got, want) {
		t.Errorf("RecipeAdvanced keys %v, want FlowAdvanced's less the five %v", got, want)
	}

	var g RecipeAdvanced
	rv := reflect.ValueOf(&g).Elem()
	for i := 0; i < rv.NumField(); i++ {
		switch f := rv.Field(i); f.Kind() {
		case reflect.Bool:
			f.SetBool(true)
		case reflect.Int:
			f.SetInt(3)
		case reflect.String:
			f.SetString("keep_lineside")
		}
	}
	var a FlowAdvanced
	g.LayOver(&a)
	read := func(v any) map[string]any {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]any{}
		if err := json.Unmarshal(raw, &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	laid := read(a)
	for k, v := range read(g) {
		if !reflect.DeepEqual(laid[k], v) {
			t.Errorf("LayOver left %s at %v, want the group's %v", k, laid[k], v)
		}
	}
	if a.ReorderPointSource != ReorderPointSourceManual {
		t.Errorf("a group's reorder point laid on reads source %q, want %q", a.ReorderPointSource, ReorderPointSourceManual)
	}
}
