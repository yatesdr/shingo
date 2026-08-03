//go:build docker

package www

import (
	"net/http"
	"testing"

	"shingocore/fleet/simulator"
	"shingocore/internal/testdb"
)

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
