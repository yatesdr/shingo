package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// THE WIRE EVERY FULL-SHAPE BUILDER EMITS, frozen.
//
// Keep-staged recomposes the full builders as BuildStageSteps ++ tail. That
// refactor must not move a byte of what an ordinary claim sends Core, so the
// output of every builder that reaches the wire is captured here for every role
// and every configurable mode, with no keep-staged node, and compared exactly.
//
// Regenerate only on a deliberate wire change:
//
//	go test ./engine -run TestSwapWireGolden -update-swap-wire-golden
var updateSwapWireGolden = flag.Bool("update-swap-wire-golden", false, "rewrite testdata/swap_wire_golden.json")

const swapWireGoldenPath = "testdata/swap_wire_golden.json"

func goldenClaim(role protocol.ClaimRole, mode protocol.SwapMode, payload, secondPaired string, flip bool) *processes.NodeClaim {
	return &processes.NodeClaim{
		ID: 1, StyleID: 1, CoreNodeName: "LINE", Role: role, SwapMode: mode, PayloadCode: payload,
		UOPCapacity: 40, ReorderPoint: 10,
		InboundStaging: "STG-IN", OutboundStaging: "STG-OUT",
		InboundSource: "SRC", OutboundDestination: "DST",
		PairedCoreNode: "PAIR-1", SecondPairedCoreNode: secondPaired,
		IndexRobotSupplies: flip,
	}
}

type goldenVariant struct {
	name         string
	secondPaired string
	flip         bool
}

func goldenVariants(mode protocol.SwapMode) []goldenVariant {
	if mode != protocol.SwapModeTwoRobotPressIndex {
		return []goldenVariant{{name: "base"}}
	}
	return []goldenVariant{
		{name: "2pos"}, {name: "3pos", secondPaired: "PAIR-2"},
		{name: "2pos-flip", flip: true}, {name: "3pos-flip", secondPaired: "PAIR-2", flip: true},
	}
}

func renderChangeoverDispatch(d ChangeoverDispatch) map[string]any {
	out := map[string]any{
		"StepsA": d.StepsA, "DeliveryNodeA": d.DeliveryNodeA, "AutoConfirmA": d.AutoConfirmA,
		"CurtainWaitsA": d.CurtainWaitsA, "CarriesFromPayloadA": d.CarriesFromPayloadA,
		"StepsB": d.StepsB, "AutoConfirmB": d.AutoConfirmB,
	}
	if d.Roles != nil {
		leg := func(l changeoverLeg) map[string]any {
			return map[string]any{"steps": l.steps, "deliveryNode": l.deliveryNode,
				"autoConfirm": l.autoConfirm, "carriesFromPayload": l.carriesFromPayload}
		}
		out["Roles"] = map[string]any{"evac": leg(d.Roles.evac), "supply": leg(d.Roles.supply)}
	}
	return out
}

func renderPlanActions(diffs []ChangeoverNodeDiff) []map[string]any {
	nodes := []processes.Node{{ID: 1, Name: "LINE", CoreNodeName: "LINE", ProcessID: 1}}
	plan := BuildChangeoverPlan(diffs, nodes, false, nil, toolingChangeover{})
	var out []map[string]any
	for _, a := range plan.Actions {
		errText := ""
		if a.Err != nil {
			errText = a.Err.Error()
		}
		out = append(out, map[string]any{
			"Situation": a.Situation, "SupplyOrder": a.SupplyOrder, "EvacOrder": a.EvacOrder,
			"NextState": a.NextState, "LogTag": a.LogTag, "Err": errText,
		})
	}
	return out
}

func buildSwapWireGolden(t *testing.T) map[string]any {
	t.Helper()
	golden := map[string]any{}
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE", ProcessID: 1}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range protocol.ConfigurableSwapModes() {
			for _, v := range goldenVariants(mode) {
				key := fmt.Sprintf("%s/%s/%s", role, mode, v.name)

				claim := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
				disp, err := BuildSwapDispatch(node, claim)
				entry := map[string]any{"BuildSwapDispatch": disp}
				if err != nil {
					entry["BuildSwapDispatchErr"] = err.Error()
				}

				for _, pay := range []struct{ name, to string }{{"samePayload", "P-FROM"}, {"newPayload", "P-TO"}} {
					from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
					to := goldenClaim(role, mode, pay.to, v.secondPaired, v.flip)
					entry["BuildSwapChangeoverSteps/"+pay.name] = renderChangeoverDispatch(
						BuildSwapChangeoverSteps(from, to, "", ""))
					entry["BuildEvacuateChangeoverSteps/"+pay.name] = renderChangeoverDispatch(
						BuildEvacuateChangeoverSteps(from, to, "", ""))
				}

				from := goldenClaim(role, mode, "P-FROM", v.secondPaired, v.flip)
				to := goldenClaim(role, mode, "P-TO", v.secondPaired, v.flip)
				entry["BuildChangeoverPlan"] = map[string]any{
					"swap":     renderPlanActions([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationSwap, FromClaim: from, ToClaim: to}}),
					"evacuate": renderPlanActions([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationEvacuate, FromClaim: from, ToClaim: to}}),
					"add":      renderPlanActions([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationAdd, ToClaim: to}}),
					"drop":     renderPlanActions([]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: SituationDrop, FromClaim: from}}),
				}
				golden[key] = entry
			}
		}
	}
	return golden
}

func TestSwapWireGolden(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(buildSwapWireGolden(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	got = append(got, '\n')
	if *updateSwapWireGolden {
		if err := os.MkdirAll(filepath.Dir(swapWireGoldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(swapWireGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(swapWireGoldenPath)
	if err != nil {
		t.Fatalf("read %s (run with -update-swap-wire-golden to create it): %v", swapWireGoldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("the full-shape wire moved. Every ordinary claim's swap and changeover output must stay "+
			"byte-identical; diff %s against the regenerated output to see what changed", swapWireGoldenPath)
	}
}
