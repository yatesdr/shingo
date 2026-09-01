//go:build docker

package bins_test

import (
	"database/sql"
	"errors"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/bins"
	"shingocore/store/nodes"
	"shingocore/store/reservations"
)

// cell_position_empty_docker_test.go — a carrier standing on a cell's own
// position belongs to that cell, and the plant-wide empty scan may not take it.
//
// ── THE DEADLOCK THIS PREVENTS ────────────────────────────────────────────
//
// A sequential A/B press keeps a fresh EMPTY on its parked side, ready for the
// next flip. That carrier is unclaimed, unlocked, unstaged, carries no payload
// and stands at an enabled physical node — so it matched every clause of
// EmptyCarrierWhere and the plant-wide scan harvested it.
//
// MEASURED, demo.yaml 2026-08-30:
//
//	order 144  retrieve_empty  PLN_004 -> PEB_003   confirmed
//
// PLN_004 is PRESS-2's parked side. Nothing ever delivered to it again — the
// sequential backfill targets the ACTIVE side's claim — so the cell could never
// flip, and it deadlocked the first time the active side filled:
//
//	PLN_004 empty  ->  "A/B cutover rejected: PLN_004 has no bin on it"
//	no flip        ->  "the line is pulling from PLN_003; flip to PLN_004 first"
//	no evac        ->  PLN_003 stays full, its robot pinned
//	no place       ->  "HOLDING at PLN_003", a second robot pinned
//
// Three robots, and every order downstream of PANEL-B starved behind them.
//
// ── WHAT IS AND IS NOT EXCLUDED ───────────────────────────────────────────
//
// The rule keys on style_claims.core_node_name, so it covers exactly the
// positions a cell has claimed — a press side, a weld consume point — and
// nothing else. An ordinary storage slot, a loader window, a staging node and an
// empties-bank position are all untouched, which is what the second half of this
// test asserts: an exclusion that swallowed the empties bank would starve every
// producer instead of one press.
func TestEmptyScan_SkipsACellsOwnPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	// The press's parked side, and an ordinary bank position beside it.
	press := &nodes.Node{Name: "CELLPOS-PRESS-B", Enabled: true}
	if err := db.CreateNode(press); err != nil {
		t.Fatalf("create the press position: %v", err)
	}
	bank := &nodes.Node{Name: "CELLPOS-BANK-1", Enabled: true}
	if err := db.CreateNode(bank); err != nil {
		t.Fatalf("create the bank position: %v", err)
	}

	// ONLY the press position carries a claim. This is the whole discriminator.
	if _, err := db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, role, swap_mode, payload_code, allowed_payload_codes, uop_capacity, reorder_point, seq)
		VALUES ($1,$2,$3,'produce','sequential','', '[]', 0, 0, 0)`,
		"CELLPOS-PROC", "CELLPOS-STYLE", press.Name); err != nil {
		t.Fatalf("seed the press claim: %v", err)
	}

	// An identical empty carrier on each.
	parked := testdb.CreateBinAtNode(t, db, "", press.ID, "BIN-CELLPOS-PARKED")
	banked := testdb.CreateBinAtNode(t, db, "", bank.ID, "BIN-CELLPOS-BANK")

	found := map[int64]bool{}
	for i := 0; i < 4; i++ {
		// Take repeatedly: the scan returns one carrier, so a single call could
		// miss the parked one by luck of the ordering rather than by the rule.
		// A drained pool answers with sql.ErrNoRows rather than a nil bin — see
		// none_found_contract_test.go — so both shapes end the walk.
		b, err := db.FindEmptyCompatibleBin("", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
		if errors.Is(err, sql.ErrNoRows) || b == nil {
			break
		}
		if err != nil {
			t.Fatalf("plant-wide empty scan: %v", err)
		}
		found[b.ID] = true
		// Claim it so the next call moves on.
		if _, err := db.Exec(`UPDATE bins SET locked=true WHERE id=$1`, b.ID); err != nil {
			t.Fatalf("take the carrier out of the pool: %v", err)
		}
	}

	if found[parked.ID] {
		t.Errorf("the plant-wide empty scan took the carrier parked on %s, which is a CLAIMED cell "+
			"position. That carrier is the cell's working stock — the bin its next A/B flip needs — "+
			"and harvesting it deadlocks the press: no bin on the parked side, so no flip; no flip, "+
			"so no evac; no evac, so the refill cannot place. Three robots and everything downstream",
			press.Name)
	}
	if !found[banked.ID] {
		t.Errorf("the scan did NOT take the carrier at %s, which carries no claim and is exactly what "+
			"the empty pool is for. An exclusion this broad starves every producer instead of "+
			"protecting one press", bank.Name)
	}
}
