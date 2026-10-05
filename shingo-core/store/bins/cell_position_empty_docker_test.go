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
// This arm keys on a claim's core_node_name and its paired positions
// (paired_core_node, second_paired_core_node), so it covers exactly the
// positions a cell has claimed — a press side, a press-index position, a weld
// consume point — and nothing else. Loader windows and homes are excluded by a separate arm over
// bin_loader_homes, because style_claims never holds a loader (see
// loader_position_empty_docker_test.go). An ordinary storage slot, a staging
// node and an empties-bank position are untouched by both, which is what the
// second half of this test asserts: an exclusion that swallowed the empties bank
// would starve every producer instead of one press.
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

// A press-index cell claims its head by core_node_name and its index positions
// only by paired_core_node and second_paired_core_node. The empty on an index
// position is the carrier the swap's index leg lifts to finish the cycle, so it
// is the cell's working stock exactly as the parked side of a sequential press
// is.
//
// MEASURED, polish-6 sim, 2026-10-04: a press-index swap pair was built while
// PRESS-1's paired position held its empty. While the pair waited for its
// keep-staged spare, the empties group's level keeper topped up its pool with
// that carrier ("order 23 fulfilled — bin 17 (PLN_002 -> PEB_001)"). The index
// leg could then never reserve, nothing re-plans a parked pair, and the press
// held until a changeover cancelled the swap.
func TestEmptyScan_SkipsAPressCellsPairedPositions(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)

	head := &nodes.Node{Name: "CELLPOS-PI-HEAD", Enabled: true}
	deck := &nodes.Node{Name: "CELLPOS-PI-DECK", Enabled: true}
	back := &nodes.Node{Name: "CELLPOS-PI-BACK", Enabled: true}
	bank := &nodes.Node{Name: "CELLPOS-PI-BANK", Enabled: true}
	for _, n := range []*nodes.Node{head, deck, back, bank} {
		if err := db.CreateNode(n); err != nil {
			t.Fatalf("create %s: %v", n.Name, err)
		}
	}

	if _, err := db.Exec(`INSERT INTO style_claims
		(process_id, style_id, core_node_name, paired_core_node, second_paired_core_node, role, swap_mode,
		 payload_code, allowed_payload_codes, uop_capacity, reorder_point, seq)
		VALUES ($1,$2,$3,$4,$5,'produce','two_robot_press_index','', '[]', 0, 0, 0)`,
		"CELLPOS-PI-PROC", "CELLPOS-PI-STYLE", head.Name, deck.Name, back.Name); err != nil {
		t.Fatalf("seed the press-index claim: %v", err)
	}

	onDeck := testdb.CreateBinAtNode(t, db, "", deck.ID, "BIN-CELLPOS-PI-DECK")
	onBack := testdb.CreateBinAtNode(t, db, "", back.ID, "BIN-CELLPOS-PI-BACK")
	banked := testdb.CreateBinAtNode(t, db, "", bank.ID, "BIN-CELLPOS-PI-BANK")

	found := map[int64]bool{}
	for i := 0; i < 5; i++ {
		b, err := db.FindEmptyCompatibleBin("", "", 0, bins.EmptyFence{}, reservations.DigAsker{})
		if errors.Is(err, sql.ErrNoRows) || b == nil {
			break
		}
		if err != nil {
			t.Fatalf("plant-wide empty scan: %v", err)
		}
		found[b.ID] = true
		if _, err := db.Exec(`UPDATE bins SET locked=true WHERE id=$1`, b.ID); err != nil {
			t.Fatalf("take the carrier out of the pool: %v", err)
		}
	}

	for _, p := range []struct {
		bin  *bins.Bin
		node string
	}{{onDeck, deck.Name}, {onBack, back.Name}} {
		if found[p.bin.ID] {
			t.Errorf("the plant-wide empty scan took the carrier on %s, a press-index cell's paired "+
				"position. The swap's index leg lifts that carrier; harvesting it leaves the pair "+
				"unable to reserve, and a parked pair is never re-planned", p.node)
		}
	}
	if !found[banked.ID] {
		t.Errorf("the scan did NOT take the carrier at %s, which carries no claim", bank.Name)
	}
}
