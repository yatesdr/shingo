//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

// TestDirectSingleLegSwap_LeavesTheProcessNodeBlank pins a blank on purpose,
// which is the sort of thing that otherwise looks like the omission it started
// as.
//
// The two-robot legs carry a process node because the swap hold reads it to
// work out which leg is which. The sequential and single-robot modes make ONE
// order and have no sibling, so nothing about the hold applies — and stamping
// it anyway is not free.
//
// The allocator asks legPlacesLineBin(steps, order.ProcessNode) to decide what
// an order whose source turned out to be empty MEANS. With a process node it
// can read "the replacement has not been staged yet", which is demand, and
// waits for material. With none it reads false and keeps the older answer:
// the work is moot, skip it. That narrowing was deliberate and its comment says
// so — it was written for two-robot legs and explicitly scoped not to re-home
// other traffic.
//
// Filling this in would re-home exactly that traffic: every sequential and
// single-robot order would stop being skipped when its source is empty and
// start waiting for someone to stock it. That might be right. It is not
// obviously right, it is not this change, and it is not something to acquire by
// accident while fixing a sibling link.
func TestDirectSingleLegSwap_LeavesTheProcessNodeBlank(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex",
		map[string]any{
			"cycle_mode":           "sequential",
			"location":             sd.LineNode.Name,
			"outbound_destination": sd.StorageNode.Name,
			"payload_code":         sd.Payload.Code,
			"priority":             1,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rows, err := db.ListOrdersByStation("core-direct", 50)
	if err != nil {
		t.Fatalf("list direct orders: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d orders, want 1 — sequential is a single order", len(rows))
	}
	if rows[0].ProcessNode != "" {
		t.Errorf("process_node = %q, want blank.\n"+
			"If this is being filled in on purpose, the allocator's moot-versus-waiting decision changes for every order of this shape — say so in the commit and change this test.",
			rows[0].ProcessNode)
	}
}

// TestDirectTwoRobotSwap_LegsAreLinked pins that the engineers' page builds a
// swap PAIR rather than two unrelated orders that happen to be about the same
// node.
//
// Only the Edge ever set a sibling uuid. This page minted two complex orders
// and linked neither, which silently switched off every protection that keys
// off the pair: the starvation hold (so the removal leg pulls the line's bin
// with nothing committed to replace it — the ALN_003 strand), the peer-death
// unwind (one leg dies, the other keeps flying), and the abandon sweep's
// cascade.
//
// The process node matters just as much and is the easier half to forget:
// the hold reads the leg's role by comparing its steps against that node, and
// with it blank both role tests short-circuit to "not a swap leg" and the gate
// returns not-held no matter how well the pair is linked.
func TestDirectTwoRobotSwap_LegsAreLinked(t *testing.T) {
	t.Parallel()
	sim := simulator.New()
	h, db := testHandlersWithSim(t, sim)
	sd := testdb.SetupStandardData(t, db)

	rec := postJSON(t, h.apiDirectComplexOrderSubmit, "/api/test-orders/direct/complex",
		map[string]any{
			"cycle_mode":           "two_robot",
			"location":             sd.LineNode.Name,
			"inbound_staging":      sd.StorageNode.Name,
			"inbound_source":       sd.StorageNode.Name,
			"outbound_destination": sd.StorageNode.Name,
			"payload_code":         sd.Payload.Code,
			"priority":             1,
		})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	rows, err := db.ListOrdersByStation("core-direct", 50)
	if err != nil {
		t.Fatalf("list direct orders: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("got %d orders, want 2 (a supply leg and a removal leg)", len(rows))
	}

	a, b := rows[0], rows[1]
	if a.SiblingOrderUUID == "" && b.SiblingOrderUUID == "" {
		t.Fatal("neither leg names the other. An unlinked pair reads as two ordinary " +
			"complex orders, which turns off the starvation hold, the peer-death " +
			"unwind and the abandon cascade all at once.")
	}
	// Forward link on the second-created leg, back-link filled in by intake.
	if a.SiblingOrderUUID != b.EdgeUUID {
		t.Errorf("leg %d points at %q, want %q", a.ID, a.SiblingOrderUUID, b.EdgeUUID)
	}
	if b.SiblingOrderUUID != a.EdgeUUID {
		t.Errorf("leg %d points at %q, want %q", b.ID, b.SiblingOrderUUID, a.EdgeUUID)
	}

	for _, o := range rows {
		if o.ProcessNode != sd.LineNode.Name {
			t.Errorf("leg %d process_node = %q, want %q — blank makes the hold "+
				"read the leg as 'not a swap leg' and pass it straight through",
				o.ID, o.ProcessNode, sd.LineNode.Name)
		}
	}
}
