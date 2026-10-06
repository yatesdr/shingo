package store

import (
	"database/sql"
	"errors"
	"testing"

	"shingoedge/store/processes"
)

// process_node_lookup_test.go — resolving a Core node name to an Edge row when
// the name names more than one row.
//
// core_node_name is plant-unique on CORE (nodes.name is TEXT NOT NULL UNIQUE).
// It is NOT unique in process_nodes: one physical slot is legitimately named by
// several processes, which is what a shared loader window IS. The lookup was a
// bare QueryRow with no ORDER BY, so it took whatever row the engine handed back
// first — not merely unscoped but UNSTABLE, able to answer differently across
// restarts or an index change, while six callers believed they had THE node.
//
// One of those callers is the UOP adjustment handler. An adjustment resolved to
// the wrong process's row is a count written to the wrong slot.

// twoProcessesOneSlot builds the shared-window shape: two processes, each with a
// process_node naming the same Core slot.
func twoProcessesOneSlot(t *testing.T, db *DB, coreNodeName string) (firstID, secondID int64) {
	t.Helper()
	mk := func(procName, nodeName string) int64 {
		pid, err := db.CreateProcess(procName, "", "", "", false)
		if err != nil {
			t.Fatalf("create process %s: %v", procName, err)
		}
		id, err := db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, CoreNodeName: coreNodeName, Code: nodeName, Name: nodeName,
			Sequence: 1, Enabled: true,
		})
		if err != nil {
			t.Fatalf("create node for %s: %v", procName, err)
		}
		return id
	}
	return mk("SHARED-A", "A-WINDOW"), mk("SHARED-B", "B-WINDOW")
}

// TestGetProcessNodeByCoreNodeName_IsStableWhenTheNameIsAmbiguous pins the
// deterministic answer.
//
// It does NOT assert which process is "right", because there is no right one —
// the lookup has no process to scope by, and inventing a parameter for it at six
// callers that are trying to LEARN the process would be guessing wearing a
// signature. What it asserts is that the answer does not move: the same question
// gets the same row, so a UOP adjustment cannot land on process A today and
// process B after a restart.
//
// MUTATION, AND THE HONEST RESULT. Reversing the tie-break (ORDER BY n.id DESC)
// fails this, so the lowest-id assertion is real. REMOVING the ORDER BY entirely
// does NOT fail it — SQLite happens to return these rows in rowid order today,
// which is the same order the clause asks for.
//
// That is worth writing down rather than hiding, because it is the whole reason
// the clause is there. The old behaviour was not wrong on this machine on this
// day; it was UNGUARANTEED. A query with no ORDER BY is entitled to answer in
// any order, and the entitlement is cashed by an index change, a schema rebuild
// (which the FK repair performs), a VACUUM, or a different engine. This test
// cannot demonstrate that difference, because the incidental behaviour and the
// guaranteed one coincide — so what it pins is the tie-break's DIRECTION, and
// the ORDER BY is what turns today's coincidence into tomorrow's contract.
func TestGetProcessNodeByCoreNodeName_IsStableWhenTheNameIsAmbiguous(t *testing.T) {
	db := testDB(t)
	firstID, secondID := twoProcessesOneSlot(t, db, "SHARED_SLOT")
	if firstID == secondID {
		t.Fatalf("fixture: both nodes have id %d", firstID)
	}

	var got int64
	for i := 0; i < 5; i++ {
		n, err := db.GetProcessNodeByCoreNodeName("SHARED_SLOT")
		if err != nil {
			t.Fatalf("lookup %d: %v", i, err)
		}
		if n == nil {
			t.Fatalf("lookup %d returned no node for a name two rows carry", i)
		}
		if i == 0 {
			got = n.ID
			continue
		}
		if n.ID != got {
			t.Fatalf("lookup %d resolved to node %d, lookup 0 resolved to %d. The answer MOVED — "+
				"six callers turn a Core-originated name into an Edge row on the strength of this, "+
				"and an unstable answer means a UOP adjustment can land on one process today and "+
				"another after a restart", i, n.ID, got)
		}
	}
	if got != firstID {
		t.Errorf("resolved to node %d, want the lowest id %d — lowest-id-wins is the documented "+
			"tie-break, and a different one here means the ORDER BY changed without the doc", got, firstID)
	}
}

// TestGetProcessNodeByCoreNodeName_MissStaysErrNoRows pins the disposition two
// callers branch on.
//
// resolveProjectionNode and the delivered fallback both treat sql.ErrNoRows as
// the ORDINARY answer — "we do not own this destination" — rather than a fault,
// and both say so at length. Most deliveries land at a supermarket or staging
// node that is not an Edge process node at all. Swapping the miss for a
// different error, or for a nil-nil, would turn the commonest correct answer
// into either an alarm or a nil dereference.
func TestGetProcessNodeByCoreNodeName_MissStaysErrNoRows(t *testing.T) {
	db := testDB(t)
	twoProcessesOneSlot(t, db, "SHARED_SLOT")

	_, err := db.GetProcessNodeByCoreNodeName("A_SLOT_THIS_EDGE_DOES_NOT_OWN")
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss returned %v, want sql.ErrNoRows — the projection and the delivered fallback "+
			"both branch on it and both treat it as the ordinary 'not ours' answer", err)
	}
}

// TestGetProcessNodeByCoreNodeName_PrefersTheLiveRow closes the retired-row
// arm: a position removed and re-added under the same Core name leaves a
// tombstone and a live row, and the lookup answered the TOMBSTONE — the
// lowest-id rule was applied across both classes together. Every caller turns
// the name into a row to act on the live position, so an adjustment landed on
// a slot nobody can see, while the live row's tile read a count that never
// moved.
//
// The preference is per-class, not global: within live rows and within
// retired rows the lowest id still wins (the stability pin above keeps its
// meaning), and a name held ONLY by retired rows still resolves to one of
// them — the callers' fallback behaviour is unchanged for names whose live
// position is gone.
func TestGetProcessNodeByCoreNodeName_PrefersTheLiveRow(t *testing.T) {
	db := testDB(t)
	firstID, _ := twoProcessesOneSlot(t, db, "SHARED_SLOT")

	// Retire BOTH rows carrying the name, then re-add one in the first
	// process: the tombstones stay (soft delete is a foreign-key decision —
	// see processes.DeleteNode), and the new live row has the HIGHEST id of
	// the three. The old answer, lowest id over all rows, is the tombstone.
	if err := db.DeleteProcessNode(firstID); err != nil {
		t.Fatalf("retire first: %v", err)
	}
	rows, err := db.Query(`SELECT id FROM process_nodes WHERE core_node_name='SHARED_SLOT' AND deleted_at IS NULL`)
	if err != nil {
		t.Fatalf("list live: %v", err)
	}
	var live []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		live = append(live, id)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close rows: %v", err)
	}
	for _, id := range live {
		if err := db.DeleteProcessNode(id); err != nil {
			t.Fatalf("retire %d: %v", id, err)
		}
	}

	var processID int64
	if err := db.QueryRow(`SELECT process_id FROM process_nodes WHERE id=?`, firstID).Scan(&processID); err != nil {
		t.Fatalf("read process id: %v", err)
	}
	_, err = db.CreateProcessNode(processes.NodeInput{
		ProcessID: processID, CoreNodeName: "SHARED_SLOT", Name: "SHARED_SLOT", Sequence: 9, Enabled: true,
	})
	if err != nil {
		t.Fatalf("re-add under the same name: %v", err)
	}

	n, err := db.GetProcessNodeByCoreNodeName("SHARED_SLOT")
	if err != nil || n == nil {
		t.Fatalf("lookup after re-add: node=%v err=%v", n, err)
	}
	if n.DeletedAt != nil {
		t.Fatalf("lookup answered the retired row (id %d, deleted_at set) — the position was removed and "+
			"re-added under the same Core name, and the resolution must follow the live row, not the tombstone", n.ID)
	}
}
