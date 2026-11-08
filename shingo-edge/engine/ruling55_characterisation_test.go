// ruling55_characterisation_test.go — what the seat holds, their replay and
// the ledger's automatic landings do, one case per mechanism decision 55
// removes, each beside its prediction ("Decision 55:"): every one becomes
// charged, logged, or the row stays. The ruling's commit flips each case to its
// prediction; nothing else in it changes.
package engine

import (
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/domain"
)

// seat55 is what a case reads at a position: the cart's count, the seat's
// hold, and the open ledger rows' quantity there (any reason).
type seat55 struct {
	cart   int
	hold   int64
	logged int64
}

func readSeat55(t *testing.T, eng *Engine, pid, nodeID int64) seat55 {
	t.Helper()
	rt := runtimeOf(t, eng, nodeID)
	s := seat55{cart: rt.RemainingUOPCached, hold: rt.PendingUOPDelta}
	for _, h := range openHolds(t, eng, pid) {
		if h.ProcessNodeID == nodeID {
			s.logged += h.Qty
		}
	}
	return s
}

func wantSeat55(t *testing.T, eng *Engine, pid, nodeID int64, what string, want seat55) {
	t.Helper()
	if got := readSeat55(t, eng, pid, nodeID); got != want {
		t.Errorf("%s: cart %d, hold %d, logged %d; want cart %d, hold %d, logged %d", what,
			got.cart, got.hold, got.logged, want.cart, want.hold, want.logged)
	}
}

// openQty is the process's open ledger quantity, every position and the
// whole draws.
func openQty(t *testing.T, eng *Engine, pid int64) int64 {
	t.Helper()
	n := int64(0)
	for _, h := range openHolds(t, eng, pid) {
		n += h.Qty
	}
	return n
}

// TestPinRuling55_SeatHoldsAndTheLedgersLandings: today's seat holds and the
// ledger's automatic landings, each with what it does under decision 55.
func TestPinRuling55_SeatHoldsAndTheLedgersLandings(t *testing.T) {
	t.Parallel()

	t.Run("D12: strokes at an empty seat are held, and replay onto the next cart of their part", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		pid, nodeID, styleID, _ := emptySeat(t, eng, "R55A", "PART-A", 100)
		for i := 0; i < 3; i++ {
			tickConsume(eng, pid, styleID, 1)
		}
		// Decision 55: logged — {0, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "after 3 strokes at the empty seat", seat55{0, 3, 0})
		eng.HandleBinLanded(landing("R55A-NODE", 8551, 1, 100, "PART-A", 7))
		tickConsume(eng, pid, styleID, 1)
		// Decision 55: the cart lands at its full count and takes its own
		// stroke; the row stays — {99, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "the next cart of the part after one stroke", seat55{96, 0, 0})
	})

	t.Run("D12: another part's hold is kept, and a stroke beside it is neither held nor charged", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		pid, nodeID, from, to, fromClaim, toClaim := seedChangeoverScenario(t, db)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBin(nodeID, &fromClaim, nil, 0), "empty the seat")
		for i := 0; i < 2; i++ {
			tickConsume(eng, pid, from, 1)
		}
		// Decision 55: logged — {0, 0, 2}.
		wantSeat55(t, eng, pid, nodeID, "after 2 PART-OLD strokes at the empty seat", seat55{0, 2, 0})
		testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &to), "the switch to PART-NEW")
		testutil.MustNoErr(t, db.SetProcessNodeRuntime(nodeID, &toClaim, 0), "seat on PART-NEW's claim")
		tickConsume(eng, pid, to, 1)
		// Decision 55: logged, under PART-NEW — {0, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "after a PART-NEW stroke beside the PART-OLD hold", seat55{0, 2, 0})
		eng.HandleBinLanded(landing("CO-NODE", 8561, 1, 200, "PART-NEW", 7))
		tickConsume(eng, pid, to, 1)
		// Decision 55: the rows stay — {199, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "the PART-NEW cart after one stroke", seat55{199, 2, 0})
	})

	t.Run("R3: a cart released at 0 takes no stroke; past the pile the strokes are held for the next cart", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		pid, nodeID, styleID, claimID := emptySeat(t, eng, "R55C", "PART-C", 100)
		_, err := db.CaptureLinesideBucket(nodeID, "PART-C", 3)
		testutil.MustNoErr(t, err, "seed the pile")
		releaseEmptyAt(t, eng, nodeID, claimID, "R55C-NODE", 8571, 4)
		for i := 0; i < 5; i++ {
			tickConsume(eng, pid, styleID, 1)
		}
		if got := pileQty(t, eng, nodeID); got != 0 {
			t.Errorf("pile = %d, want 0 (the pile drains first)", got)
		}
		// Decision 55: logged; the cart stays at 0 — {0, 0, 2}.
		wantSeat55(t, eng, pid, nodeID, "the cart released at 0, after 5 strokes", seat55{0, 2, 0})
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, "R55C-NODE"), "the lift")
		eng.HandleBinLanded(landing("R55C-NODE", 8572, 1, 100, "PART-C", 7))
		tickConsume(eng, pid, styleID, 1)
		// Decision 55: the next cart lands fresh; the row stays — {99, 0, 2}.
		wantSeat55(t, eng, pid, nodeID, "the next cart of the part after one stroke", seat55{97, 0, 0})
	})

	t.Run("B v1: the run-out after the lift is held under the incoming part and prepays its cart", func(t *testing.T) {
		t.Parallel()
		eng, _, pid, nodeID, from, _ := runOutRig(t)
		runOutToGap(t, eng, pid, nodeID, from)
		// Decision 55: logged, under PART-OLD — {0, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "after the 3 strokes in the gap", seat55{0, 3, 0})
		landCart(t, eng, nodeID, 9102, "PART-NEW", 53)
		for i := 0; i < 4; i++ {
			tickConsume(eng, pid, from, 1)
		}
		// Decision 55: a PART-OLD stroke at the PART-NEW cart is logged; the
		// cart stays at its full count — {53, 0, 7}.
		wantSeat55(t, eng, pid, nodeID, "the incoming cart after 4 run-out strokes", seat55{46, 0, 0})
	})

	t.Run("decision 45: START retags a run-out hold of the departing part to the incoming part", func(t *testing.T) {
		t.Parallel()
		eng, pid, nodeID, from := runOutBeforeTheStart(t)
		// Decision 55: nothing to retag; the 3 before the start are logged —
		// {0, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "after the start", seat55{0, 3, 0})
		for i := 0; i < 2; i++ {
			tickConsume(eng, pid, from, 1)
		}
		// Decision 55: logged — {0, 0, 5}.
		wantSeat55(t, eng, pid, nodeID, "after 2 strokes in the changeover", seat55{0, 5, 0})
		landCart(t, eng, nodeID, 9102, "PART-NEW", 53)
		tickConsume(eng, pid, from, 1)
		// Decision 55: logged; the incoming cart is fresh — {53, 0, 6}.
		wantSeat55(t, eng, pid, nodeID, "the incoming cart after one run-out stroke", seat55{47, 0, 0})
	})

	t.Run("the cancel retags the hold back to the staying part", func(t *testing.T) {
		t.Parallel()
		eng, _, pid, nodeID, from, _ := runOutRig(t)
		runOutToGap(t, eng, pid, nodeID, from)
		testutil.MustNoErr(t, eng.CancelProcessChangeover(pid), "cancel")
		// Decision 55: nothing to retag; the rows stay — {0, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "after the cancel", seat55{0, 3, 0})
		landCart(t, eng, nodeID, 9105, "PART-OLD", 30)
		tickConsume(eng, pid, from, 1)
		// Decision 55: the cart lands fresh — {29, 0, 3}.
		wantSeat55(t, eng, pid, nodeID, "the re-landed old cart after one stroke", seat55{26, 0, 0})
	})

	t.Run("decision 52: a cart that cannot say, bound before the start, holds the incoming strokes", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := unsaidUnderAChangeover(t, "before")
		mark := deltaMark(eng)
		tickConsume(eng, pid, alt, 1)
		// Decision 55: logged under MIRROR-R, the cart (the departing part)
		// untouched — {30, 0, 1}.
		wantSeat55(t, eng, pid, nodeID, "after an ALT stroke", seat55{30, 1, 0})
		if d := deltasSince(eng, mark); len(d) != 0 {
			t.Errorf("deltas %+v, want none (Decision 55: none)", d)
		}
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, otherPartNode), "the cart leaves")
		eng.HandleBinLanded(landing(otherPartNode, 9911, 1, 40, "MIRROR-R", 7))
		tickConsume(eng, pid, alt, 1)
		// Decision 55: the incoming cart lands fresh — {39, 0, 1}.
		wantSeat55(t, eng, pid, nodeID, "the MIRROR-R cart after one stroke", seat55{38, 0, 0})
	})

	t.Run("decision 38 rule 2: a stroke at a known cart of another part is held for a cart of its part", func(t *testing.T) {
		t.Parallel()
		eng, pid, _, alt, nodeID := otherPartRig(t, false, "MIRROR-L", true, nil)
		tickConsume(eng, pid, alt, 1)
		// Decision 55: logged, the cart unchanged — {30, 0, 1}.
		wantSeat55(t, eng, pid, nodeID, "after an ALT stroke at the MIRROR-L cart", seat55{30, 1, 0})
		testutil.MustNoErr(t, eng.carrierLeft(nodeID, otherPartNode), "the MIRROR-L cart leaves")
		eng.HandleBinLanded(landing(otherPartNode, 9911, 1, 40, "MIRROR-R", 7))
		tickConsume(eng, pid, alt, 1)
		// Decision 55: the MIRROR-R cart lands fresh — {39, 0, 1}.
		wantSeat55(t, eng, pid, nodeID, "the MIRROR-R cart after one stroke", seat55{38, 0, 0})
	})

	t.Run("D11 (ii): the strokes counted after the robot took the cart are held for the next cart", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		_, nodeID, _, claimID := seedConsumeNode(t, db, consumeNodeConfig{Prefix: "R55I", PayloadCode: "PART-I", UOPCapacity: 50})
		bin := int64(8591)
		testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, &claimID, &bin, 3, 40), "bind at generation 3")
		eng := testEngine(t, db)
		eng.SetInventoryDeltaSink(&afterLiftSink{sum: -4})
		eng.recordLinesideCarrier(nodeID, "R55I-NODE", domain.KnownCarrier("PART-I"), domain.CarrierFromDelivery)
		node, err := db.GetProcessNode(nodeID)
		testutil.MustNoErr(t, err, "read node")
		held, part := eng.takeTicksAfterLift(node, runtimeOf(t, eng, nodeID), protocol.BinPickedUp{BinID: bin, Epoch: 4, PickedUpAt: time.Now()})
		// Decision 55: nothing held; the 4 are logged at the position.
		if held != 4 || part != "PART-I" {
			t.Errorf("after the lift: %d held of %q; want 4 of PART-I", held, part)
		}
		if n := openQty(t, eng, node.ProcessID); n != 0 {
			t.Errorf("logged %d, want 0", n)
		}
	})

	t.Run("the ledger lands a whole draw at the completion onto its style", func(t *testing.T) {
		t.Parallel()
		eng, pid, a, _, _ := holdRig(t, "1003")
		strokeAt(eng, pid, a, 2)
		c := pinnedStyle(t, eng, pid, "STYLE-C", "1003")
		cNode := cartNode(t, eng, pid, c, "C-NODE", "PART-C", "PART-C", 9801)
		landOn(t, eng, pid, a, c)
		// Decision 55: the row stays; the cart is untouched — cart 40, open 2.
		if got, open := runtimeOf(t, eng, cNode).RemainingUOPCached, openQty(t, eng, pid); got != 38 || open != 0 {
			t.Errorf("after the completion: C's cart %d, open %d; want 38, 0 (released onto it)", got, open)
		}
	})

	for _, row := range []struct {
		name, later    string
		qty            int64
		cart, open     int
		cart55, open55 int
	}{
		// Decision 55: the row stays, the cart lands fresh — 40, 3.
		{name: "the ledger lands a share at a landing of its part (f2203783)", cart: 37, open: 0, cart55: 40, open55: 3},
		// Decision 55: the row stays — 40, 3.
		{name: "the since-guard does not count a cancelled changeover (decision 47)", later: "cancelled", cart: 37, open: 0, cart55: 40, open55: 3},
		// Decision 55: the row stays — 40, 3 (unchanged: nothing is released to refuse).
		{name: "the since-guard holds a share past a changeover started since", later: "open", cart: 40, open: 3, cart55: 40, open55: 3},
		// Decision 55: the row stays — 40, 56 (unchanged: nothing is released to refuse).
		{name: "the fit refuses a share bigger than the cart (decision 46)", qty: 56, cart: 40, open: 56, cart55: 40, open55: 56},
	} {
		t.Run(row.name, func(t *testing.T) {
			t.Parallel()
			eng, pid, a, b, _ := holdRig(t, "1002")
			node := mirrorNode(t, eng, pid, a, b, "N-MIRROR", "MIRROR-L", "MIRROR-R", 9751)
			qty := row.qty
			if qty == 0 {
				qty = 3
			}
			_, err := eng.db.HoldCatidDraw(pid, "1002", node, "MIRROR-R", holdReasonOffPlan, qty, time.Now().Add(-time.Minute))
			testutil.MustNoErr(t, err, "B's share held at N-MIRROR a minute ago")
			_, err = eng.changeoverService.Create(pid, &a, b, "test", "", nil, nil, nil, nil)
			testutil.MustNoErr(t, err, "the changeover onto B, after the hold opened")
			testutil.MustNoErr(t, eng.SetProcessActiveStyle(pid, &b), "the cutover onto B, the MIRROR-L cart still there")
			if row.later != "" {
				later, err := eng.changeoverService.Create(pid, &b, a, "test", "", nil, nil, nil, nil)
				testutil.MustNoErr(t, err, "another changeover")
				if row.later == "cancelled" {
					testutil.MustNoErr(t, eng.db.UpdateProcessChangeoverState(later, domain.ChangeoverCancelled), "cancelled")
					testutil.MustNoErr(t, eng.db.SetTargetStyle(pid, nil), "the cancel clears the target")
				}
			}
			testutil.MustNoErr(t, eng.carrierLeft(node, "N-MIRROR"), "the MIRROR-L cart lifted")
			eng.HandleBinLanded(protocol.BinLanded{BinID: 9752, CoreNodeName: "N-MIRROR", Count: 40, Epoch: 1,
				PayloadCode: "MIRROR-R", PayloadKnown: true, PlacedSeq: 1 << 40})
			if got, open := runtimeOf(t, eng, node).RemainingUOPCached, openQty(t, eng, pid); got != row.cart || open != int64(row.open) {
				t.Errorf("after the MIRROR-R landing: cart %d, open %d; want %d, %d", got, open, row.cart, row.open)
			}
		})
	}

	t.Run("the stranded monitor watches a seat hold", func(t *testing.T) {
		t.Parallel()
		db := testEngineDB(t)
		eng := testEngine(t, db)
		eng.wireEventHandlers()
		pid, nodeID, styleID, _ := emptySeat(t, eng, "R55L", "PART-L", 100)
		sm := &strandedMonitor{eng: eng, states: map[int64]*strandedNodeState{}}
		base := time.Now()
		for i := 0; i < strandedWindow+1; i++ {
			tickConsume(eng, pid, styleID, 3)
			sm.tick(base.Add(time.Duration(i) * time.Minute))
		}
		// Decision 55: no hold to watch; the 12 are a row in the ledger, on
		// the strip — {0, 0, 12}, and no parked-ticks alarm.
		wantSeat55(t, eng, pid, nodeID, "after 4 scans of rising strokes", seat55{0, 12, 0})
		if eng.StrandedAlarmDetail("R55L-NODE") == "" {
			t.Error("no parked-ticks alarm, want one")
		}
	})
}
