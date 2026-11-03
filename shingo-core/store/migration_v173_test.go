//go:build docker

package store_test

import (
	"bytes"
	"log"
	"os"
	"strconv"
	"strings"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store"
)

// ageOrderCartsSchema puts order_bins and orders back in their pre-v173 shape:
// orders.bin_id and the junction's node copies (v174's drop undone), step_index
// (the pick), no drop_step, one row per (order, bin), the plain by-bin index.
// It is also the v174+v173 rollback DDL, run after the previous binary is back.
const ageOrderCartsSchema = `
ALTER TABLE orders ADD COLUMN IF NOT EXISTS bin_id BIGINT REFERENCES bins(id);
ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS action TEXT NOT NULL DEFAULT 'pickup';
ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS node_name TEXT NOT NULL DEFAULT '';
ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS dest_node TEXT NOT NULL DEFAULT '';
ALTER TABLE order_bins DROP CONSTRAINT IF EXISTS order_bins_steps;
DROP INDEX IF EXISTS order_bins_pick;
DROP INDEX IF EXISTS order_bins_drop;
DROP INDEX IF EXISTS order_bins_by_bin;
DELETE FROM order_bins WHERE pick_step IS NULL;
ALTER TABLE order_bins DROP COLUMN drop_step;
ALTER TABLE order_bins ALTER COLUMN pick_step SET NOT NULL;
ALTER TABLE order_bins RENAME COLUMN pick_step TO step_index;
CREATE UNIQUE INDEX order_bins_order_bin_uniq ON order_bins (order_id, bin_id);
CREATE INDEX idx_order_bins_bin ON order_bins (bin_id);
`

// TestV173_AnOrdersCartsBecomeRowsPerLift is v173 on a fixture: a live swap with
// A in flight and one with A set down get their drops; a live plain order gets
// its implied lift; a live order whose cart's pickup is ambiguous, a stale row
// and the books' disagreements are listed and left; an ended order is carried
// over with no steps; a second run changes nothing.
//
// Not parallel: it reads the migration's census lines from the process logger.
func TestV173_AnOrdersCartsBecomeRowsPerLift(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	sd := testdb.SetupStandardData(t, db)
	if _, err := db.Exec(ageOrderCartsSchema); err != nil {
		t.Fatalf("age order_bins to the pre-v173 shape: %v", err)
	}
	bin := func(label string) int64 {
		return testdb.CreateBinAtNode(t, db, "DEFAULT", sd.StorageNode.ID, label).ID
	}
	// Orders are written as a pre-v173 Core wrote them: the row alone (the store's
	// writer now writes a born-held cart's rows, in a shape the aged table lacks).
	order := func(uuid, status, steps string, binID int64) int64 {
		var bin any
		if binID != 0 {
			bin = binID
		}
		var id int64
		if err := db.QueryRow(`INSERT INTO orders (edge_uuid, station_id, order_type, status, quantity,
			source_node, delivery_node, steps_json, bin_id, vendor_order_id)
			VALUES ($1, 'line-1', 'complex', $2, 1, 'P', 'P', $3, $4, $5) RETURNING id`,
			uuid, status, steps, bin, "v-"+uuid).Scan(&id); err != nil {
			t.Fatalf("order %s: %v", uuid, err)
		}
		return id
	}
	oldRow := func(orderID, binID int64, pick int, dest string) {
		if _, err := db.Exec(`INSERT INTO order_bins (order_id, bin_id, step_index, action, node_name, dest_node)
			VALUES ($1, $2, $3, 'pickup', '', $4)`, orderID, binID, pick, dest); err != nil {
			t.Fatalf("old row order %d bin %d: %v", orderID, binID, err)
		}
	}
	claim := func(binID, orderID int64, node string) {
		if _, err := db.Exec(`UPDATE bins SET claimed_by=$2, node_id=(SELECT id FROM nodes WHERE name=$3) WHERE id=$1`,
			binID, orderID, node); err != nil {
			t.Fatalf("claim bin %d: %v", binID, err)
		}
	}
	swap := `[{"action":"wait","node":"P"},{"action":"pickup","node":"P"},{"action":"dropoff","node":"W"},` +
		`{"action":"pickup","node":"S"},{"action":"dropoff","node":"P"}]`

	// A live swap, A on the robot: rows for A (pick 1) and B (pick 3).
	inFlightA, inFlightB := bin("V173-IF-A"), bin("V173-IF-B")
	inFlight := order("v173-in-flight", "in_transit", swap, inFlightA)
	oldRow(inFlight, inFlightA, 1, "W")
	oldRow(inFlight, inFlightB, 3, "P")
	claim(inFlightA, inFlight, "_TRANSIT")
	claim(inFlightB, inFlight, sd.StorageNode.Name)

	// A live swap, A set down at W (claim kept), B on the robot; and its dest_node
	// for B drifted.
	downA, downB := bin("V173-DN-A"), bin("V173-DN-B")
	down := order("v173-down", "in_transit", swap, downA)
	oldRow(down, downA, 1, "W")
	oldRow(down, downB, 3, "SOMEWHERE")
	claim(downA, down, sd.StorageNode.Name)
	claim(downB, down, "_TRANSIT")

	// A live plain order holding its cart: the implied lift.
	plainBin := bin("V173-PLAIN")
	plain := order("v173-plain", "dispatched", "", plainBin)
	claim(plainBin, plain, sd.StorageNode.Name)

	// A live single-cart order whose plan has two non-relay pickups: ambiguous.
	ambBin := bin("V173-AMB")
	amb := order("v173-ambiguous", "dispatched", swap, ambBin)
	claim(ambBin, amb, sd.StorageNode.Name)

	// A live order with a row whose pick is not a pickup in its plan (a stale
	// allocation, waiting to re-allocate: nothing held): kept, listed. And an open lift beside it, a dwelling leg's shape:
	// the plan ends with its cart in hand.
	staleBin := bin("V173-STALE")
	stale := order("v173-stale", "queued",
		`[{"action":"pickup","node":"S"},{"action":"dropoff","node":"W"},{"action":"pickup","node":"Q"}]`, 0)
	oldRow(stale, staleBin, 1, "")
	openBin := bin("V173-OPEN")
	oldRow(stale, openBin, 2, "")

	// Two carts claimed on _TRANSIT by one live order, and a claim with no row.
	twoA, twoB := bin("V173-TWO-A"), bin("V173-TWO-B")
	two := order("v173-two", "in_transit", "", twoA)
	claim(twoA, two, "_TRANSIT")
	claim(twoB, two, "_TRANSIT")

	// An ended order with a bin_id and no rows: carried over with no steps.
	endedBin := bin("V173-ENDED")
	ended := order("v173-ended", "confirmed", "", endedBin)

	var logged bytes.Buffer
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// v174 drops orders.bin_id after each run; the second run is v173 again on
	// the shape it reads, so the column and its values come back first (the
	// rollback DDL's first half), and v174 runs again after it.
	if _, err := db.Exec(`CREATE TABLE v173_test_bin_ids AS SELECT id, bin_id FROM orders`); err != nil {
		t.Fatalf("keep the bin_id values: %v", err)
	}
	for run := 1; run <= 2; run++ {
		if _, err := db.Exec(`ALTER TABLE orders ADD COLUMN IF NOT EXISTS bin_id BIGINT;
			UPDATE orders o SET bin_id = k.bin_id FROM v173_test_bin_ids k WHERE k.id = o.id;
			ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS action TEXT NOT NULL DEFAULT 'pickup';
			ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS node_name TEXT NOT NULL DEFAULT '';
			ALTER TABLE order_bins ADD COLUMN IF NOT EXISTS dest_node TEXT NOT NULL DEFAULT '';
			DELETE FROM schema_migrations WHERE version = 173`); err != nil {
			t.Fatalf("run %d: v174 undone and v173 forgotten: %v", run, err)
		}
		migrated, err := store.Open(cfg)
		if err != nil {
			t.Fatalf("run %d: v173: %v", run, err)
		}
		migrated.Close()
	}
	log.SetOutput(os.Stderr)

	type lift struct {
		bin        int64
		pick, drop int
	}
	rows := func(orderID int64) []lift {
		t.Helper()
		r, err := db.Query(`SELECT bin_id, COALESCE(pick_step, -1), COALESCE(drop_step, -1) FROM order_bins
			WHERE order_id = $1 ORDER BY pick_step NULLS FIRST, id`, orderID)
		if err != nil {
			t.Fatalf("rows of %d: %v", orderID, err)
		}
		defer r.Close()
		var out []lift
		for r.Next() {
			var l lift
			if err := r.Scan(&l.bin, &l.pick, &l.drop); err != nil {
				t.Fatalf("scan: %v", err)
			}
			out = append(out, l)
		}
		return out
	}
	for _, c := range []struct {
		name  string
		order int64
		want  []lift
	}{
		{"a swap with A in flight", inFlight, []lift{{inFlightA, 1, 2}, {inFlightB, 3, 4}}},
		{"a swap with A set down", down, []lift{{downA, 1, 2}, {downB, 3, 4}}},
		{"a plain order", plain, []lift{{plainBin, 0, 1}}},
		{"an ambiguous order", amb, nil},
		{"a stale row and an open lift", stale, []lift{{staleBin, 1, -1}, {openBin, 2, -1}}},
		{"an ended order", ended, []lift{{endedBin, -1, -1}}},
	} {
		got := rows(c.order)
		if len(got) != len(c.want) {
			t.Errorf("%s: rows %v, want %v", c.name, got, c.want)
			continue
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Errorf("%s: rows %v, want %v", c.name, got, c.want)
				break
			}
		}
	}

	out := logged.String()
	for _, line := range []string{
		"v173 list live_pick_step_ambiguous: order " + strconv.FormatInt(amb, 10),
		"v173 list live_row_without_drop: order " + strconv.FormatInt(stale, 10),
		"v173 list live_claim_without_row: order " + strconv.FormatInt(two, 10),
		"v173 list live_row_without_claim: order " + strconv.FormatInt(stale, 10),
		"v173 list live_two_in_hand: order " + strconv.FormatInt(two, 10),
		"v173 list plan_final_drop_disagrees: order " + strconv.FormatInt(stale, 10),
		"v173 list dest_node_drift: order " + strconv.FormatInt(down, 10),
		"ended_rows_carried_over=1",
	} {
		if !strings.Contains(out, line) {
			t.Errorf("the census did not say %q; it said:\n%s", line, out)
		}
	}
}
