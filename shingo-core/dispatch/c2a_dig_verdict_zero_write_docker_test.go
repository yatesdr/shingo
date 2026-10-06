//go:build docker

package dispatch

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"shingo/protocol/testutil"
	"shingocore/dispatch/plan"
	"shingocore/internal/testdb"
	"shingocore/store"
)

// THE DIG VERDICT WRITES NOTHING (C2a A.1). plan.DigFits is the planner's part
// that answers "does this dig fit": the owed count and the shuffle walk it is
// handed. The golden's zero-write judge sees it on the three door cases that
// plan a dig, each with room and nothing owed. This runs it where the answer
// turns: four digs started and owing their blockers, a fifth asked against a
// pool they have spent, against a statement-counting store, and judges every
// statement issued inside each call, four fitting and one short.
//
// Not parallel: the observer is process-wide (see vacated_zero_write).
func TestC2aPin_TheDigVerdictWritesNothing(t *testing.T) {
	db, cfg := testdb.OpenWithConfig(t)
	grp, lanes, bp := setupDigsAndParks(t, db, "C2AZ", 5, 4)
	var targets []*BuriedError
	for i, lane := range lanes {
		slots, err := db.ListLaneSlots(lane.ID)
		testutil.MustNoErr(t, err, "list lane slots")
		createTestBinAtNode(t, db, bp.Code, slots[0].ID, fmt.Sprintf("C2AZ-BLK%d", i+1))
		target := createTestBinAtNode(t, db, bp.Code, slots[1].ID, fmt.Sprintf("C2AZ-TGT%d", i+1))
		targets = append(targets, &BuriedError{Bin: target, Slot: slots[1], LaneID: lane.ID})
	}

	cdb, counter, err := store.OpenCounting(cfg)
	testutil.MustNoErr(t, err, "open counting store")
	t.Cleanup(func() { cdb.Close() })
	d, _ := newTestDispatcher(t, cdb, testdb.NewSuccessBackend())

	var (
		mu         sync.Mutex
		depth      int
		start      int
		calls      int
		judged     int
		violations []string
	)
	plan.SetObserver(func(r any, part string, enter bool) {
		if r != any(cdb) || part != "dig-fits" {
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if enter {
			if depth == 0 {
				start = len(counter.Statements())
			}
			depth++
			calls++
			return
		}
		depth--
		if depth == 0 {
			for _, s := range counter.Statements()[start:] {
				judged++
				if !vzwIsRead(s) {
					violations = append(violations, strings.Join(strings.Fields(s), " "))
				}
			}
		}
	})
	t.Cleanup(func() { plan.SetObserver(nil) })

	// Four digs fit (owed 0..3 against four parks) and start; the fifth is short.
	for i := range lanes[:4] {
		demand := mkQueuedComplexParent(t, db, fmt.Sprintf("c2az-demand-%d", i+1), bp.Code)
		d.planBuriedReshuffleAtIntake(demand, bp.Code, "line-1", targets[i])
	}
	fifth := mkQueuedComplexParent(t, db, "c2az-demand-5", bp.Code)
	_, _, shortErr := planUnbury(cdb, targets[4].Bin, targets[4].Slot, lanes[4], grp.ID, digAskerFor(fifth), nil)
	if !errors.Is(shortErr, ErrNoShuffleSlot) {
		t.Fatalf("fixture: the fifth dig answered %v, want ErrNoShuffleSlot (four parks, four owed)", shortErr)
	}

	mu.Lock()
	defer mu.Unlock()
	t.Logf("dig-fits calls %d; %d statements judged", calls, judged)
	if calls != 5 || judged == 0 {
		t.Fatalf("fixture: %d dig-fits call(s), %d statement(s) judged, want 5 calls — the assertion below "+
			"would be vacuous", calls, judged)
	}
	for _, v := range violations {
		t.Errorf("the dig verdict wrote: %s", v)
	}
}
