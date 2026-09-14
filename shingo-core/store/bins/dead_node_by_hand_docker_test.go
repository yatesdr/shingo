//go:build docker

package bins_test

import (
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
)

// dead_node_by_hand_docker_test.go — a disabled node is dead to automation and
// open to an engineer, and both halves of that are load-bearing.
//
// The 2026-09-14 ruling: "switched off = hard dead. an engineer should be able
// to manually manipulate a bin there but nothing else." Every automated reader
// got bins.NodeEnabledSQL for the first half. The second half was already true
// and had nothing holding it that way — the by-hand move is a bare UPDATE with
// no node predicate, which is correct and is exactly the kind of correctness
// that gets "tidied" into a guard by someone making the readers consistent.
//
// So the door is pinned open, next to the readers being pinned shut.
//
// VERIFIED RED BY: dropping bins.NodeEnabledSQL out of BinAtLiveNodeSQL — the
// automated half fired, naming FindSourceFIFO. The by-hand half fires if a
// node check is ever added to bins.move.
func TestDeadNode_ClosedToAutomation_OpenToAnEngineer(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	std := testdb.SetupStandardData(t, db)

	dead := &nodes.Node{Name: "DEAD-NODE-1", Enabled: false, Zone: "A"}
	if err := nodes.Create(db.DB, dead); err != nil {
		t.Fatalf("create disabled node: %v", err)
	}
	live := &nodes.Node{Name: "LIVE-NODE-1", Enabled: true, Zone: "A"}
	if err := nodes.Create(db.DB, live); err != nil {
		t.Fatalf("create enabled node: %v", err)
	}

	// A bin that is perfect in every way except where it is standing.
	var binID int64
	if err := db.DB.QueryRow(`
		INSERT INTO bins (bin_type_id, label, node_id, status, payload_code,
		                  uop_remaining, manifest_confirmed, locked)
		VALUES ($1,'DEAD-NODE-BIN',$2,'available',$3,100,true,false) RETURNING id`,
		std.BinType.ID, dead.ID, std.Payload.Code).Scan(&binID); err != nil {
		t.Fatalf("create bin on the disabled node: %v", err)
	}

	// ── CLOSED TO AUTOMATION ────────────────────────────────────────────────
	if got, err := bins.FindSourceFIFO(db.DB, std.Payload.Code, 0); err == nil && got != nil && got.ID == binID {
		t.Errorf("FindSourceFIFO sourced a bin standing on a DISABLED node — "+
			"a switched-off node is dead to automation, and a robot must not be "+
			"sent to one (bin %d at %q)", binID, dead.Name)
	}

	// ── OPEN TO AN ENGINEER ─────────────────────────────────────────────────
	// The by-hand door: an engineer moving the bin off the dead node by hand.
	// This must keep working, or a bin on a node somebody switched off is
	// stranded with no way back that does not involve SQL.
	if err := bins.MoveAndClearStaging(db.DB, binID, live.ID, false); err != nil {
		t.Fatalf("an engineer could not move a bin OFF a disabled node: %v — "+
			"the by-hand door is the only way a bin leaves a dead node, and "+
			"closing it strands the bin", err)
	}
	var landed int64
	if err := db.DB.QueryRow(`SELECT node_id FROM bins WHERE id=$1`, binID).Scan(&landed); err != nil {
		t.Fatalf("read bin node after the move: %v", err)
	}
	if landed != live.ID {
		t.Errorf("bin landed at node %d, want %d", landed, live.ID)
	}

	// And the reverse direction: an engineer may deliberately put a bin ONTO a
	// dead node (staging a repair, quarantining a lane). Automation still will
	// not pick it up — the check above already proved that.
	if err := bins.MoveAndClearStaging(db.DB, binID, dead.ID, false); err != nil {
		t.Fatalf("an engineer could not move a bin ONTO a disabled node: %v", err)
	}
}
