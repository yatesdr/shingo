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

// station_child_tile_test.go — Fix A part 2: the affordance widening.
//
// Three narrowings hid a changeover node from its own station. A press-index
// extension seat is auto-created with no operator_station_id, so it fell
// through every one and appeared on no board at all — which is why the
// operators fork-trucked those seats.

// seatScenario builds a process with one stationed press node and one
// STATIONLESS seat node, an active changeover with a task on the press, and an
// indexed_over participant for the seat owned by that task. That is exactly the
// same-bin-type press-index shape: the seat owns no task and no station.
func seatScenario(t *testing.T) (db *store.DB, stationID, pressNodeID, seatNodeID, changeoverID int64) {
	t.Helper()
	db = testdb.Open(t)

	processID, err := db.CreateProcess("SEAT-PROC", "child tile", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	stationID, err = db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "SEAT-ST", Name: "Seat Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	pressNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "PLN_A1", Code: "PLNA1", Name: "Press A1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create press node: %v", err)
	}
	// The seat: NO operator_station_id — exactly how changeover_service.go
	// auto-creates an extension position.
	seatNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PLN_A2", Code: "PLNA2", Name: "Press A2 Seat", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create seat node: %v", err)
	}

	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, called_by)
		VALUES (?, 1, 'active', 'test')`, processID)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	changeoverID, _ = res.LastInsertId()

	tres, err := db.Exec(`INSERT INTO changeover_node_tasks
		(process_changeover_id, process_node_id, situation, state)
		VALUES (?, ?, 'swap', 'swap_required')`, changeoverID, pressNodeID)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	taskID, _ := tres.LastInsertId()

	for _, p := range []struct {
		name  string
		node  int64
		role  string
		owner *int64
	}{
		{"PLN_A1", pressNodeID, domain.ParticipantRoleTask, &taskID},
		{"PLN_A2", seatNodeID, domain.ParticipantRoleIndexedOver, &taskID},
	} {
		if _, err := db.Exec(`INSERT INTO changeover_participants
			(process_changeover_id, core_node_name, process_node_id, role, owning_task_id)
			VALUES (?, ?, ?, ?, ?)`, changeoverID, p.name, p.node, p.role, p.owner); err != nil {
			t.Fatalf("insert participant %s: %v", p.name, err)
		}
	}
	// NOTE: deliberately NO changeover_station_tasks row. That absence was the
	// third narrowing — it used to blank the task map for the whole station.
	return db, stationID, pressNodeID, seatNodeID, changeoverID
}

// TestListParticipantsWithStation_ResolvesSeatViaOwner pins the shared
// resolver: a stationless seat resolves to its owning task's node's station,
// and reports WHICH rule answered.
func TestListParticipantsWithStation_ResolvesSeatViaOwner(t *testing.T) {
	db, stationID, _, seatNodeID, changeoverID := seatScenario(t)

	parts, err := db.ListParticipantsWithStation(changeoverID)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	byName := map[string]processes.ParticipantWithStation{}
	for _, p := range parts {
		byName[p.CoreNodeName] = p
	}

	press := byName["PLN_A1"]
	if press.StationID == nil || *press.StationID != stationID || press.StationSource != "own" {
		t.Errorf("press resolved to %v/%q, want %d/own", press.StationID, press.StationSource, stationID)
	}

	seat := byName["PLN_A2"]
	if seat.StationID == nil {
		t.Fatal("stationless seat resolved to NO station — it would render nowhere, which is the bug")
	}
	if *seat.StationID != stationID {
		t.Errorf("seat station = %d, want %d (its owning task's node's station)", *seat.StationID, stationID)
	}
	if seat.StationSource != "owner" {
		t.Errorf("seat StationSource = %q, want owner", seat.StationSource)
	}
	if seat.ProcessNodeID == nil || *seat.ProcessNodeID != seatNodeID {
		t.Errorf("seat process_node_id = %v, want %d", seat.ProcessNodeID, seatNodeID)
	}
}

// TestBuildView_StationlessSeatRendersAsChildTile is the affordance itself: the
// seat must appear on the press's station, marked as a child so the board can
// suppress a release button it has no work for.
func TestBuildView_StationlessSeatRendersAsChildTile(t *testing.T) {
	db, stationID, _, seatNodeID, _ := seatScenario(t)
	svc := NewStationService(db)

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}

	var seat *domain.StationNodeView
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID == seatNodeID {
			seat = &view.Nodes[i]
			break
		}
	}
	if seat == nil {
		t.Fatal("stationless seat is absent from its press's station view — " +
			"this is the invisibility that made operators fork-truck those seats")
	}
	if seat.ChildOfNode == "" {
		t.Error("seat tile is not marked as a child; the board cannot suppress its release button")
	}
	if seat.ChildOfNode != "Press A1" {
		t.Errorf("ChildOfNode = %q, want the owning node's display name %q", seat.ChildOfNode, "Press A1")
	}
	// A child tile owns no task — that is precisely why it needs the marker
	// rather than a task-derived render.
	if seat.ChangeoverTask != nil {
		t.Errorf("seat unexpectedly owns a task (%+v); indexed_over seats mint none", seat.ChangeoverTask)
	}
}

// TestBuildView_TaskAttachesWithoutAStationTaskRow covers the third narrowing:
// the station has NO changeover_station_tasks row, which used to blank the task
// map entirely and leave every node on the station taskless.
func TestBuildView_TaskAttachesWithoutAStationTaskRow(t *testing.T) {
	db, stationID, pressNodeID, _, _ := seatScenario(t)
	svc := NewStationService(db)

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	if view.StationTask != nil {
		t.Fatal("scenario invariant broken: this station should have no station-task row")
	}

	for i := range view.Nodes {
		if view.Nodes[i].Node.ID != pressNodeID {
			continue
		}
		if view.Nodes[i].ChangeoverTask == nil {
			t.Fatal("press node has no ChangeoverTask despite one existing — " +
				"the station-task guard must not gate the node-task map")
		}
		return
	}
	t.Fatal("press node missing from its own station view")
}

// fanOutScenario is the seat scenario's sibling, and the shape that broke at
// Hopkinsville on 2026-07-28. Instead of riding along as an `indexed_over`
// child of the press's task, the stationless seat gets its OWN task — which is
// what a changeover does when it fans out and drops every press position
// independently (a tote->bin style change, where all four positions leave).
//
// Station resolution walks own -> owning-task's-node. For a SELF-owning task
// both hops land on the same stationless row, so the seat resolves to NO
// station and renders on no board at all. Two robots parked at those seats
// could not be released because there was no tile to press.
func fanOutScenario(t *testing.T) (db *store.DB, stationID, pressNodeID, seatNodeID int64) {
	t.Helper()
	db = testdb.Open(t)

	processID, err := db.CreateProcess("FANOUT-PROC", "fan out", "active_production", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	stationID, err = db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "FAN-ST", Name: "Fan Station", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	pressNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "PLN_B1", Code: "PLNB1", Name: "Press B1", Sequence: 1, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create press node: %v", err)
	}
	seatNodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID:    processID,
		CoreNodeName: "PLN_B2", Code: "PLNB2", Name: "Press B2 Seat", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create seat node: %v", err)
	}

	// The press-index claim the fan-out derives FROM. Only the FRONT position
	// carries a style_node_claims row; the seat exists solely as this claim's
	// paired_core_node. That asymmetry is the whole reason a fanned-out seat has
	// no claim of its own to render from.
	styleID, err := db.CreateStyle("FANOUT-STYLE", "", processID)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if err := db.SetActiveStyle(processID, &styleID); err != nil {
		t.Fatalf("set active style: %v", err)
	}
	claimID, err := db.UpsertStyleNodeClaim(processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN_B1", Role: protocol.ClaimRoleProduce,
		SwapMode: protocol.SwapModeTwoRobotPressIndex, PayloadCode: "TOTE-A",
		UOPCapacity: 120, PairedCoreNode: "PLN_B2",
		InboundSource: "SMN_IN", OutboundDestination: "SMN_OUT",
	})
	if err != nil {
		t.Fatalf("upsert press-index claim: %v", err)
	}

	res, err := db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, called_by)
		VALUES (?, 1, 'active', 'test')`, processID)
	if err != nil {
		t.Fatalf("insert changeover: %v", err)
	}
	changeoverID, _ := res.LastInsertId()

	// The fan-out: BOTH positions get their own drop task. Each records the
	// press-index claim it was planned from — a synthesized per-position claim
	// carries its PARENT's ID, so both tasks point at the one front-position row.
	mkTask := func(nodeID int64) int64 {
		tres, terr := db.Exec(`INSERT INTO changeover_node_tasks
			(process_changeover_id, process_node_id, from_claim_id, situation, state)
			VALUES (?, ?, ?, 'drop', 'staging_requested')`, changeoverID, nodeID, claimID)
		if terr != nil {
			t.Fatalf("insert task for node %d: %v", nodeID, terr)
		}
		id, _ := tres.LastInsertId()
		return id
	}
	pressTaskID := mkTask(pressNodeID)
	seatTaskID := mkTask(seatNodeID)

	// Both participants are role=task owning their OWN task — the seat's owner
	// is itself, which is what collapses station resolution to nil.
	for _, p := range []struct {
		name  string
		node  int64
		owner int64
	}{
		{"PLN_B1", pressNodeID, pressTaskID},
		{"PLN_B2", seatNodeID, seatTaskID},
	} {
		if _, err := db.Exec(`INSERT INTO changeover_participants
			(process_changeover_id, core_node_name, process_node_id, role, owning_task_id)
			VALUES (?, ?, ?, ?, ?)`, changeoverID, p.name, p.node, domain.ParticipantRoleTask, p.owner); err != nil {
			t.Fatalf("insert participant %s: %v", p.name, err)
		}
	}
	return db, stationID, pressNodeID, seatNodeID
}

// TestBuildView_FannedOutSeatStillRenders is the regression pin for HK
// 2026-07-28: a stationless seat that owns its OWN task must still appear on
// the board running the changeover, or the robot parked there can never be
// released.
func TestBuildView_FannedOutSeatStillRenders(t *testing.T) {
	db, stationID, _, seatNodeID := fanOutScenario(t)
	svc := NewStationService(db)

	view, err := svc.BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}

	var seat *domain.StationNodeView
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID == seatNodeID {
			seat = &view.Nodes[i]
			break
		}
	}
	if seat == nil {
		t.Fatal("fanned-out stationless seat is absent from the board running its changeover — " +
			"its parked robot would have no tile to release from (HK 2026-07-28)")
	}
	// It owns real work, so it must carry its task: isReleaseReady reads
	// changeover_task and bails without it, so no task means no blue
	// release-ready glow no matter how the tile renders.
	if seat.ChangeoverTask == nil {
		t.Fatal("fanned-out seat has no ChangeoverTask — the release-ready glow keys on it")
	}
	// Owning its own task, it is nobody's child; naming itself its own parent
	// would be meaningless.
	if seat.ChildOfNode != "" {
		t.Errorf("ChildOfNode = %q, want empty — a self-owning seat is its own tile", seat.ChildOfNode)
	}
}

// TestBuildView_StationlessSeatStaysOffUnrelatedBoards guards the blast radius
// of the orphan fallback: adopting "any stationless participant" must not leak
// a seat onto a station that owns none of the changeover's work. A permanent
// tile on a paired on-deck position is actively harmful — LoadBin refuses to
// stamp a part there because doing so hung a press-index swap once already.
func TestBuildView_StationlessSeatStaysOffUnrelatedBoards(t *testing.T) {
	db, _, _, seatNodeID := fanOutScenario(t)

	// A second station on the same process that owns NO changeover task.
	var processID int64
	if err := db.QueryRow(`SELECT process_id FROM process_nodes WHERE id=?`, seatNodeID).Scan(&processID); err != nil {
		t.Fatalf("read process id: %v", err)
	}
	otherID, err := db.CreateOperatorStation(stations.Input{
		ProcessID: processID, Code: "OTHER-ST", Name: "Other Station", Sequence: 2, Enabled: true,
	})
	if err != nil {
		t.Fatalf("create other station: %v", err)
	}
	if _, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &otherID,
		CoreNodeName: "PLN_Z9", Code: "PLNZ9", Name: "Unrelated", Sequence: 1, Enabled: true,
	}); err != nil {
		t.Fatalf("create unrelated node: %v", err)
	}

	view, err := NewStationService(db).BuildView(context.Background(), otherID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID == seatNodeID {
			t.Fatal("stationless seat leaked onto a board that owns none of the changeover's work")
		}
	}
}

// seatView finds one node's view on a station board, failing the test if the
// tile is absent (an absent tile is its own regression, pinned above).
func seatView(t *testing.T, db *store.DB, stationID, nodeID int64) *domain.StationNodeView {
	t.Helper()
	view, err := NewStationService(db).BuildView(context.Background(), stationID)
	if err != nil {
		t.Fatalf("BuildView: %v", err)
	}
	for i := range view.Nodes {
		if view.Nodes[i].Node.ID == nodeID {
			return &view.Nodes[i]
		}
	}
	t.Fatalf("node %d missing from station %d view", nodeID, stationID)
	return nil
}

// TestBuildView_FannedOutSeatGetsSynthesizedClaim is the regression pin for HK
// 2026-08-05 (P400 changeover 51, tote -> bin). The seat renders — that was
// fixed on 2026-07-28 — but it rendered CLAIMLESS, and the operator modal keys
// its whole action region off the claim. The tile glowed release-ready off the
// TASK while the modal behind it offered no buttons at all, so two robots sat
// at a staged wait until the operator cancelled their orders.
//
// The view must derive the same per-position claim the planner built.
func TestBuildView_FannedOutSeatGetsSynthesizedClaim(t *testing.T) {
	db, stationID, pressNodeID, seatNodeID := fanOutScenario(t)

	seat := seatView(t, db, stationID, seatNodeID)
	if seat.ActiveClaim == nil {
		t.Fatal("fanned-out seat has no ActiveClaim — every claim-keyed gate fails closed " +
			"and its modal renders no release button (HK 2026-08-05)")
	}
	if seat.ActiveClaim.CoreNodeName != "PLN_B2" {
		t.Errorf("synthesized claim CoreNodeName = %q, want the SEAT's own name PLN_B2 — "+
			"handing it the parent's identity would make the tile act on the parent's work",
			seat.ActiveClaim.CoreNodeName)
	}
	if seat.ActiveClaim.SwapMode != domain.SwapModePressPosition {
		t.Errorf("synthesized claim SwapMode = %q, want %q",
			seat.ActiveClaim.SwapMode, domain.SwapModePressPosition)
	}
	// Inherited from the parent — the payload physically on the seat.
	if seat.ActiveClaim.PayloadCode != "TOTE-A" {
		t.Errorf("synthesized claim PayloadCode = %q, want TOTE-A inherited from the parent claim",
			seat.ActiveClaim.PayloadCode)
	}
	// Geometry fields must be cleared, or the seat reads as its own parent and
	// the planner/view could fan it out again.
	if seat.ActiveClaim.PairedCoreNode != "" || seat.ActiveClaim.SecondPairedCoreNode != "" {
		t.Errorf("synthesized claim kept press-index geometry (paired=%q second=%q), want both cleared",
			seat.ActiveClaim.PairedCoreNode, seat.ActiveClaim.SecondPairedCoreNode)
	}

	// The front position must keep its REAL persisted claim, untouched.
	press := seatView(t, db, stationID, pressNodeID)
	if press.ActiveClaim == nil || press.ActiveClaim.SwapMode != protocol.SwapModeTwoRobotPressIndex {
		t.Errorf("front position claim = %+v, want the persisted two_robot_press_index row", press.ActiveClaim)
	}
}

// TestBuildView_FannedOutSeatRoutesToPlainRelease pins the modal path the
// synthesized claim must land on. operator-modal.js picks its action region in
// order: manual_swap loader UI, then swap_ready (consolidated two-robot
// release), then the two_robot "WAITING FOR OTHER ROBOT" hold, then the plain
// `staged` single-order RELEASE.
//
// A fanned-out seat is a standalone evac with no sibling leg, so it must fall
// all the way through to the LAST branch — the same one the front position used
// successfully at Hopkinsville. Landing on either two-robot branch would give
// the operator a disabled WAITING button and reproduce the outage in a new
// costume, which is why this asserts the misses and not just the hit.
func TestBuildView_FannedOutSeatRoutesToPlainRelease(t *testing.T) {
	db, stationID, _, seatNodeID := fanOutScenario(t)

	seat := seatView(t, db, stationID, seatNodeID)
	if seat.ActiveClaim == nil {
		t.Fatal("no ActiveClaim; covered by TestBuildView_FannedOutSeatGetsSynthesizedClaim")
	}
	if seat.ActiveClaim.SwapMode == protocol.SwapModeManualSwap {
		t.Error("seat claims manual_swap — the modal would render the loader demand queue, " +
			"and LoadBin must never stamp a part on an on-deck press position")
	}
	if seat.ActiveClaim.SwapMode.IsTwoRobot() {
		t.Errorf("SwapMode %q reports IsTwoRobot — ComputeSwapReady would gate the seat on a "+
			"sibling leg it does not have, disabling its release", seat.ActiveClaim.SwapMode)
	}
	if seat.SwapReady {
		t.Error("seat reports swap_ready — the modal would offer the consolidated two-robot " +
			"release for a standalone evac with no sibling")
	}
}

// TestBuildView_IndexedOverSeatStaysClaimless is the blast-radius guard on the
// fallback: it must fire ONLY for a seat that owns its own task. A same-bin-type
// press-index seat rides along on the front position's task, owns no work, and
// renders as a child tile that deliberately offers nothing. Giving it a claim
// would make it look actionable and re-open the LoadBin hazard the child-tile
// branch exists to prevent.
func TestBuildView_IndexedOverSeatStaysClaimless(t *testing.T) {
	db, stationID, _, seatNodeID, _ := seatScenario(t)

	seat := seatView(t, db, stationID, seatNodeID)
	if seat.ChangeoverTask != nil {
		t.Fatal("scenario invariant broken: an indexed_over seat mints no task")
	}
	if seat.ActiveClaim != nil {
		t.Errorf("indexed_over seat was given a claim (%+v) — it owns no work, and a claim "+
			"makes an on-deck position look actionable", seat.ActiveClaim)
	}
}
