package engine

import (
	"strings"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
	"shingoedge/store/processes"
)

// keep_staged_decide_test.go — THE decision for a keep-staged spot
// (decideSpot), pure, and the keeper's pause read from the rows.

func decideClaim() *processes.NodeClaim {
	return &processes.NodeClaim{CoreNodeName: "LINE", KeepStagedNode: "SPOT", InboundSource: "SRC",
		PayloadCode: "PART", Role: protocol.ClaimRoleConsume}
}

func TestDecideSpot_TheRule(t *testing.T) {
	t.Parallel()
	right := spotRead{known: true, occupied: true, payload: "PART"}
	wrong := spotRead{known: true, occupied: true, payload: "OTHER"}
	bare := spotRead{known: true}
	cases := []struct {
		name        string
		f           spotFacts
		wantRefills int
		wantReturn  bool
		wantNote    string
	}{
		{"idle, spare standing: nothing", spotFacts{read: right}, 0, false, ""},
		{"idle, bare: one", spotFacts{read: bare}, 1, false, ""},
		{"bare, one coming: nothing more, never two in flight", spotFacts{read: bare, coming: 1}, 0, false, ""},
		{"wrong spare: it stays, nothing comes, and the board says to move it", spotFacts{read: wrong}, 0, false, "Move it by hand"},
		{"an empty on a consume spot is wrong: the same", spotFacts{read: spotRead{known: true, occupied: true}}, 0, false, "Move it by hand"},
		{"a leg still drops onto the spot: nothing", spotFacts{read: wrong, drops: true}, 0, false, "still bringing a bin"},
		{"paused: nothing, and the board says so", spotFacts{read: bare, paused: "Keep-staged is paused: x"}, 0, false, "paused"},
		{"unknown spot: nothing", spotFacts{}, 0, false, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			plan, note := decideSpot(decideClaim(), c.f)
			if plan.refills != c.wantRefills || plan.returnSpare != c.wantReturn {
				t.Errorf("plan = %+v, want refills=%d return=%v", plan, c.wantRefills, c.wantReturn)
			}
			if (c.wantNote == "") != (note == "") || !strings.Contains(note, c.wantNote) {
				t.Errorf("note = %q, want one containing %q", note, c.wantNote)
			}
		})
	}
}

// What is coming counts only for the claim's part and role: a refill for the
// outgoing style does not stand in for this one's.
func TestDecideSpot_CountsOnlyThisPart(t *testing.T) {
	t.Parallel()
	c := decideClaim()
	pn := int64(1)
	rows := []domain.Order{
		{ID: 1, OrderType: protocol.OrderTypeRetrieve, Status: protocol.StatusQueued, DeliveryNode: "SPOT",
			SourceNode: "SRC", PayloadCode: "PART-OLD", ProcessNodeID: &pn},
	}
	if got := spotComing(rows, c); got != 0 {
		t.Fatalf("coming = %d with only another part's refill on its way, want 0", got)
	}
	plan, _ := decideSpot(c, spotFacts{read: spotRead{known: true}, coming: spotComing(rows, c)})
	if plan.refills != 1 {
		t.Fatalf("refills = %d, want 1: another part's refill does not fill this spot", plan.refills)
	}
}

// ANY CANCEL PAUSES, and a REQUEST, a changeover start (both create an order of
// the line) or a claim save re-arms.
func TestSpotPause_FromTheRows(t *testing.T) {
	t.Parallel()
	c := decideClaim()
	t0 := time.Date(2026, 10, 8, 14, 0, 0, 0, time.UTC)
	refill := func(id int64, st protocol.Status, ended time.Time) domain.Order {
		return domain.Order{ID: id, OrderType: protocol.OrderTypeRetrieve, Status: st, DeliveryNode: "SPOT",
			SourceNode: "SRC", CreatedAt: ended.Add(-time.Minute), UpdatedAt: ended}
	}
	ret := domain.Order{ID: 9, OrderType: protocol.OrderTypeMove, Status: protocol.StatusCancelled,
		SourceNode: "SPOT", DeliveryNode: "SRC", CreatedAt: t0, UpdatedAt: t0.Add(time.Minute)}
	swap := func(created time.Time) domain.Order {
		return domain.Order{ID: 20, OrderType: protocol.OrderTypeComplex, Status: protocol.StatusConfirmed,
			DeliveryNode: "LINE", CreatedAt: created}
	}
	for _, st := range []protocol.Status{protocol.StatusCancelled, protocol.StatusFailed, protocol.StatusSkipped} {
		if got := spotPause([]domain.Order{refill(5, st, t0)}, c, nil); !strings.Contains(got, "paused") {
			t.Errorf("a refill ended %s: pause = %q, want paused", st, got)
		}
	}
	if got := spotPause([]domain.Order{refill(5, protocol.StatusConfirmed, t0)}, c, nil); got != "" {
		t.Errorf("a landed refill paused the keeper: %q", got)
	}
	if got := spotPause([]domain.Order{ret}, c, nil); !strings.Contains(got, "return from SPOT") {
		t.Errorf("a cancelled return: pause = %q, want it named", got)
	}
	if got := spotPause([]domain.Order{refill(5, protocol.StatusCancelled, t0), swap(t0.Add(time.Second))}, c, nil); got != "" {
		t.Errorf("an order of the line created after the cancel did not re-arm: %q", got)
	}
	if got := spotPause([]domain.Order{refill(5, protocol.StatusCancelled, t0), swap(t0.Add(-time.Hour))}, c, nil); got == "" {
		t.Error("an order of the line created BEFORE the cancel re-armed it")
	}
	saved := *c
	later := t0.Add(time.Second)
	saved.UpdatedAt = &later
	if got := spotPause([]domain.Order{refill(5, protocol.StatusCancelled, t0)}, &saved, nil); got != "" {
		t.Errorf("a claim save after the cancel did not re-arm: %q", got)
	} // The board's RESUME re-arms on its own, with no order and no claim save.
	resumed := t0.Add(time.Second)
	if got := spotPause([]domain.Order{refill(5, protocol.StatusCancelled, t0)}, c, &resumed); got != "" {
		t.Errorf("a RESUME after the cancel did not re-arm: %q", got)
	}
	before := t0.Add(-time.Second)
	if got := spotPause([]domain.Order{refill(5, protocol.StatusCancelled, t0)}, c, &before); got == "" {
		t.Error("a RESUME from BEFORE the cancel re-armed it")
	}
}

// The board's RESUME, end to end: a cancel pauses the line, RESUME refills the
// spot at once, and nothing is sent to the line (no swap legs).
func TestKeepStagedKeeper_ResumeRefillsWithoutASwap(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := keeperCell(t)
	endedRefill(t, eng, db, nodeID, protocol.StatusCancelled)
	eng.sweepCellLevels()
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
		t.Fatalf("refills = %d while paused, want 0", got.refills)
	}
	// The cancel happened a moment ago: SQLite stamps both at one-second
	// resolution, and a RESUME in the same second as the cancel is not after it.
	_, err := db.DB.Exec(`UPDATE orders SET updated_at = datetime('now', '-5 seconds') WHERE delivery_node = ?`, ksSpot)
	testutil.MustNoErr(t, err, "backdate the cancel")
	testutil.MustNoErr(t, eng.ResumeKeepStaged(nodeID), "resume")
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d after RESUME, want 1", got.refills)
	}
	rows, rerr := db.ListActiveOrdersByProcessNode(nodeID)
	testutil.MustNoErr(t, rerr, "rows")
	for _, o := range rows {
		if o.DeliveryNode == ksLine || o.OrderType == protocol.OrderTypeComplex {
			t.Errorf("RESUME sent order %d (%s to %s) to the line; it must only refill the spot", o.ID, o.OrderType, o.DeliveryNode)
		}
	}
	if note := eng.KeepStagedNote(ksLine); note != "" {
		t.Errorf("board note = %q after RESUME, want it cleared", note)
	}
	if err := eng.ResumeKeepStaged(nodeID + 999); err == nil {
		t.Error("RESUME on an unknown node succeeded")
	}
}

// End to end: a cancelled refill is not re-created by any sweep and the board
// says so; REQUEST re-arms it. The REQUEST orders nothing for the spot itself:
// its swap, with the spot bare, fetches from the market and stages on the spot,
// so the keeper's next pass waits for that leg instead of refilling.
func TestKeepStagedKeeper_CancelPausesAndRequestResumes(t *testing.T) {
	t.Parallel()
	eng, db, nodeID := keeperCell(t)
	endedRefill(t, eng, db, nodeID, protocol.StatusCancelled)
	for i := 0; i < 3; i++ {
		eng.sweepCellLevels()
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
		t.Fatalf("refills = %d after a cancel, want 0", got.refills)
	}
	if note := eng.KeepStagedNote(ksLine); !strings.Contains(note, "paused") {
		t.Fatalf("board note = %q, want it to say the keeper is paused", note)
	}
	// SQLite stamps at one-second resolution, and a REQUEST in the same second
	// as the cancel is not after it.
	_, err := db.DB.Exec(`UPDATE orders SET updated_at = datetime('now', '-5 seconds'), created_at = datetime('now', '-6 seconds') WHERE delivery_node = ?`, ksSpot)
	testutil.MustNoErr(t, err, "backdate the cancel")
	_, err = eng.RequestNodeMaterial(nodeID, 1)
	testutil.MustNoErr(t, err, "REQUEST")
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 {
		t.Fatalf("refills = %d after REQUEST, want none: a request asks only for the line", got.refills)
	}
	eng.sweepCellLevels()
	if note := eng.KeepStagedNote(ksLine); strings.Contains(note, "paused") || !strings.Contains(note, "still bringing a bin") {
		t.Errorf("board note = %q after REQUEST, want the pause gone and the keeper waiting for the swap's staging", note)
	}
}

// NOTHING IS ORDERED AHEAD OF A LIFT. A swap waiting at Core to take the
// standing spare leaves the spot as it is; the refill comes at the lift
// (TestKeepStagedKeeper_ALiftAtTheSpotOrdersTheRefill). So a swap cancelled
// before its lift leaves no refill queued behind a spare that never moved.
func TestKeepStagedKeeper_NothingAheadOfALift(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {Occupied: true, PayloadCode: ksPart}})
	holdingSwap(t, db, nodeID) // built from the keep-staged claim: it lifts the spare
	for i := 0; i < 3; i++ {
		eng.sweepCellLevels()
	}
	if got := readSpotOrders(t, db, nodeID); got.refills != 0 || got.returns != 0 {
		t.Fatalf("refills=%d returns=%d with the spare still standing, want none", got.refills, got.returns)
	}
}

// A WRONG BIN IS LEFT WHERE IT STANDS, on every pass: the keeper neither sends
// it anywhere nor orders a refill that could not be set down, and the board
// names it for a person to move.
func TestKeepStagedKeeper_AWrongBinIsLeftStanding(t *testing.T) {
	t.Parallel()
	for name, wrong := range map[string]NodeBinInfo{
		"another part": {Occupied: true, PayloadCode: "PART-OTHER"},
		"an empty":     {Occupied: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
				map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: wrong})
			for i := 0; i < 3; i++ {
				eng.sweepCellLevels()
			}
			if got := readSpotOrders(t, db, nodeID); got.refills != 0 || got.returns != 0 {
				t.Fatalf("refills=%d returns=%d with a wrong bin on the spot, want none", got.refills, got.returns)
			}
			if note := eng.KeepStagedNote(ksLine); !strings.Contains(note, ksSpot) || !strings.Contains(note, "Move it by hand") {
				t.Errorf("board note = %q, want it to name %s for a person to move", note, ksSpot)
			}
		})
	}
}

// A lift at the spot runs the keeper at once: the refill behind the spare is
// ordered at the pickup, not at the swap's end.
func TestKeepStagedKeeper_ALiftAtTheSpotOrdersTheRefill(t *testing.T) {
	t.Parallel()
	eng, db, nodeID, _ := keepStagedCell(t, protocol.ClaimRoleConsume, protocol.SwapModeTwoRobot,
		map[string]NodeBinInfo{ksLine: {Occupied: true, PayloadCode: ksPart}, ksSpot: {}})
	claim := keeperClaimByID(t, db, nodeID)
	a, _ := BuildTwoRobotSwapSteps(claim)
	leg := mkSwapLeg(t, db, nodeID, "lift-swap", a, "")
	testutil.MustNoErr(t, db.UpdateOrderStatus(leg.ID, string(protocol.StatusInTransit)), "with the fleet")

	order, err := db.GetOrderByUUID(leg.UUID)
	testutil.MustNoErr(t, err, "leg")
	eng.keepSpotOnPickup(order, ksSpot)
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d after the lift, want 1", got.refills)
	}
	eng.keepSpotOnPickup(order, ksLine)
	if got := readSpotOrders(t, db, nodeID); got.refills != 1 {
		t.Fatalf("refills = %d after a pickup elsewhere, want still 1", got.refills)
	}
}
