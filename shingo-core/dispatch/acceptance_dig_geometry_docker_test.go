//go:build docker

package dispatch

import (
	"bytes"
	"log"
	"os"
	"strings"
	"sync"
	"testing"

	"shingocore/internal/testdb"
	"shingocore/store/orders"
)

// acceptance_dig_geometry_docker_test.go — the 2f pin: the three geometry faults
// out of a dweller's own dig summons are LOUD, and the slot is named.
//
// The old `default:` arm in summonOwnDigs folded laneClearNoGroup,
// laneClearSlotNotInLane and laneClearUnplannable into one quiet line shared
// with every other unplannable outcome, while the complex path
// (proposeDigForBuriedPickup, complex_dispatch.go) gives the same three their own
// arm and a sentence that says the corridor is never going to open. §R.45 ruled
// the disposition: a slot attached to no lane keeps failing loudly WITH THE SLOT
// NAMED — no bin moving anywhere changes which nodes exist, so the wait has no
// releaser, and a quiet line is a robot standing at a mark with nobody told why.
//
// The demand is NOT failed in either arm: a committed robot cannot be re-planned,
// so the order stays staged and the next pass re-asks in case the configuration
// is fixed underneath it. What changes is that the fault is now said out loud, in
// the shape the complex path already established.
//
// MUTATION (verified this session): fold the three outcomes back into the old
// default arm and this test fails — the captured sentence loses both the entry
// slot's name and the "corridor nothing is going to open" clause, which is the
// quiet line the finding is about.

// captureDispatchLog swaps the package logger for a buffer, the same seam
// fleet/simulator's and messaging's tests use. The dweller's dig summons logs
// through the standard logger, so this is the one place the sentence can be read
// back from without inventing a test-only hook in production code.
func captureDispatchLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf syncBuffer
	log.SetOutput(&buf)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	return &buf.buf
}

// syncBuffer serializes writes from parallel test goroutines — the logger is
// package-global and the dispatch tests run parallel, so an unsynchronized
// buffer is a data race the -race build would find.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// TestSummonOwnDigs_GeometryFaultIsLoudAndNamesTheSlot drives the dweller's own
// dig summons against a lane no group owns and asserts the loud sentence: the
// entry slot named, the corridor said never to open, the fault called
// configuration rather than congestion.
func TestSummonOwnDigs_GeometryFaultIsLoudAndNamesTheSlot(t *testing.T) {
	t.Parallel()
	db := testDBShared(t)
	d, _ := newTestDispatcher(t, db, testdb.NewSuccessBackend())

	lane, slots, bp := orphanLane(t, db, "GEOM")
	buf := captureDispatchLog(t)

	// A bin in front of the entry, so the shape is the buried one; a dweller to
	// own the dig. acceptanceDigNeeded's facts are not re-driven here — the arm
	// under test is the OUTCOME DISPOSITION, and the request is exactly what the
	// evaluator would have built for this lane. orphanLane is the fixture the
	// planner's own parentless-lane pin already uses (one spelling of the shape,
	// law 3) — its mouth slot is the entry.
	createTestBinAtNode(t, db, bp.Code, slots[0].ID, "GEOM-WALL")
	dweller := testdb.CreateOrder(t, db, func(o *orders.Order) {
		o.EdgeUUID = "geom-dweller"
		o.Status = "staged"
	})
	req := acceptanceRequest{order: dweller, entry: slots[0]}

	d.summonOwnDigs(lane, req)

	got := buf.String()
	// (a) THE SLOT IS NAMED — §R.45's own wording for what loud means here.
	if !strings.Contains(got, slots[0].Name) {
		t.Errorf("the geometry fault does not name the entry slot %s. The log reads:\n  %s"+
			"\n§R.45: a slot attached to no lane keeps failing loudly WITH THE SLOT NAMED.",
			slots[0].Name, got)
	}
	// (b) AND SAYS THE CORRIDOR WILL NOT OPEN — the sentence half that separates
	// this arm from congestion, which is what the quiet default buried it under.
	if !strings.Contains(got, "nothing is going to open") {
		t.Errorf("the geometry fault does not say the corridor will not open. The log reads:\n  %s"+
			"\nThis is the complex path's clause (complex_dispatch.go) and it is the half an "+
			"operator needs: the wait has no releaser.", got)
	}
	// (c) AND THE ORDER IS UNTOUCHED — loud is not failed.
	after, err := db.GetOrder(dweller.ID)
	if err != nil {
		t.Fatalf("reload dweller: %v", err)
	}
	if after.Status != "staged" {
		t.Errorf("the dweller is %q after the fault — loud is not failed; the next pass re-asks", after.Status)
	}
}
