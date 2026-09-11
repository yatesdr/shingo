//go:build docker

package dispatch

import (
	"errors"
	"strings"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store/reservations"
)

// TestStepOccupancy_GateAndSlotClaimAgree is the drift pin on binsAtStep's
// callers. The destination gate and the slot claim judge the same staging node
// for the same plan, one before the reserve and one after it, and they must
// give the same answer: a plan the gate lets through and the claim then refuses
// holds a partial set for ever, and a plan the gate refuses that the claim would
// have taken never gets asked.
//
// The world in each case is the one the claim sees — the plan's own reserve has
// run — and the gate is asked of that same world.
//
// MUTATIONS: put the steps-only guard back in the gate ("an earlier step picks up
// here") — "another order holds the kept bin" then passes the gate and is refused
// at the claim. Put the plain NOT EXISTS back in ClaimSlotTx — "the plan's own
// bin" passes the gate and is refused at the claim.
func TestStepOccupancy_GateAndSlotClaimAgree(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name      string
		prefix    string
		kept      string // "": inbound staging is empty; "ours" / "stranger": who holds the kept bin
		foreign   bool   // a second bin beside the kept one, held by nobody
		wantClear bool
	}{
		{"nothing there", "OCC1", "", false, true},
		{"the plan's own bin", "OCC2", "ours", false, true},
		{"a stranger's bin beside it", "OCC3", "ours", true, false},
		{"another order holds the kept bin", "OCC4", "stranger", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			db := testDB(t)
			sd := testdb.SetupStandardData(t, db)
			d, _ := newTestDispatcher(t, db, testdb.NewTrackingBackend())
			stage := prNode(t, db, tc.prefix+"-IN-STAGE")
			market := prNode(t, db, tc.prefix+"-MARKET")
			src := prNode(t, db, tc.prefix+"-SRC")
			steps := prResolved(prPick(stage.Name), prDrop(market.Name), prPick(src.Name), prDropExcl(stage.Name),
				prWait(""), prPick(stage.Name), prDrop(sd.LineNode.Name))
			const stageStep = 3
			order := prLegRow(t, db, tc.prefix+"-supply", "", sd.LineNode.Name, sd.LineNode.Name, stage.Name,
				sd.Payload.Code, steps)

			if tc.kept != "" {
				kept := testdb.CreateBinAtNode(t, db, sd.Payload.Code, stage.ID, tc.prefix+"-KEPT")
				holder := order.ID
				if tc.kept == "stranger" {
					holder = prLegRow(t, db, tc.prefix+"-other", "", sd.LineNode.Name, sd.LineNode.Name, stage.Name,
						sd.Payload.Code, prResolved(prPick(stage.Name), prDrop(market.Name))).ID
				}
				testutil.MustNoErr(t, reservations.Acquire(db.DB, holder, holder, kept.ID, "test"), "reserve the kept bin")
			}
			if tc.foreign {
				testdb.CreateBinAtNode(t, db, sd.Payload.Code, stage.ID, tc.prefix+"-FOREIGN")
			}

			st := d.reserveComplexDestination(order, steps)
			gateClear := !(st.done && st.err != nil && strings.Contains(st.err.Error(), "dropoff capacity"))

			// The claim needs its slot reservation; the gate takes it when it lets the
			// plan through, and a refused gate took none.
			if err := db.ReserveSlot(stage.ID, order.ID); err != nil && !errors.Is(err, reservations.ErrReservationConflict) {
				t.Fatalf("reserve the staging slot: %v", err)
			}
			_, takenFirst, err := d.allocator.binsAtStep(order, steps, stageStep, stage.Name)
			testutil.MustNoErr(t, err, "binsAtStep")
			claimOK := db.ConfirmSlotClaim(stage.ID, order.ID, takenFirst) == nil

			if gateClear != claimOK {
				t.Fatalf("the gate says clear=%t and the slot claim says ok=%t for the same node, plan and world. "+
					"Two answers to one question is a plan that passes one check and wedges at the next", gateClear, claimOK)
			}
			if gateClear != tc.wantClear {
				t.Errorf("both callers say clear=%t, want %t", gateClear, tc.wantClear)
			}
		})
	}
}
