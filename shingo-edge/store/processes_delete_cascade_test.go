package store

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// Deleting a process used to be one statement with the children left to
// ON DELETE CASCADE — and edge SQLite runs with foreign keys OFF, so that
// cascade never fired. Every process ever deleted left its styles, nodes and
// stations behind. The live Springfield edge carried five orphaned styles and an
// orphaned process_node that SetNodes was still willing to ADOPT by name onto a
// live station, an hour after the process that made it was deleted.
//
// These pin the two halves that matter: what must be gone, and what must NOT be
// touched on the way past.

// seedProcessWithChildren builds a process carrying one of everything the
// cascade has an opinion about.
func seedProcessWithChildren(t *testing.T, db *DB, name string) (processID, styleID, nodeID, stationID int64) {
	t.Helper()
	processID, err := db.CreateProcess(name, "", "", "", "", false)
	if err != nil {
		t.Fatalf("CreateProcess: %v", err)
	}
	styleID, err = db.CreateStyle("S-"+name, "", processID)
	if err != nil {
		t.Fatalf("CreateStyle: %v", err)
	}
	if _, err := db.CreateReportingPoint("PLC-"+name, "TAG-"+name, styleID); err != nil {
		t.Fatalf("CreateReportingPoint: %v", err)
	}
	stationID, err = db.CreateOperatorStation(stations.Input{ProcessID: processID, Name: "ST-" + name})
	if err != nil {
		t.Fatalf("CreateOperatorStation: %v", err)
	}
	nodeID, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, OperatorStationID: &stationID,
		CoreNodeName: "N-" + name, Name: "N-" + name, Enabled: true,
	})
	if err != nil {
		t.Fatalf("CreateProcessNode: %v", err)
	}
	if _, err := db.EnsureProcessNodeRuntime(nodeID); err != nil {
		t.Fatalf("EnsureProcessNodeRuntime: %v", err)
	}
	// The permanent production record, and the two name-keyed mirrors.
	if _, err := db.Exec(`INSERT INTO hourly_counts (process_id, style_id, bucket_start, delta)
		VALUES (?, ?, 1787734800, 42)`, processID, styleID); err != nil {
		t.Fatalf("insert hourly: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO sourcing_state (process_id, style_id, status)
		VALUES (?, 'Default', 'green')`, name); err != nil {
		t.Fatalf("insert sourcing_state: %v", err)
	}
	return processID, styleID, nodeID, stationID
}

func count(t *testing.T, db *DB, q string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("count (%s): %v", q, err)
	}
	return n
}

func TestDeleteProcess_RetiresItsChildren(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, sid, nid, stid := seedProcessWithChildren(t, db, "Bin Unloader")

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM processes WHERE id=?`, pid); n != 0 {
		t.Errorf("process row survived: %d", n)
	}

	// THE ONE THAT CAUSED THE INCIDENT. Soft delete rather than hard, because
	// changeover_node_tasks would CASCADE-destroy its detail — but retired is
	// what matters here: ListNodesByProcess filters on deleted_at, so the row
	// can no longer be adopted by core_node_name.
	if n := count(t, db, `SELECT COUNT(*) FROM process_nodes WHERE id=? AND deleted_at IS NOT NULL`, nid); n != 1 {
		t.Errorf("process_node not retired")
	}
	live, err := processes.ListNodesByProcess(db.DB, pid)
	if err != nil {
		t.Fatalf("ListNodesByProcess: %v", err)
	}
	if len(live) != 0 {
		t.Errorf("retired node still adoptable: %+v", live)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM process_node_runtime_states WHERE process_node_id=?`, nid); n != 0 {
		t.Errorf("runtime state survived: stale state for an unaddressable node")
	}

	// Styles retire and their reporting points stop polling, exactly as
	// DeleteStyle does it.
	if n := count(t, db, `SELECT COUNT(*) FROM styles WHERE id=? AND deleted_at IS NOT NULL`, sid); n != 1 {
		t.Errorf("style not retired")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM reporting_points WHERE style_id=? AND enabled=1`, sid); n != 0 {
		t.Errorf("reporting point still enabled — it would keep polling a PLC for a dead style")
	}

	if n := count(t, db, `SELECT COUNT(*) FROM operator_stations WHERE id=?`, stid); n != 0 {
		t.Errorf("operator_station survived: it lists via LEFT JOIN, so it would render with a blank process")
	}
	// Keyed on the process NAME, so a later process reusing the name would
	// inherit the stale row.
	if n := count(t, db, `SELECT COUNT(*) FROM sourcing_state WHERE process_id='Bin Unloader'`); n != 0 {
		t.Errorf("sourcing_state survived")
	}
}

// The production record is the thing a config action must never be able to
// reach. Since the 2026-09 UTC move that record is hourly_counts ALONE:
// daily_counts was dropped because it cached a SUM of these very rows, and the
// hours are now kept permanently instead of being purged at 90 days. So this
// assertion carries the whole weight that used to be split across two tables —
// delete a process and its counting history must still be there.
func TestDeleteProcess_LeavesTheProductionRecord(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _, _, _ := seedProcessWithChildren(t, db, "SNF9")

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM hourly_counts WHERE process_id=?`, pid); n != 1 {
		t.Errorf("hourly_counts destroyed by a config action: got %d", n)
	}
}

// A process delete takes the process's lineside piles with it, active and
// stranded, in the same transaction. It used to refuse while a pile held parts
// (ErrProcessHasStock); the brief's U3 deletes that precondition, and the
// engine sends each deleted pile's level as 0 so Core's mirror loses it too.
func TestDeleteProcess_DeletesItsPiles(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _, nid, _ := seedProcessWithChildren(t, db, "P-Stocked")

	if _, err := db.Exec(`INSERT INTO node_lineside_bucket (node_id, payload_code, qty, state)
		VALUES (?, 'SYN-PART12E.06', 240, 'active'), (?, 'SYN-PART12E.06', 7, 'stranded')`, nid, nid); err != nil {
		t.Fatalf("insert piles: %v", err)
	}

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM processes WHERE id=?`, pid); n != 0 {
		t.Errorf("process still present after the delete")
	}
	if n := count(t, db, `SELECT COUNT(*) FROM node_lineside_bucket WHERE node_id=?`, nid); n != 0 {
		t.Errorf("%d pile(s) survived the process delete, want 0", n)
	}
}

// Deleting something that is already gone is not an error — the handler maps a
// failure to a 500, and a double-click is not a server fault.
func TestDeleteProcess_MissingIsNotAnError(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.DeleteProcess(99999); err != nil {
		t.Fatalf("deleting a missing process: %v", err)
	}
}

// ── The demand episodes a process delete leaves behind ────────────────────
//
// demand_origins_open is Edge's half of the demand grain: one row per OPEN
// episode, keyed on episode_key, carrying the process NAME in process_id. The
// other half lives on Core, which is told about an episode by a demand.origin
// message on the durable outbox — opened once, closed once, both as whole
// STATE rather than as events.
//
// Deleting a process removes its open rows in the delete's own transaction and
// sends Core NOTHING, so Core's rows stay open with no Edge row left to close
// them. These pin that as it stands, which is what makes a later change to it
// visible rather than plausible.

// seedOpenEpisode opens one demand episode for a process NAME, the way the
// engine's mint path does. Returns the episode key.
func seedOpenEpisode(t *testing.T, db *DB, processName, payload string, role protocol.ClaimRole) string {
	t.Helper()
	key := protocol.CellEpisodeKey(processName, payload, role)
	expected := 2
	if err := db.OpenDemandOrigin(&OpenOrigin{
		EpisodeKey: key, OriginID: "origin-" + key, Kind: protocol.EpisodeKindCell,
		Direction: role, TriggerKind: protocol.EpisodeTriggerAutoreorder,
		TriggerRef: "claim:1", ProcessID: processName, CoreNodeName: "SYN_NODE01",
		PayloadCode: payload, Threshold: 50, ExpectedOrders: &expected,
		OpenedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("open demand origin %s: %v", key, err)
	}
	return key
}

// TestDeleteProcess_DropsOpenEpisodesAndTellsCoreNothing is the characterisation
// of the delete as it stands: the open rows for this process's name go with the
// process, another process's episode is untouched, and not one byte reaches the
// outbox — so Core keeps every one of those episodes open, indefinitely, with
// nothing left on Edge that could ever close them.
func TestDeleteProcess_DropsOpenEpisodesAndTellsCoreNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	pid, _, _, _ := seedProcessWithChildren(t, db, "EPI-DELETED")
	seedOpenEpisode(t, db, "EPI-DELETED", "SYN-PANEL-B", protocol.ClaimRoleConsume)
	seedOpenEpisode(t, db, "EPI-DELETED", "SYN-PANEL-B", protocol.ClaimRoleProduce)

	// A bystander: same table, different process. The delete is keyed on the
	// NAME, so this row is the evidence that the key is doing the work.
	if _, err := db.CreateProcess("EPI-BYSTANDER", "", "", "", "", false); err != nil {
		t.Fatalf("create bystander process: %v", err)
	}
	seedOpenEpisode(t, db, "EPI-BYSTANDER", "SYN-PANEL-B", protocol.ClaimRoleConsume)

	if n := count(t, db, `SELECT COUNT(*) FROM demand_origins_open`); n != 3 {
		t.Fatalf("fixture: %d open episodes, want 3", n)
	}

	if err := db.DeleteProcess(pid); err != nil {
		t.Fatalf("DeleteProcess: %v", err)
	}

	if n := count(t, db, `SELECT COUNT(*) FROM demand_origins_open WHERE process_id='EPI-DELETED'`); n != 0 {
		t.Errorf("open episode rows survived the delete: %d", n)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM demand_origins_open WHERE process_id='EPI-BYSTANDER'`); n != 1 {
		t.Errorf("another process's episode was taken with it: %d rows left, want 1", n)
	}

	// THE PIN THAT MATTERS. Deleting a process is silent: no demand.origin
	// close, no message of any kind. Core's rows for those two episodes stay
	// open until its own childless pass sweeps them under a reason nobody asked
	// for, and the Edge rows that could have closed them are already gone.
	if n := count(t, db, `SELECT COUNT(*) FROM outbox`); n != 0 {
		t.Errorf("the delete enqueued %d outbox message(s); today it sends Core nothing", n)
	}
}

// Deleting an already-deleted process is not an error and must stay silent: a
// double-click is not a reason to send Core anything.
func TestDeleteProcess_MissingSendsNothing(t *testing.T) {
	t.Parallel()
	db := testDB(t)
	if err := db.DeleteProcess(99999); err != nil {
		t.Fatalf("deleting a missing process: %v", err)
	}
	if n := count(t, db, `SELECT COUNT(*) FROM outbox`); n != 0 {
		t.Errorf("deleting a missing process enqueued %d outbox message(s), want 0", n)
	}
}
