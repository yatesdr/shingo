package engine

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// claim_leg_writers_test.go — the engine's two claim writers, handed a lane.
// The rest of the writers, and what "saved" and "refused" mean, are in
// www/claim_leg_writers_test.go.

const (
	engLegSaved   = "saved"
	engLegRefused = "refused"
)

// engLaneCore is a Core list with a lane in a group, and every other node the
// reorder claim names, so a refusal there is for the lane and not for a name
// Core's list does not hold.
var engLaneCore = []protocol.NodeInfo{
	{Name: "CW-SUP", NodeType: protocol.NodeClassNGRP},
	{Name: "CW-SUP.CW-LANE", NodeType: protocol.NodeClassLANE},
	{Name: "CW-PRESS"}, {Name: "CW-HOLD"}, {Name: "CW-STG"}, {Name: "CW-STG2"},
}

// engCoreListFor is Core's list holding every node the cells name, and extra:
// a save is then refused for the leg under test, not for a name Core's list
// does not hold.
func engCoreListFor(cells []domain.FlowCell, extra ...protocol.NodeInfo) []protocol.NodeInfo {
	list := append([]protocol.NodeInfo(nil), extra...)
	known := map[string]bool{}
	for _, c := range cells {
		for _, n := range []string{c.CoreNodeName, c.PairedCoreNode, c.InboundSource, c.OutboundDestination,
			c.InboundStaging, c.OutboundStaging, c.PartialDestination} {
			if n != "" && !known[n] {
				known[n] = true
				list = append(list, protocol.NodeInfo{Name: n})
			}
		}
	}
	return list
}

// composerLegCensus is the flow composer's save, per leg a two_robot cell
// carries. The validator refuses each, and the store's write behind it would.
var composerLegCensus = []struct{ leg, want string }{
	{"inbound_source", engLegRefused},
	{"outbound_destination", engLegRefused},
	{"inbound_staging", engLegRefused},
}

func TestSaveFlow_ALaneOnEachLeg(t *testing.T) {
	t.Parallel()
	for _, row := range composerLegCensus {
		t.Run(row.leg, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, _, toStyleID := seedFlowScenario(t, db)
			eng := testEngine(t, db)
			enableComposer(t, db, processID)
			cells := cellsOf(t, db, toStyleID)
			eng.SetCoreNodes(engCoreListFor(cells, engLaneCore[:2]...))

			fp, err := eng.FlowFingerprint(processID, toStyleID)
			testutil.MustNoErr(t, err, "fingerprint")
			for i := range cells {
				if cells[i].CoreNodeName != "FLOW-ADD" {
					continue
				}
				switch row.leg {
				case "inbound_source":
					cells[i].InboundSource = "CW-LANE"
				case "outbound_destination":
					cells[i].OutboundDestination = "CW-LANE"
				case "inbound_staging":
					cells[i].InboundStaging = "CW-LANE"
				}
			}
			_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID,
				Cells: cells, Fingerprint: fp, CalledBy: "Press 400"})
			stored := engStoredLane(t, db, toStyleID, row.leg)
			var refused *FlowValidationError
			got := ""
			switch {
			case err == nil && stored:
				got = engLegSaved
			case errors.As(err, &refused) && !stored:
				got = engLegRefused
			default:
				t.Fatalf("save: err %v, lane stored %v", err, stored)
			}
			if got != row.want {
				t.Errorf("composer with a lane on %s: %s, want %s", row.leg, got, row.want)
			}
		})
	}
}

// reorderLegCensus is the replenishment page's reorder edit on a claim that
// already names a lane. The edit is about the reorder point, but the write
// echoes the claim's legs, so it is refused until the leg is fixed.
var reorderLegCensus = []struct{ leg, want string }{
	{"inbound_source", engLegRefused},
	{"outbound_destination", engLegRefused},
	{"containment_destination", engLegRefused},
	{"inbound_staging", engLegRefused},
	{"outbound_staging", engLegRefused},
}

func TestUpdateCellReorder_AClaimNamingALane(t *testing.T) {
	t.Parallel()
	for _, row := range reorderLegCensus {
		t.Run(row.leg, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			pid, err := db.CreateProcess("CW-PROC", "", "", "", false)
			testutil.MustNoErr(t, err, "create process")
			sid, err := db.CreateStyle("CW-STYLE", "", pid)
			testutil.MustNoErr(t, err, "create style")
			in := processes.NodeClaimInput{
				StyleID: sid, CoreNodeName: "CW-PRESS", Role: protocol.ClaimRoleProduce,
				SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-CW", ReorderPoint: 5,
				InboundSource: "CW-SUP", OutboundDestination: "CW-SUP",
				ContainmentDestination: "CW-HOLD",
				InboundStaging:         "CW-STG", OutboundStaging: "CW-STG2",
			}
			lane := "CW-LANE"
			switch row.leg {
			case "inbound_source":
				in.InboundSource = lane
			case "outbound_destination":
				in.OutboundDestination = lane
			case "containment_destination":
				in.ContainmentDestination = lane
			case "inbound_staging":
				in.InboundStaging = lane
			case "outbound_staging":
				in.OutboundStaging = lane
			}
			// Stored before the refusal existed: no node list to check it.
			claimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, in)
			testutil.MustNoErr(t, err, "seed a stored lane claim")
			eng := testEngine(t, db)
			eng.SetCoreNodes(engLaneCore)

			err = eng.UpdateCellReorder(CellReorderInput{ClaimID: claimID, ReorderPoint: 25, Source: "manual"})
			c, rerr := db.GetStyleNodeClaim(claimID)
			testutil.MustNoErr(t, rerr, "read claim back")
			got := ""
			switch {
			case err == nil && c.ReorderPoint == 25:
				got = engLegSaved
			case err != nil && c.ReorderPoint == 5:
				got = engLegRefused
			default:
				t.Fatalf("reorder edit: err %v, reorder point %d", err, c.ReorderPoint)
			}
			if got != row.want {
				t.Errorf("reorder edit of a claim naming a lane on %s: %s, want %s", row.leg, got, row.want)
			}
		})
	}
}

func engStoredLane(t *testing.T, db *store.DB, styleID int64, leg string) bool {
	t.Helper()
	claims, err := db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "list claims")
	for _, c := range claims {
		v := map[string]string{
			"inbound_source": c.InboundSource, "outbound_destination": c.OutboundDestination,
			"inbound_staging": c.InboundStaging,
		}[leg]
		if v == "CW-LANE" {
			return true
		}
	}
	return false
}

// TestSaveFlow_AMemberWhereCorePicks: the claim editor's save (the flow
// composer), handed a member of a node group on each leg a two_robot cell
// carries. Where Core picks the member is refused, on its field; as the cell's
// staging spot it saves, because a staging spot names the exact node.
func TestSaveFlow_AMemberWhereCorePicks(t *testing.T) {
	t.Parallel()
	for _, row := range []struct{ leg, want string }{
		{"inbound_source", engLegRefused},
		{"outbound_destination", engLegRefused},
		{"inbound_staging", engLegSaved},
	} {
		t.Run(row.leg, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, _, toStyleID := seedFlowScenario(t, db)
			eng := testEngine(t, db)
			enableComposer(t, db, processID)
			cells := cellsOf(t, db, toStyleID)
			// Core's list holds every node the cells name, and a group with its member.
			eng.SetCoreNodes(engCoreListFor(cells,
				protocol.NodeInfo{Name: "CW-GRP", NodeType: protocol.NodeClassNGRP}, protocol.NodeInfo{Name: "CW-GRP.CW-SLOT"}))

			fp, err := eng.FlowFingerprint(processID, toStyleID)
			testutil.MustNoErr(t, err, "fingerprint")
			for i := range cells {
				if cells[i].CoreNodeName != "FLOW-ADD" {
					continue
				}
				switch row.leg {
				case "inbound_source":
					cells[i].InboundSource = "CW-SLOT"
				case "outbound_destination":
					cells[i].OutboundDestination = "CW-SLOT"
				case "inbound_staging":
					cells[i].InboundStaging = "CW-SLOT"
				}
			}
			_, err = eng.SaveFlow(processID, FlowSaveRequest{Source: domain.ClaimSourceHMI, ToStyleID: toStyleID,
				Cells: cells, Fingerprint: fp, CalledBy: "Press 400"})
			stored := engStoredNode(t, db, toStyleID, row.leg, "CW-SLOT")
			var refused *FlowValidationError
			got := ""
			switch {
			case err == nil && stored:
				got = engLegSaved
			case errors.As(err, &refused) && !stored:
				got = engLegRefused
				onField := false
				for _, f := range refused.Findings {
					onField = onField || string(f.Field) == row.leg
				}
				if !onField {
					t.Errorf("the refusal is not on %s: %+v", row.leg, refused.Findings)
				}
			default:
				t.Fatalf("save: err %v, member stored %v", err, stored)
			}
			if got != row.want {
				t.Errorf("composer with a member on %s: %s, want %s", row.leg, got, row.want)
			}
		})
	}
}

func engStoredNode(t *testing.T, db *store.DB, styleID int64, leg, name string) bool {
	t.Helper()
	claims, err := db.ListStyleNodeClaims(styleID)
	testutil.MustNoErr(t, err, "list claims")
	for _, c := range claims {
		v := map[string]string{
			"inbound_source": c.InboundSource, "outbound_destination": c.OutboundDestination,
			"inbound_staging": c.InboundStaging,
		}[leg]
		if v == name {
			return true
		}
	}
	return false
}

// TestRefusedSetups_ListsWhatTheRuleRefuses pins the Edge's report of saved
// setups, run when Core's node list changes: a claim, a routing row and a
// recipe saved before the rule, each naming a member of a group where Core
// picks, are each listed with the rule's sentence. The claim's staging spot
// naming the same member is not listed (a staging spot names the exact node),
// nor is a leg naming the group.
func TestRefusedSetups_ListsWhatTheRuleRefuses(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	pid, err := db.CreateProcess("RS-PROC", "", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	sid, err := db.CreateStyle("RS-STYLE", "", pid)
	testutil.MustNoErr(t, err, "create style")
	none := domain.CoreNodeKinds{}
	_, err = db.UpsertStyleNodeClaim(none, processes.NodeClaimInput{
		StyleID: sid, CoreNodeName: "RS-PRESS", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-RS",
		InboundSource: "RS-GRP", OutboundDestination: "RS-SLOT", InboundStaging: "RS-SLOT", OutboundStaging: "RS-SLOT",
	})
	testutil.MustNoErr(t, err, "a claim saved before the rule")
	_, err = db.UpsertRoutingNode(none, processes.RoutingNodeInput{ProcessID: pid, CoreNodeName: "RS-SLOT",
		Role: domain.RoutingRoleSource, Origin: domain.RoutingOriginEngineer})
	testutil.MustNoErr(t, err, "a routing row saved before the rule")
	_, err = db.Exec(`INSERT INTO flow_presets (process_id, name, version, flow_json, created_by) VALUES (?, ?, 1, ?, 'test')`,
		pid, "RS-RECIPE", `{"cells":[{"core_node_name":"RS-PRESS","swap_mode":"single_robot","outbound_destination":"RS-SLOT"}]}`)
	testutil.MustNoErr(t, err, "a recipe saved before the rule")

	eng := testEngine(t, db)
	if got := eng.refusedSetups(); len(got) != 0 {
		t.Fatalf("with no node list heard the report lists %q, want nothing", got)
	}
	eng.SetCoreNodes([]protocol.NodeInfo{{Name: "RS-GRP", NodeType: protocol.NodeClassNGRP},
		{Name: "RS-GRP.RS-SLOT"}, {Name: "RS-PRESS"}})
	got := eng.refusedSetups()
	member := protocol.MsgMemberIsCoresToPick + `: "RS-SLOT" is in "RS-GRP"`
	want := []string{
		"claim RS-PROC / RS-STYLE at RS-PRESS: outbound_destination: " + member,
		"routing set of RS-PROC: source row: " + member,
		"recipe RS-RECIPE v1 of RS-PROC, cell 1: outbound_destination: " + member,
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("the report lists\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
}
