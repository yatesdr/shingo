//go:build docker

package dispatch

import (
	"encoding/json"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/store"
	"shingocore/store/nodes"
)

// c2a_gate_rebind_interior_drop_docker_test.go — C2a's A.5 (the orc's ruling 2,
// 2026-10-06; Lane F's D1 / F3, STUDY-floor-and-gate.md §1(c)). It was Lane F's
// C2b pin, red at ebb4dae0; ruled C2a's, a flat bug in base code.
//
// rebindGatedDropoff re-binds the drop its gate speaks for, at entryIndex. For a
// swap that drop is INTERIOR: a pickup follows it in the plan. Three things went
// wrong there, each fixed by one line of the study:
//
//   - it wrote the lane slot into orders.delivery_node, the order's FINAL
//     destination. handleStoreBlockCompleted then took the lane drop for the final
//     one (location == delivery_node) and left the bin in _TRANSIT until the whole
//     order finished, so the slot book read the lane's mouth as empty. That is how
//     dig 242 was admitted in front of bin 10 (s1-after-660fd228-20m, order 233);
//   - it read the slot it already held from delivery_node, so it re-bound "PLN_002
//     → LSD_001" every time, even when the step already named the right slot;
//   - it released the slot claim on that node: the swap's far end.
//
// Now delivery_node is written only when entryIsFinalDropoff; `current` is the
// entry step's own node; and only that slot's claim is released.

// TestC2aPin_AGateRebindOfAnInteriorDropKeepsTheOrdersDestination: the gate
// releases the swap into lane D, and the order still ends at the press.
//
// Two shapes:
//   - "kept": the plan's lane drop already names the slot the selector picks (the
//     run's shape). Nothing is re-bound; the plan and the row stay as they were.
//   - "re-aimed": the plan names the shallower slot, so the gate moves the drop to
//     the deeper one. The step moves, the row does not, and the claim the swap
//     holds at the press is not touched; the old lane slot's claim is.
func TestC2aPin_AGateRebindOfAnInteriorDropKeepsTheOrdersDestination(t *testing.T) {
	t.Parallel()
	deepest := func(db *store.DB, laneD int64, _ []*nodes.Node) *nodes.Node {
		n, err := db.FindStoreSlotInLaneExcluding(laneD, 0)
		testutil.MustNoErr(t, err, "the lane's own pick")
		return n
	}
	shallower := func(db *store.DB, laneD int64, slots []*nodes.Node) *nodes.Node {
		best := deepest(db, laneD, slots)
		for _, s := range slots {
			if s.ID != best.ID {
				return s
			}
		}
		t.Fatal("fixture: lane D has one slot")
		return nil
	}

	for _, tc := range []struct {
		name    string
		drop    func(*store.DB, int64, []*nodes.Node) *nodes.Node
		reAimed bool
	}{
		{"kept", deepest, false},
		{"re-aimed", shallower, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newC2bSwapDroppingAt(t, tc.drop)
			db, d := f.db, f.d
			atStation, err := db.GetOrder(f.swap.ID)
			testutil.MustNoErr(t, err, "reload the swap")

			// The slot the gate should end on, asked before the swap holds anything.
			want := f.dDrop
			if tc.reAimed {
				want = deepest(db, f.laneD, nil)
			}

			// The swap holds its old lane slot, and a claim at its far end that the
			// re-bind has no business releasing. Taken BEFORE the station release:
			// since C2b F1 that release is the lane wait's own event, so the gate
			// re-binds on it.
			claim := func(n *nodes.Node, what string) {
				testutil.MustNoErr(t, db.ReserveSlot(n.ID, f.swap.ID), "reserve "+what)
				testutil.MustNoErr(t, db.ConfirmSlotClaim(n.ID, f.swap.ID, nil), "claim "+what)
			}
			if tc.reAimed {
				claim(f.dDrop, "the plan's lane slot")
			}
			claim(f.press2, "the press")

			d.HandleOrderRelease(d.syntheticEnvelope(atStation.StationID),
				&protocol.OrderRelease{OrderUUID: atStation.EdgeUUID})

			// The gate's release, by whichever trigger: the evaluator is the one
			// door. The station release above already evaluated lane D (F1); a
			// second evaluation finds nothing owed.
			d.EvaluateLaneReleases(f.laneD)

			after, err := db.GetOrder(f.swap.ID)
			testutil.MustNoErr(t, err, "reload after the gate release")
			if after.WaitIndex < 2 {
				t.Fatalf("fixture: the gate did not release the swap into lane %d (wait_index %d, cause %q)",
					f.laneD, after.WaitIndex, after.QueueCause)
			}
			if after.DeliveryNode != f.press2.Name {
				t.Errorf("delivery_node = %q, want the swap's destination %q: the gate wrote its INTERIOR drop "+
					"into the order's final destination, so the bin it sets down in the lane reads as final and "+
					"goes unrecorded until the order finishes", after.DeliveryNode, f.press2.Name)
			}

			var steps []resolvedStep
			testutil.MustNoErr(t, json.Unmarshal([]byte(after.StepsJSON), &steps), "read the plan")
			if steps[3].Node != want.Name {
				t.Errorf("the lane drop (step 3) = %q, want %q", steps[3].Node, want.Name)
			}
			if steps[6].Node != f.press2.Name {
				t.Errorf("the final drop (step 6) = %q, want the press %q", steps[6].Node, f.press2.Name)
			}

			press2, err := db.GetNode(f.press2.ID)
			testutil.MustNoErr(t, err, "reload the press")
			if press2.ClaimedBy == nil || *press2.ClaimedBy != f.swap.ID {
				t.Errorf("the press's claim by order %d was released (claimed_by %v): the re-bind released "+
					"the swap's far end as if it were the lane slot it left", f.swap.ID, press2.ClaimedBy)
			}
			if tc.reAimed {
				old, err := db.GetNode(f.dDrop.ID)
				testutil.MustNoErr(t, err, "reload the slot the plan left")
				if old.ClaimedBy != nil && *old.ClaimedBy == f.swap.ID {
					t.Errorf("the swap still claims %s, the lane slot it was re-aimed off", f.dDrop.Name)
				}
			}
		})
	}
}

// TestC2aPin_ARedirectOverlayNeverReAimsTheSwapsReturnAtItsLaneDrop: the
// release-time redirect overlay (patchRedirectSegments) rewrites the FINAL
// segment's last dropoff to orders.delivery_node. While the gate's re-bind wrote
// the interior lane slot into that column, a later release of the swap's last
// segment (pickup the empty, drop it at the press) re-aimed the empty at the lane
// slot the same swap had just filled: two unloads at one node. The hard release
// reaches that segment directly; the station release and the vacated-slot fence
// read the same overlay.
//
// Now the column holds the press, and the overlay rewrites the press to itself.
func TestC2aPin_ARedirectOverlayNeverReAimsTheSwapsReturnAtItsLaneDrop(t *testing.T) {
	t.Parallel()
	f := newC2bSwap(t)
	db, d, backend := f.db, f.d, f.backend
	atStation, err := db.GetOrder(f.swap.ID)
	testutil.MustNoErr(t, err, "reload the swap")
	d.HandleOrderRelease(d.syntheticEnvelope(atStation.StationID),
		&protocol.OrderRelease{OrderUUID: atStation.EdgeUUID})
	d.EvaluateLaneReleases(f.laneD)

	atM, err := db.GetOrder(f.swap.ID)
	testutil.MustNoErr(t, err, "reload after the gate release")
	if atM.WaitIndex != 2 {
		t.Fatalf("fixture: the swap should stand at lane M's wait (wait_index 2), got %d (cause %q)",
			atM.WaitIndex, atM.QueueCause)
	}
	var steps []resolvedStep
	testutil.MustNoErr(t, json.Unmarshal([]byte(atM.StepsJSON), &steps), "read the plan")
	laneDrop := steps[3].Node

	testutil.MustNoErr(t, d.HardReleaseStagedOrder(f.swap.ID, "c2a-pin"), "hard release lane M's wait")
	calls := backend.ReleaseCalls()
	if len(calls) == 0 {
		t.Fatal("fixture: no fleet append at all")
	}
	last := calls[len(calls)-1].Blocks
	if len(last) == 0 {
		t.Fatal("fixture: the last append carried no blocks")
	}
	final := last[len(last)-1]
	if final.Location != f.press2.Name {
		t.Errorf("the empty's return drop = %s, want the press %s", final.Location, f.press2.Name)
	}
	if final.Location == laneDrop {
		t.Errorf("the empty's return drop names %s, the lane slot this swap set its full bin down in: "+
			"two unloads at one node", laneDrop)
	}
}
