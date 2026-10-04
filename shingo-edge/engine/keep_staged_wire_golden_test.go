package engine

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"testing"

	"shingo/protocol"
	"shingoedge/store/processes"
)

// THE WIRE A KEEP-STAGED SWAP EMITS, frozen.
//
// A keep-staged claim's swap and changeover fetch the spare standing on its
// spot. Every byte of what they send Core is captured here, for both roles of
// single_robot and two_robot, steady state and changeover, the incoming part
// the same and changed, so a rework of how those step lists are built is held
// to the same wire.
//
// Regenerate only on a deliberate wire change:
//
//	go test ./engine -run TestKeepStagedWireGolden -update-keep-staged-wire-golden
var updateKeepStagedWireGolden = flag.Bool("update-keep-staged-wire-golden", false,
	"rewrite testdata/keep_staged_wire_golden.json")

const keepStagedWireGoldenPath = "testdata/keep_staged_wire_golden.json"

func buildKeepStagedWireGolden(t *testing.T) map[string]any {
	t.Helper()
	golden := map[string]any{}
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE", ProcessID: 1}
	for _, role := range []protocol.ClaimRole{protocol.ClaimRoleConsume, protocol.ClaimRoleProduce} {
		for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot} {
			claim := keepStagedClaim(role, mode, "P-FROM")
			disp, err := BuildSwapDispatch(node, claim)
			entry := map[string]any{"BuildSwapDispatch": disp}
			if err != nil {
				entry["BuildSwapDispatchErr"] = err.Error()
			}
			for _, pay := range []struct{ name, to string }{{"samePayload", "P-FROM"}, {"newPayload", "P-TO"}} {
				for _, situation := range []ChangeoverSituation{SituationSwap, SituationEvacuate} {
					// The outgoing claim keeps no spare; the incoming one does.
					from := goldenClaim(role, mode, "P-FROM", "", false)
					from.PairedCoreNode = ""
					to := keepStagedClaim(role, mode, pay.to)
					entry[fmt.Sprintf("BuildChangeoverPlan/%s/%s", situation, pay.name)] = renderPlanActions(
						[]ChangeoverNodeDiff{{CoreNodeName: "LINE", Situation: situation, FromClaim: from, ToClaim: to}})
				}
			}
			golden[fmt.Sprintf("%s/%s", role, mode)] = entry
		}
	}
	return golden
}

func TestKeepStagedWireGolden(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(buildKeepStagedWireGolden(t), "", "  ")
	if err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	got = append(got, '\n')
	if *updateKeepStagedWireGolden {
		if err := os.WriteFile(keepStagedWireGoldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(keepStagedWireGoldenPath)
	if err != nil {
		t.Fatalf("read %s (run with -update-keep-staged-wire-golden to create it): %v", keepStagedWireGoldenPath, err)
	}
	if string(got) != string(want) {
		t.Fatalf("the keep-staged wire moved. A keep-staged swap and changeover must stay byte-identical; "+
			"diff %s against the regenerated output to see what changed", keepStagedWireGoldenPath)
	}
}
