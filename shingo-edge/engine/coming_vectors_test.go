package engine

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"shingo/protocol"
	"shingo/shared/comingvectors"
	"shingoedge/store/processes"
)

var updateComingVectors = flag.Bool("update-coming-vectors", false,
	"rewrite shared/comingvectors/vectors.json from the step builders")

// comingVectorsPath is the shared file, from this package's directory.
var comingVectorsPath = filepath.Join("..", "..", "shared", "comingvectors", "vectors.json")

// buildComingVectors is every leg shape the builders write, steady state and
// changeover, for every swap mode, both roles, flipped and unflipped, two and
// three positions where the mode has them, plus the plain orders; each with
// protocol.OrderPlacesBinAt's answer at every node it names.
func buildComingVectors(t *testing.T) []comingvectors.Case {
	t.Helper()
	var out []comingvectors.Case
	add := func(name string, orderType protocol.OrderType, steps []protocol.ComplexOrderStep, delivery string) {
		if orderType == protocol.OrderTypeComplex && len(steps) == 0 {
			return
		}
		places := map[string]bool{}
		for _, s := range steps {
			if s.Node != "" {
				places[s.Node] = protocol.OrderPlacesBinAt(orderType, steps, delivery, s.Node)
			}
		}
		if delivery != "" {
			places[delivery] = protocol.OrderPlacesBinAt(orderType, steps, delivery, delivery)
		}
		var raw json.RawMessage
		if len(steps) > 0 {
			var err error
			if raw, err = json.Marshal(steps); err != nil {
				t.Fatalf("%s: %v", name, err)
			}
		}
		out = append(out, comingvectors.Case{Name: name, OrderType: string(orderType), Steps: raw,
			DeliveryNode: delivery, Places: places})
	}
	node := &processes.Node{ID: 1, Name: "LINE", CoreNodeName: "LINE"}
	for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot,
		protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential} {
		for _, role := range []protocol.ClaimRole{protocol.ClaimRoleProduce, protocol.ClaimRoleConsume} {
			for _, flipped := range []bool{false, true} {
				for _, second := range []string{"", "C"} {
					press := mode == protocol.SwapModeTwoRobotPressIndex
					if !press && (flipped || second != "") {
						continue
					}
					claim := &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: mode, Role: role,
						IndexRobotSupplies: flipped, InboundSource: "IN", InboundStaging: "IN-STG",
						OutboundStaging: "OUT-STG", OutboundDestination: "OUT", PayloadCode: "PART"}
					if mode != protocol.SwapModeSingleRobot {
						claim.PairedCoreNode, claim.SecondPairedCoreNode = "B", second
					}
					to := *claim
					to.PayloadCode = "NEXT"
					name := string(mode) + "/" + string(role)
					if flipped {
						name += "/flipped"
					}
					if second != "" {
						name += "/3-position"
					}
					d, err := buildSwapDispatch(node, claim)
					if err != nil {
						t.Fatalf("%s: %v", name, err)
					}
					if d != nil {
						add(name+"/steady/A", protocol.OrderTypeComplex, d.StepsA, "")
						add(name+"/steady/B", protocol.OrderTypeComplex, d.StepsB, "")
					}
					c := BuildSwapChangeoverSteps(claim, &to, "B", "LINE")
					if c.Roles != nil {
						add(name+"/changeover/evac", protocol.OrderTypeComplex, c.Roles.evac.steps, "")
						add(name+"/changeover/supply", protocol.OrderTypeComplex, c.Roles.supply.steps, "")
					} else {
						add(name+"/changeover/A", protocol.OrderTypeComplex, c.StepsA, "")
						add(name+"/changeover/B", protocol.OrderTypeComplex, c.StepsB, "")
					}
				}
			}
		}
	}
	add("plain/retrieve to the on-deck", protocol.OrderTypeRetrieve, nil, "B")
	add("plain/retrieve_empty to the on-deck", protocol.OrderTypeRetrieveEmpty, nil, "B")
	add("plain/move off the on-deck to the outbound", protocol.OrderTypeMove, nil, "OUT")
	add("plain/store to the outbound", protocol.OrderTypeStore, nil, "OUT")
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// TestComingVectors_AreTheBuildersShapes is the Edge half of the "coming"
// agreement: the shared file is exactly what the builders write today. Core's
// docker test reads the same file over stored rows.
func TestComingVectors_AreTheBuildersShapes(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(buildComingVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateComingVectors {
		if err := os.WriteFile(comingVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, comingvectors.Raw()) {
		t.Fatalf("shared/comingvectors/vectors.json is stale against the step builders. Regenerate it:\n" +
			"  cd shingo-edge && go test ./engine/ -run TestComingVectors_AreTheBuildersShapes -update-coming-vectors\n" +
			"and run Core's TestComingVectors_CoreAgreesOverStoredRows (docker) on the new file.")
	}
}
