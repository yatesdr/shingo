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
	"shingo/shared/parkvectors"
	"shingoedge/store/processes"
)

var updateParkVectors = flag.Bool("update-park-vectors", false,
	"rewrite shared/parkvectors/vectors.json from the step builders")

// parkVectorsPath is the shared file, from this package's directory.
var parkVectorsPath = filepath.Join("..", "..", "shared", "parkvectors", "vectors.json")

// parkRouting is one way a line's claim can route through a dedicated loader:
// where the outgoing part comes from, is staged, and goes; and where the
// incoming part of a part change comes from, is staged, and goes. The names
// are parkvectors' roles.
type parkRouting struct {
	name                    string
	source, staging, dest   string
	toSource, toStaging, to string
}

// parkRoutings are the routings whose legs reach the loader park:
//   - staged: the sim's WELD-5 rig (plants/master.yaml ALN_008), source and
//     destination the part's home, staging between;
//   - home-staging: the home is the source, the staging and the destination
//     (the line brief G's part A runs);
//   - home-return: a market feeds the line and the spent cart goes home;
//   - home-fed: the home feeds the line and the spent cart goes elsewhere.
var parkRoutings = []parkRouting{
	{name: "staged", source: "HOME", staging: "STAGE", dest: "HOME",
		toSource: "HOME-B", toStaging: "STAGE", to: "HOME-B"},
	{name: "home-staging", source: "HOME", staging: "HOME", dest: "HOME",
		toSource: "HOME-B", toStaging: "HOME-B", to: "HOME-B"},
	{name: "home-return", source: "MARKET", staging: "STAGE", dest: "HOME",
		toSource: "MARKET-B", toStaging: "STAGE", to: "HOME-B"},
	{name: "home-fed", source: "HOME", staging: "STAGE", dest: "OUT",
		toSource: "HOME-B", toStaging: "STAGE", to: "OUT"},
}

// buildParkVectors is every complex leg the Edge builds for a consume line on
// each routing: the steady state as REQUEST makes it (steadyCell, the
// preview's walk of the request-time builders, Order B and the sequential
// backfill included), the part change (BuildSwapChangeoverSteps, the planner's
// one switch), and the changeover's single-leg doors (the stage and the staged
// delivery the station's buttons make, the drop's staged release, a tool
// change's leg). Pairs name each other, as the doors that mint both uuids send
// them. Every leg names LINE as its process node, as every builder's caller
// does (claim.CoreNodeName, diff.CoreNodeName).
func buildParkVectors(t *testing.T) []parkvectors.Case {
	t.Helper()
	var out []parkvectors.Case
	add := func(r parkRouting, mode, phase, leg, payload, sibling string, steps []protocol.ComplexOrderStep) {
		if len(steps) == 0 {
			return
		}
		raw, err := json.Marshal(steps)
		if err != nil {
			t.Fatalf("%s/%s/%s/%s: %v", r.name, mode, phase, leg, err)
		}
		out = append(out, parkvectors.Case{Name: parkCaseName(r.name, mode, phase, leg), Config: r.name, Mode: mode,
			Phase: phase, Leg: leg, PayloadCode: payload, ProcessNode: "LINE", Sibling: sibling, Steps: raw})
	}
	pair := func(r parkRouting, mode, phase, a, b string, stepsA, stepsB []protocol.ComplexOrderStep) (string, string) {
		if len(stepsA) == 0 || len(stepsB) == 0 {
			return "", ""
		}
		return parkCaseName(r.name, mode, phase, b), parkCaseName(r.name, mode, phase, a)
	}
	for _, r := range parkRoutings {
		for _, mode := range []protocol.SwapMode{protocol.SwapModeSingleRobot, protocol.SwapModeTwoRobot,
			protocol.SwapModeTwoRobotPressIndex, protocol.SwapModeSequential} {
			from := &processes.NodeClaim{CoreNodeName: "LINE", SwapMode: mode, Role: protocol.ClaimRoleConsume,
				InboundSource: r.source, InboundStaging: r.staging, OutboundDestination: r.dest, PayloadCode: "PART-X"}
			if mode == protocol.SwapModeSingleRobot {
				from.OutboundStaging = "OSTAGE"
			}
			if mode == protocol.SwapModeTwoRobotPressIndex {
				from.PairedCoreNode = "PAIR"
			}
			to := *from
			to.InboundSource, to.InboundStaging, to.OutboundDestination, to.PayloadCode = r.toSource, r.toStaging, r.to, "PART-Y"
			m := string(mode)

			cell := steadyCell(from)
			var stepsA, stepsB []protocol.ComplexOrderStep
			for _, l := range cell.Legs {
				switch {
				case l.Kind != SteadyKindComplex:
				case l.Leg == SteadyLegA:
					stepsA = l.Steps
				case l.Leg == SteadyLegB:
					stepsB = l.Steps
				default:
					add(r, m, "steady", "backfill", "PART-X", "", l.Steps)
				}
			}
			sibA, sibB := pair(r, m, "steady", "A", "B", stepsA, stepsB)
			add(r, m, "steady", "A", "PART-X", sibA, stepsA)
			add(r, m, "steady", "B", "PART-X", sibB, stepsB)

			c := BuildSwapChangeoverSteps(from, &to, "", "LINE")
			if c.Roles != nil {
				supplyPayload := "PART-Y"
				if c.Roles.supply.carriesFromPayload {
					supplyPayload = "PART-X"
				}
				sibS, sibE := pair(r, m, "changeover", "supply", "evac", c.Roles.supply.steps, c.Roles.evac.steps)
				add(r, m, "changeover", "supply", supplyPayload, sibS, c.Roles.supply.steps)
				add(r, m, "changeover", "evac", "PART-X", sibE, c.Roles.evac.steps)
			} else {
				payloadA := "PART-Y"
				if c.CarriesFromPayloadA {
					payloadA = "PART-X"
				}
				sibA, sibB := pair(r, m, "changeover", "A", "B", c.StepsA, c.StepsB)
				add(r, m, "changeover", "A", payloadA, sibA, c.StepsA)
				add(r, m, "changeover", "B", "PART-X", sibB, c.StepsB)
			}
		}
		// The changeover's single-leg doors read no mode.
		from := &processes.NodeClaim{CoreNodeName: "LINE", Role: protocol.ClaimRoleConsume, InboundSource: r.source,
			InboundStaging: r.staging, OutboundDestination: r.dest, PayloadCode: "PART-X"}
		to := *from
		to.InboundSource, to.InboundStaging, to.OutboundDestination, to.PayloadCode = r.toSource, r.toStaging, r.to, "PART-Y"
		add(r, "any", "changeover", "stage", "PART-Y", "", BuildStageSteps(&to))
		add(r, "any", "changeover", "staged-deliver", "PART-Y", "", BuildStagedDeliverSteps(&to))
		add(r, "any", "changeover", "drop-release", "PART-X", "", BuildStagedReleaseSteps(from))
		add(r, "any", "changeover", "tooling", "PART-X", "",
			buildToolingEvacSteps("LINE", from.OutboundDestination, from, &to, from.InboundStaging))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func parkCaseName(routing, mode, phase, leg string) string {
	return routing + "/" + mode + "/" + phase + "/" + leg
}

// TestParkVectors_AreTheBuildersShapes is the Edge half of the park's pins:
// the shared file is exactly what the builders write today. Core's docker test
// runs every case through its loader park (TestLoaderPark_EveryBuilderLeg).
func TestParkVectors_AreTheBuildersShapes(t *testing.T) {
	t.Parallel()
	got, err := json.MarshalIndent(buildParkVectors(t), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	if *updateParkVectors {
		if err := os.WriteFile(parkVectorsPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	if !bytes.Equal(got, parkvectors.Raw()) {
		t.Fatalf("shared/parkvectors/vectors.json is stale against the step builders. Regenerate it:\n" +
			"  cd shingo-edge && go test ./engine/ -run TestParkVectors_AreTheBuildersShapes -update-park-vectors\n" +
			"and run Core's TestLoaderPark_EveryBuilderLeg (docker) on the new file: a new or changed leg needs " +
			"its placement pinned there.")
	}
}
