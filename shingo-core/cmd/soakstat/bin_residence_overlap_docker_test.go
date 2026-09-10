//go:build docker

package main

import (
	"fmt"
	"testing"
	"time"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/bins"
)

// ── THE RESIDENCE THIS ASSERTS ON IS ITS OWN ────────────────────────────────
//
// Same isolation rule as the carrier-pool fixtures: these tests share a
// database and run in parallel, and the overlap check is a question about
// EVERY node. Each case seeds its own node name and asserts only on the
// line naming it.
//
// ── WHAT THE FIXTURES SEED, AND WHY IT CHANGED ─────────────────────────────
//
// They used to seed one ORDER per event and one order_history row per order,
// because the check read a step's time off its order's `delivered` stamp. That
// fixture shape could not fail: with one step per order the two are the same
// instant, so the check looked right on every test and was wrong on every real
// swap, where one order does several things hours apart.
//
// The fixtures now seed the PER-BLOCK LEDGER — mission_events rows carrying
// BLOCK_FINISHED, which is what the engine writes when the fleet reports a
// block finished (engine.recordBlockLeg). That is where an executed step's time
// actually lives.

// mustOrder mints a real order. `binID` is orders.bin_id — the order's primary
// bin, used only as the LABEL on a violation line; the check's decision never
// depends on it (see checkBinResidenceOverlap's own note on why).
func mustOrder(t *testing.T, db *store.DB, label, deliveryNode, stepsJSON string, binID int64) *domain.Order {
	t.Helper()
	o := &domain.Order{
		EdgeUUID:     "soak-res-" + label,
		StationID:    "soak",
		OrderType:    protocol.OrderTypeComplex,
		Status:       protocol.StatusDelivered,
		Quantity:     1,
		StepsJSON:    stepsJSON,
		BinID:        &binID,
		DeliveryNode: deliveryNode,
		PayloadCode:  "PART-R",
	}
	if err := db.CreateOrder(o); err != nil {
		t.Fatalf("create order %s: %v", label, err)
	}
	return o
}

// mustDelivered writes the order_history row the FINAL delivery arm reads.
// The sim driver does not emit a block event for an order's last block — it is
// represented by the order reaching FINISHED — so the final dropoff's arrival
// has no ledger row and comes from here.
func mustDelivered(t *testing.T, db *store.DB, o *domain.Order, at time.Time) {
	t.Helper()
	if _, err := db.DB.Exec(
		`INSERT INTO order_history (order_id, status, detail, created_at)
		 VALUES ($1, 'delivered', 'residence fixture', $2)`, o.ID, at); err != nil {
		t.Fatalf("history row for order %d: %v", o.ID, err)
	}
}

// mustBlock writes one executed-block row: what the engine records when the
// fleet reports a block finished. binTask is the vendor's vocabulary, matched
// by engine.IsPickupBlock / engine.IsDropoffBlock.
func mustBlock(t *testing.T, db *store.DB, o *domain.Order, node, binTask string, at time.Time) {
	t.Helper()
	blocks := fmt.Sprintf(
		`[{"blockId":"b-%d-%s","location":%q,"binTask":%q,"startTime":%d,"terminateTime":%d,"durationSeconds":1}]`,
		o.ID, binTask, node, binTask, at.Unix()-1, at.Unix())
	// RAW INSERT, not telemetry.InsertEvent: that writer stamps created_at from
	// clock.Now() and ignores the field, which is right for production (the sim's
	// fast-forward clock must order these against order_history) and useless to a
	// fixture that needs a specific instant.
	if _, err := db.DB.Exec(
		`INSERT INTO mission_events (order_id, vendor_order_id, old_state, new_state, blocks_json, detail, created_at)
		 VALUES ($1, '', '', 'BLOCK_FINISHED', $2, $3, $4)`,
		o.ID, blocks, fmt.Sprintf("block @ %s (binTask=%s)", node, binTask), at); err != nil {
		t.Fatalf("block row for order %d: %v", o.ID, err)
	}
}

func dropoffStep(node string) string {
	return fmt.Sprintf(`[{"action":"dropoff","node":%q}]`, node)
}

func seedBins(t *testing.T, db *store.DB, label string, ids ...int64) {
	t.Helper()
	bt := &bins.BinType{Code: fmt.Sprintf("SOAKRES-%s", label), Description: "residence fixture"}
	if err := db.CreateBinType(bt); err != nil {
		t.Fatalf("create bin type: %v", err)
	}
	for _, b := range ids {
		if _, err := db.DB.Exec(
			`INSERT INTO bins (id, bin_type_id, label, status) VALUES ($1, $2, $3, 'available')`,
			b, bt.ID, fmt.Sprintf("SOAK-RES-%s-%d", label, b)); err != nil {
			t.Fatalf("seed bin %d: %v", b, err)
		}
	}
}

// TestBinResidenceOverlap_SingleRobotSwapIsNotAnOverlap is THE REGRESSION, and
// it is the reason the check was rewritten.
//
// ── THE FALSE ALARM, AND THE MECHANISM ─────────────────────────────────────
//
// A single_robot consume swap is ONE order that lifts the resident and places
// the replacement: pickup(LINE) then dropoff(LINE). The old check stamped BOTH
// of those at the order's `delivered` time and attributed the lift to
// orders.bin_id — the bin the order PLACED, not the one it lifted. So the
// previous resident's arrival never found a departure, the check read it as
// "never lifted (still standing)", and the next carrier's arrival tripped it.
//
// Every "two bins at one node" row this check has ever produced was that:
// run3's 9128s, 470s and 310s, run2's 8971s, 5761s, 874s and 522s. A reviewer
// then showed the event is physically impossible anyway — CanEnterPosition
// refuses a second bin at an occupied position — so the instrument was
// generating alarms for a shape the plant cannot produce, in a check U3
// certifies with.
//
// The fixture is run3's ALN_003 carousel in miniature: consecutive swaps
// alternate carriers 18 → 19 → 18, and you cannot place 19 unless the swap
// before it lifted 18 first.
func TestBinResidenceOverlap_SingleRobotSwapIsNotAnOverlap(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := "SOAKPOS-SR"
	seedBins(t, db, "SR", 9110, 9111)
	now := time.Now().UTC()

	// The seeding delivery: carrier 9110 is placed on the line.
	seed := mustOrder(t, db, "SR-seed", node, dropoffStep(node), 9110)
	mustDelivered(t, db, seed, now.Add(-30*time.Minute))

	// The swap: ONE order, lifts 9110 and places 9111. Its line-pickup block
	// executes at -20m; the order does not reach `delivered` until -2m, because
	// it still has to carry 9110 out to the market afterwards. THAT GAP is what
	// the old check mistook for two bins standing together.
	swap := mustOrder(t, db, "SR-swap", node,
		fmt.Sprintf(`[{"action":"pickup","node":%q},{"action":"dropoff","node":%q}]`, node, node), 9111)
	mustBlock(t, db, swap, node, "Load", now.Add(-20*time.Minute))   // lifts 9110
	mustBlock(t, db, swap, node, "Unload", now.Add(-19*time.Minute)) // places 9111
	mustDelivered(t, db, swap, now.Add(-2*time.Minute))

	if got := checkBinResidenceOverlap(db); hasLine(got, "node "+node) {
		t.Errorf("a single_robot swap at %s was reported as two bins at one node.\n"+
			"The lift and the placement are ONE order minutes apart; reading both off the order's "+
			"`delivered` stamp is what produced every overlap row this check has ever emitted, "+
			"and U3 certifies with this instrument.\ngot: %v", node, got)
	}
}

// TestBinResidenceOverlap_PlacingOntoAnOccupiedPosition is the shape the check
// exists for, proven real by sim 2026-09-07: the press-index index leg placed
// bin 30 on PLN_001 while bin 17 still stood there. The lane invariants were
// silent — corridors only — and twenty seconds of two bins at one position
// passed every existing assertion.
//
// Two DIFFERENT orders, and the second places before the first has lifted.
func TestBinResidenceOverlap_PlacingOntoAnOccupiedPosition(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := "SOAKPOS-OVR"
	seedBins(t, db, "OVR", 9101, 9102)
	now := time.Now().UTC()

	resident := mustOrder(t, db, "OVR-arrA", node, dropoffStep(node), 9101)
	mustDelivered(t, db, resident, now.Add(-10*time.Minute))

	// The clearer is dispatched but has NOT lifted — no pickup block row.
	// The filler places anyway.
	filler := mustOrder(t, db, "OVR-arrB", node, dropoffStep(node), 9102)
	mustDelivered(t, db, filler, now.Add(-5*time.Minute))

	got := checkBinResidenceOverlap(db)
	if !hasLine(got, "node "+node) {
		t.Errorf("two overlapping bin residences at %s did not trip the check.\n"+
			"This is the collision the lane checks cannot see: a placing leg that set a bin "+
			"down on an occupied position.\ngot: %v", node, got)
	}
	if !hasLine(got, "bin 9102") {
		t.Errorf("the violation must name the arriving bin — the reader's next question is "+
			"WHICH robot placed it.\ngot: %v", got)
	}
}

// TestBinResidenceOverlap_QuietWhenSequential keeps the check from firing on a
// healthy plant: bins handed over node-by-node, each lifting before the next
// lands, are the ordinary choreography of every swap ever run.
//
// The lift is a BLOCK row now, which is the whole point — the lifting order's
// own completion is irrelevant to when the node became free.
func TestBinResidenceOverlap_QuietWhenSequential(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := "SOAKPOS-SEQ"
	seedBins(t, db, "SEQ", 9101, 9102)
	now := time.Now().UTC()

	resident := mustOrder(t, db, "SEQ-arrA", node, dropoffStep(node), 9101)
	mustDelivered(t, db, resident, now.Add(-10*time.Minute))

	// A separate evac lifts it at -6m and does not complete until much later.
	evac := mustOrder(t, db, "SEQ-lift", "SOAKOUT-SEQ",
		fmt.Sprintf(`[{"action":"pickup","node":%q},{"action":"dropoff","node":"SOAKOUT-SEQ"}]`, node), 9101)
	mustBlock(t, db, evac, node, "Load", now.Add(-6*time.Minute))
	mustDelivered(t, db, evac, now.Add(-1*time.Minute)) // long after the lift

	filler := mustOrder(t, db, "SEQ-arrB", node, dropoffStep(node), 9102)
	mustDelivered(t, db, filler, now.Add(-5*time.Minute))

	if got := checkBinResidenceOverlap(db); hasLine(got, "node "+node) {
		t.Errorf("a clean handover at %s was reported as an overlap. A check that fires on "+
			"ordinary swaps teaches the reader to skip its violations.\ngot: %v", node, got)
	}
}

// TestBinResidenceOverlap_QuietOnARevisitingCarousel pins the false positive
// that broke the check's first construction, measured on the 2026-09-07 re-run:
// bin 25 cycled ALN_006 every ~2 minutes and an earliest-arrival /
// earliest-departure collapse paired the LAST visit's arrival with the FIRST
// visit's lift, reading a healthy carousel as "still standing since forever".
//
// The occupancy sweep cannot regress this way — it never pairs by bin — but the
// carousel stays as a fixture because it is the traffic the plant actually runs.
func TestBinResidenceOverlap_QuietOnARevisitingCarousel(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := "SOAKPOS-CAR"
	seedBins(t, db, "CAR", 9101, 9102)
	now := time.Now().UTC()

	for i := 0; i < 3; i++ {
		base := now.Add(-time.Duration(30-i*8) * time.Minute)
		arr := mustOrder(t, db, fmt.Sprintf("CAR-arr-%d", i), node, dropoffStep(node), 9101)
		mustDelivered(t, db, arr, base)
		// A DISTINCT outbound node per lift. One shared outbound would take
		// three carriers and never lift them, which is a genuine overlap the
		// check would be right to report — and the fixture would be reporting on
		// itself. (It also must not share a prefix with `node`: hasLine is a
		// substring match, so "node SOAKPOS-CAR" matches "node SOAKPOS-CAR-OUT".)
		out := fmt.Sprintf("SOAKOUT-CAR-%d", i)
		lift := mustOrder(t, db, fmt.Sprintf("CAR-lift-%d", i), out,
			fmt.Sprintf(`[{"action":"pickup","node":%q},{"action":"dropoff","node":%q}]`, node, out), 9101)
		mustBlock(t, db, lift, node, "Load", base.Add(3*time.Minute))
		mustDelivered(t, db, lift, base.Add(4*time.Minute))
	}
	last := mustOrder(t, db, "CAR-arrB", node, dropoffStep(node), 9102)
	mustDelivered(t, db, last, now.Add(-2*time.Minute))

	if got := checkBinResidenceOverlap(db); hasLine(got, "node "+node) {
		t.Errorf("a bin cleanly revisiting %s was reported as an overlap — the visiting-unit "+
			"regression (first-visit lift paired against last-visit arrival).\ngot: %v", node, got)
	}
}

// TestBinResidenceOverlap_QuietWhenTheRunOpensWithALift pins the seed-resident
// case: a node that was already occupied when the run began has no arrival on
// record, so its lift must clear the position rather than being read as an
// event with no meaning. Erring the other way would flag the first honest
// placement after every restart.
func TestBinResidenceOverlap_QuietWhenTheRunOpensWithALift(t *testing.T) {
	t.Parallel()
	db := testdb.Open(t)
	node := "SOAKPOS-SEED"
	seedBins(t, db, "SEED", 9101, 9102)
	now := time.Now().UTC()

	// A lift with no arrival on record — the pre-run resident leaving.
	evac := mustOrder(t, db, "SEED-lift", "SOAKOUT-SEED",
		fmt.Sprintf(`[{"action":"pickup","node":%q},{"action":"dropoff","node":"SOAKOUT-SEED"}]`, node), 9101)
	mustBlock(t, db, evac, node, "Load", now.Add(-10*time.Minute))
	mustDelivered(t, db, evac, now.Add(-9*time.Minute))

	filler := mustOrder(t, db, "SEED-arr", node, dropoffStep(node), 9102)
	mustDelivered(t, db, filler, now.Add(-8*time.Minute))

	if got := checkBinResidenceOverlap(db); hasLine(got, "node "+node) {
		t.Errorf("the first placement after a pre-run resident left %s was reported as an "+
			"overlap.\ngot: %v", node, got)
	}
}
