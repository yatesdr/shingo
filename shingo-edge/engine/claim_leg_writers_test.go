package engine

import (
	"errors"
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

var engLaneCore = []protocol.NodeInfo{
	{Name: "CW-SUP", NodeType: protocol.NodeClassNGRP},
	{Name: "CW-SUP.CW-LANE", NodeType: protocol.NodeClassLANE},
}

// composerLegCensus is the flow composer's save, per leg a two_robot cell
// carries, as it stands.
var composerLegCensus = []struct{ leg, want string }{
	{"inbound_source", engLegRefused},
	{"outbound_destination", engLegRefused},
	{"changeover_evac_destination", engLegSaved},
	{"inbound_staging", engLegSaved},
}

func TestSaveFlow_ALaneOnEachLeg(t *testing.T) {
	t.Parallel()
	for _, row := range composerLegCensus {
		t.Run(row.leg, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			processID, _, toStyleID := seedFlowScenario(t, db)
			eng := testEngine(t, db)
			eng.SetCoreNodes(engLaneCore)
			enableComposer(t, db, processID)

			fp, err := eng.FlowFingerprint(processID, toStyleID)
			testutil.MustNoErr(t, err, "fingerprint")
			cells := cellsOf(t, db, toStyleID)
			for i := range cells {
				if cells[i].CoreNodeName != "FLOW-ADD" {
					continue
				}
				switch row.leg {
				case "inbound_source":
					cells[i].InboundSource = "CW-LANE"
				case "outbound_destination":
					cells[i].OutboundDestination = "CW-LANE"
				case "changeover_evac_destination":
					cells[i].ChangeoverEvacDestination = "CW-LANE"
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
// already names a lane, as it stands. The edit is about the reorder point; the
// write echoes every leg.
var reorderLegCensus = []struct{ leg, want string }{
	{"inbound_source", engLegSaved},
	{"outbound_destination", engLegSaved},
	{"changeover_evac_destination", engLegSaved},
	{"containment_destination", engLegSaved},
	{"inbound_staging", engLegSaved},
	{"outbound_staging", engLegSaved},
}

func TestUpdateCellReorder_AClaimNamingALane(t *testing.T) {
	t.Parallel()
	for _, row := range reorderLegCensus {
		t.Run(row.leg, func(t *testing.T) {
			t.Parallel()
			db := testEngineDB(t)
			pid, err := db.CreateProcess("CW-PROC", "", "active_production", "", "", false)
			testutil.MustNoErr(t, err, "create process")
			sid, err := db.CreateStyle("CW-STYLE", "", pid)
			testutil.MustNoErr(t, err, "create style")
			evac := "CW-SUP"
			in := processes.NodeClaimInput{
				StyleID: sid, CoreNodeName: "CW-PRESS", Role: protocol.ClaimRoleProduce,
				SwapMode: protocol.SwapModeSingleRobot, PayloadCode: "PART-CW", ReorderPoint: 5,
				InboundSource: "CW-SUP", OutboundDestination: "CW-SUP",
				ChangeoverEvacDestination: &evac, ContainmentDestination: "CW-HOLD",
				InboundStaging: "CW-STG", OutboundStaging: "CW-STG2",
			}
			lane := "CW-LANE"
			switch row.leg {
			case "inbound_source":
				in.InboundSource = lane
			case "outbound_destination":
				in.OutboundDestination = lane
			case "changeover_evac_destination":
				in.ChangeoverEvacDestination = &lane
			case "containment_destination":
				in.ContainmentDestination = lane
			case "inbound_staging":
				in.InboundStaging = lane
			case "outbound_staging":
				in.OutboundStaging = lane
			}
			// Stored before the refusal existed: no node list to check it.
			claimID, err := db.UpsertStyleNodeClaim(in)
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
			"changeover_evac_destination": c.ChangeoverEvacDestination, "inbound_staging": c.InboundStaging,
		}[leg]
		if v == "CW-LANE" {
			return true
		}
	}
	return false
}
