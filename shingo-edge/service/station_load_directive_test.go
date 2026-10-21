package service

import (
	"context"
	"testing"

	"shingo/protocol"
	"shingoedge/domain"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// ---------------------------------------------------------------------------
// A press-index position adopted onto a board beside a loader that carries the
// changeover load directive. The setting no longer rides the station's JSON (no
// glass reader); the engine reads it from the loader (stationTakesLoadDirective).
// What is pinned here is that the position is no loader tile.
// ---------------------------------------------------------------------------

// loadDirectiveScenario builds a station with a produce LOADER carrying the
// directive flag, plus a press-index press whose extension position is adopted onto
// the board through its owning task — the shape that renders a claimless position.
// directiveLoaders answers which Core loader a node is, with the station's
// directive setting on it. Only LDR_1 is a loader here.
type directiveLoaders struct{ on bool }

func (d directiveLoaders) LoaderForNode(n domain.NodeID) (*domain.Loader, error) {
	if n != "LDR_1" {
		return nil, nil
	}
	return domain.NewSharedWindowLoader("loader:1", domain.RoleProduce,
		domain.ReplenishmentOperator,
		[]domain.Window{{Node: "LDR_1"}}, []domain.PayloadCode{"PART-OLD", "PART-NEW"},
		domain.WithChangeoverLoadDirective(d.on))
}

func loadDirectiveScenario(t *testing.T, loaderFlag bool) (*store.DB, int64) {
	t.Helper()
	db := testdb.Open(t)

	processID, err := db.CreateProcess("LD-PROC", "load directive", "", "", false)
	mustNoErr(t, err, "create process")
	stationID, err := db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "LD-ST", Name: "LD Station", Sequence: 1, Enabled: true,
	})
	mustNoErr(t, err, "create station")

	loaderNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "LDR_1", Code: "LDR1", Name: "Loader 1", Sequence: 1, Enabled: true,
	})
	mustNoErr(t, err, "create loader node")
	pressNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "PLN_1", Code: "PLN1", Name: "Press 1", Sequence: 2, Enabled: true,
	})
	mustNoErr(t, err, "create press node")
	// Stationless: adopted onto this board through its owning task.
	positionNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PLN_2", Code: "PLN2", Name: "Press 1 Position", Sequence: 3, Enabled: true,
	})
	mustNoErr(t, err, "create position node")

	fromStyleID, err := db.CreateStyle("LD-FROM", "from", processID)
	mustNoErr(t, err, "create from style")
	toStyleID, err := db.CreateStyle("LD-TO", "to", processID)
	mustNoErr(t, err, "create to style")
	mustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// The loader: produce role, carrying the directive flag, is Core's
	// (directiveLoaders): its window's claim is the one SynthClaim builds, a
	// shared loader that can carry both styles' payloads.

	// The press-index parent. Its position inherits the
	// whole struct, and must still not read as a loader's card.
	pressClaimID, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PLN_1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "PLN_2",
		PayloadCode: "PART-OLD", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
		// Serves both payloads, so nothing about the part hides the position:
		// only its not being a loader's window does.
		AllowedPayloadCodes: []string{"PART-OLD", "PART-NEW"},
	})
	mustNoErr(t, err, "upsert press claim")

	// The incoming style wants a different payload at the press — that is what
	// the loader is being told to go and fetch carriers for.
	_, err = db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: toStyleID, CoreNodeName: "PLN_1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "PLN_2",
		PayloadCode: "PART-NEW", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
	})
	mustNoErr(t, err, "upsert to claim")

	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, from_style_id, to_style_id, state, called_by)
		VALUES (?, ?, ?, 'active', 'load-directive-test')`, processID, fromStyleID, toStyleID)
	mustNoErr(t, err, "insert changeover")
	changeoverID, _ := res.LastInsertId()

	// The board reads the incoming style's claims off processes.target_style_id,
	// not off the changeover row — StartProcessChangeover sets both, so a
	// fixture that writes only the changeover has no target claims at all.
	_, err = db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyleID, processID)
	mustNoErr(t, err, "set target style")

	// The press owns a task, which is what makes this station the one running
	// the changeover — the anchor the stationless position is adopted against.
	pres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state)
		VALUES (?, ?, ?, ?, 'swap', 'swap_required')`, changeoverID, pressNodeID, pressClaimID, pressClaimID)
	mustNoErr(t, err, "insert press task")
	pressTaskID, _ := pres.LastInsertId()

	// The position owns its OWN task, keyed to the PARENT claim — the fanned-out
	// shape pressPositionClaimsForBoard resolves through.
	sres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state)
		VALUES (?, ?, ?, ?, 'swap', 'swap_required')`, changeoverID, positionNodeID, pressClaimID, pressClaimID)
	mustNoErr(t, err, "insert position task")
	positionTaskID, _ := sres.LastInsertId()

	for _, p := range []struct {
		name  string
		node  int64
		role  string
		owner int64
	}{
		{"PLN_1", pressNodeID, domain.ParticipantRoleTask, pressTaskID},
		{"PLN_2", positionNodeID, domain.ParticipantRoleTask, positionTaskID},
	} {
		_, err = db.Exec(`INSERT INTO changeover_participants
			(process_changeover_id, core_node_name, process_node_id, role, owning_task_id)
			VALUES (?, ?, ?, ?, ?)`, changeoverID, p.name, p.node, p.role, p.owner)
		mustNoErr(t, err, "insert participant "+p.name)
	}

	_ = loaderNodeID
	return db, stationID
}

func mustNoErr(t *testing.T, err error, what string) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

func tileFor(t *testing.T, view *store.OperatorStationView, coreNodeName string) *domain.StationNodeView {
	t.Helper()
	for i := range view.Nodes {
		if view.Nodes[i].Node.CoreNodeName == coreNodeName {
			return &view.Nodes[i]
		}
	}
	t.Fatalf("no tile for %s on the board", coreNodeName)
	return nil
}

// TestBuildView_BackPositionRendersNoDirective: SynthesizePositionClaim is a
// whole-struct copy of the parent, so a position inherits the press claim. A
// press position is no loader's, so its tile carries no loader facts.
func TestBuildView_BackPositionRendersNoDirective(t *testing.T) {
	t.Parallel()
	db, stationID := loadDirectiveScenario(t, true)
	svc := NewStationService(db)
	svc.SetLoaderResolver(directiveLoaders{on: true})

	view, err := svc.BuildView(context.Background(), stationID)
	mustNoErr(t, err, "BuildView")

	position := tileFor(t, view, "PLN_2")
	if position.ActiveClaim == nil {
		t.Fatal("the position rendered claimless — this fixture exists to exercise the synthesized " +
			"position claim, so the scenario is not set up as intended")
	}
	if position.Loader != nil {
		t.Errorf("a press position carries loader facts: %+v\n"+
			"A press position loads nothing; only a Core loader's window or position is a loader tile.",
			position.Loader)
	}
}
