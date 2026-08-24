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
// The load-directive merge seam in BuildView had no test anywhere.
//
// The golden that was cited as its evidence cannot see it: the golden fixture
// predates the feature, contains zero claims, and the field is `omitempty`. The
// golden is byte-identical with the block absent, misplaced, or unguarded —
// which makes it evidence of nothing about this seam. These are the focused
// tests instead, because the golden pins the whole payload and is a poor place
// to learn why one field matters.
// ---------------------------------------------------------------------------

// loadDirectiveScenario builds a station with a produce LOADER carrying the
// directive flag, plus a press-index press whose extension seat is adopted onto
// the board through its owning task — the shape that renders a claimless seat.
func loadDirectiveScenario(t *testing.T, loaderFlag bool) (*store.DB, int64) {
	t.Helper()
	db := testdb.Open(t)

	processID, err := db.CreateProcess("LD-PROC", "load directive", "active_production", "", "", false)
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
	seatNodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "PLN_2", Code: "PLN2", Name: "Press 1 Seat", Sequence: 3, Enabled: true,
	})
	mustNoErr(t, err, "create seat node")

	fromStyleID, err := db.CreateStyle("LD-FROM", "from", processID)
	mustNoErr(t, err, "create from style")
	toStyleID, err := db.CreateStyle("LD-TO", "to", processID)
	mustNoErr(t, err, "create to style")
	mustNoErr(t, db.SetActiveStyle(processID, &fromStyleID), "set active style")

	// The loader: produce role, carrying the directive flag.
	loaderClaimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "LDR_1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeManualSwap, PayloadCode: "PART-OLD", UOPCapacity: 10,
		OutboundDestination: "MARKET",
		// A shared loader that can carry both styles' payloads. Without the
		// incoming one in its allowed set, loaderServes filters it out and the
		// directive is correctly empty — this loader would not be the one going
		// to fetch those carriers.
		AllowedPayloadCodes:     []string{"PART-OLD", "PART-NEW"},
		ChangeoverLoadDirective: domain.Ptr(loaderFlag),
	})
	mustNoErr(t, err, "upsert loader claim")

	// The press-index parent, ALSO carrying the flag — the case A8 is about.
	// Its seat inherits the whole struct, so if nothing clears the flag the
	// seat renders an instruction meant for a loader's card.
	pressClaimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: fromStyleID, CoreNodeName: "PLN_1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PairedCoreNode: "PLN_2",
		PayloadCode: "PART-OLD", UOPCapacity: 100,
		InboundSource: "MARKET", OutboundDestination: "MARKET",
		// Serves both payloads, so the directive builder's "can this node serve
		// the incoming payload" filter does NOT incidentally hide the seat. That
		// filter is why a first version of this test passed while the flag was
		// still reaching seats: the seat inherited PART-OLD and the incoming
		// payload was PART-NEW, so it was excluded for a reason that has nothing
		// to do with it being a seat.
		AllowedPayloadCodes:     []string{"PART-OLD", "PART-NEW"},
		ChangeoverLoadDirective: domain.Ptr(loaderFlag),
	})
	mustNoErr(t, err, "upsert press claim")

	// The incoming style wants a different payload at the press — that is what
	// the loader is being told to go and fetch carriers for.
	_, err = db.UpsertStyleNodeClaim(processes.NodeClaimInput{
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
	// fixture that writes only the changeover has no target claims at all and
	// the directive is built from an empty list.
	_, err = db.Exec(`UPDATE processes SET target_style_id=? WHERE id=?`, toStyleID, processID)
	mustNoErr(t, err, "set target style")

	// The press owns a task, which is what makes this station the one running
	// the changeover — the anchor the stationless seat is adopted against.
	pres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state)
		VALUES (?, ?, ?, ?, 'swap', 'swap_required')`, changeoverID, pressNodeID, pressClaimID, pressClaimID)
	mustNoErr(t, err, "insert press task")
	pressTaskID, _ := pres.LastInsertId()

	// The seat owns its OWN task, keyed to the PARENT claim — the fanned-out
	// shape pressPositionClaimsForBoard resolves through.
	sres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, from_claim_id, to_claim_id, situation, state)
		VALUES (?, ?, ?, ?, 'swap', 'swap_required')`, changeoverID, seatNodeID, pressClaimID, pressClaimID)
	mustNoErr(t, err, "insert seat task")
	seatTaskID, _ := sres.LastInsertId()

	for _, p := range []struct {
		name  string
		node  int64
		role  string
		owner int64
	}{
		{"PLN_1", pressNodeID, domain.ParticipantRoleTask, pressTaskID},
		{"PLN_2", seatNodeID, domain.ParticipantRoleTask, seatTaskID},
	} {
		_, err = db.Exec(`INSERT INTO changeover_participants
			(process_changeover_id, core_node_name, process_node_id, role, owning_task_id)
			VALUES (?, ?, ?, ?, ?)`, changeoverID, p.name, p.node, p.role, p.owner)
		mustNoErr(t, err, "insert participant "+p.name)
	}

	_ = loaderNodeID
	_ = loaderClaimID
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

// TestBuildView_LoadDirectiveReachesTheLoaderTile pins the seam itself: with the
// flag set and a bin-type resolver wired, the directive lands on the loader's
// tile naming the carrier and the cell waiting for it.
func TestBuildView_LoadDirectiveReachesTheLoaderTile(t *testing.T) {
	t.Parallel()
	db, stationID := loadDirectiveScenario(t, true)
	svc := NewStationService(db)
	svc.SetBinTypeResolver(func(payloadCode string) string {
		if payloadCode == "PART-NEW" {
			return "TOTE-BIG"
		}
		return ""
	})

	view, err := svc.BuildView(context.Background(), stationID)
	mustNoErr(t, err, "BuildView")

	loader := tileFor(t, view, "LDR_1")
	if loader.ChangeoverLoadDirective == nil {
		t.Fatal("the loader's card carries no directive — the flag is set, a changeover is active, " +
			"and the incoming style wants a payload whose bin type resolves. This is the seam the " +
			"golden cannot see: it predates the feature, holds zero claims, and the field is omitempty")
	}
	d := loader.ChangeoverLoadDirective
	if len(d.BinTypeCodes) != 1 || d.BinTypeCodes[0] != "TOTE-BIG" {
		t.Errorf("directive bin types = %v, want [TOTE-BIG] — the carrier is the instruction", d.BinTypeCodes)
	}
	if len(d.PayloadCodes) != 1 || d.PayloadCodes[0] != "PART-NEW" {
		t.Errorf("directive payloads = %v, want [PART-NEW]", d.PayloadCodes)
	}
	if len(d.ForNodes) == 0 || d.ForNodes[0] != "PLN_1" {
		t.Errorf("directive for-nodes = %v, want the press that is waiting", d.ForNodes)
	}
}

// TestBuildView_NoDirectiveWithoutTheFlag is the guard half: the flag is what
// turns a loader's card into an instruction, and a card that always shows one is
// a card whose directive nobody reads.
func TestBuildView_NoDirectiveWithoutTheFlag(t *testing.T) {
	t.Parallel()
	db, stationID := loadDirectiveScenario(t, false)
	svc := NewStationService(db)
	svc.SetBinTypeResolver(func(string) string { return "TOTE-BIG" })

	view, err := svc.BuildView(context.Background(), stationID)
	mustNoErr(t, err, "BuildView")

	if d := tileFor(t, view, "LDR_1").ChangeoverLoadDirective; d != nil {
		t.Errorf("a loader without the flag rendered a directive: %+v", d)
	}
}

// TestBuildView_BackSeatRendersNoDirective is A8, settled by test.
//
// SynthesizePressPositionClaim is a whole-struct copy of the parent, so a seat
// inherits ChangeoverLoadDirective, and the press-index parent is produce-role —
// which is the only other condition BuildChangeoverLoadDirective checks. Nothing
// else stood between a flagged press and every one of its seats showing a
// loading instruction on a tile that loads nothing.
func TestBuildView_BackSeatRendersNoDirective(t *testing.T) {
	t.Parallel()
	db, stationID := loadDirectiveScenario(t, true)
	svc := NewStationService(db)
	svc.SetBinTypeResolver(func(string) string { return "TOTE-BIG" })

	view, err := svc.BuildView(context.Background(), stationID)
	mustNoErr(t, err, "BuildView")

	seat := tileFor(t, view, "PLN_2")
	if seat.ActiveClaim == nil {
		t.Fatal("the seat rendered claimless — this fixture exists to exercise the synthesized " +
			"seat claim, so the scenario is not set up as intended")
	}
	if d := seat.ChangeoverLoadDirective; d != nil {
		t.Errorf("a press seat rendered a load directive: %+v\n"+
			"The directive is an instruction to a LOADER — go and fetch these carriers. A press "+
			"seat loads nothing, and it only has the flag because the synth copies the parent "+
			"struct wholesale.", d)
	}
}
