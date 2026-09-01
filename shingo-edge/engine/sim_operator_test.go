//go:build sim

package engine

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/orders"
	"shingoedge/store/processes"
)

func newTestSimOperator(clk clock.Clock) *simOperator {
	return &simOperator{
		e:       &Engine{logFn: func(string, ...any) {}, debugFn: func(string, ...any) {}},
		clk:     clk,
		ctx:     context.Background(),
		pending: make(map[int64]bool),
		// The cap's backoff state. A fixture that leaves it nil panics the moment
		// an order reaches the cap, which is exactly the path several of these
		// tests drive — construct the type fully rather than partially.
		cappedAt: make(map[int64]time.Time),
	}
}

// T3.2 / Gate 3: a delivered event fires the operator action after the delay,
// and a duplicate delivery to the same node while the first is pending does not
// double-fire.
func TestSimOperator_FiresOnceAfterDelay(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	var calls atomic.Int32
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		return 5 * time.Second, "load", func() error { calls.Add(1); return nil }, true
	}

	op.schedule(42)
	op.schedule(42) // duplicate while pending — must be dropped (dedup is synchronous)

	// Drive the manual clock until the single worker's delay elapses. Advancing
	// before the worker registers its waiter is harmless; loop until it fires.
	deadline := time.Now().Add(2 * time.Second)
	for calls.Load() == 0 && time.Now().Before(deadline) {
		m.Advance(5 * time.Second)
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a wrongly-spawned dup worker would fire here

	if got := calls.Load(); got != 1 {
		t.Fatalf("want exactly 1 action (deduped, after delay), got %d", got)
	}
	op.mu.Lock()
	stillPending := op.pending[42]
	op.mu.Unlock()
	if stillPending {
		t.Fatal("pending[42] should be cleared after the action ran")
	}
}

// T3.2: a node that doesn't classify as a loader/unloader produces no action.
func TestSimOperator_NoActionWhenNotClassified(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	var calls atomic.Int32
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		return 0, "", nil, false
	}
	op.schedule(7)
	m.Advance(10 * time.Second)
	time.Sleep(50 * time.Millisecond)
	if calls.Load() != 0 {
		t.Fatalf("want 0 actions for an unclassified node, got %d", calls.Load())
	}
}

// T3.2: a delivery with no ProcessNodeID is ignored (no panic, no schedule).
func TestSimOperator_IgnoresNilProcessNode(t *testing.T) {
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.classify = func(int64) (time.Duration, string, func() error, bool) {
		t.Fatal("classify must not run for a nil-ProcessNodeID delivery")
		return 0, "", nil, false
	}
	op.onDelivered(Event{Type: EventOrderDelivered, Payload: OrderDeliveredEvent{OrderID: 1}})
	time.Sleep(20 * time.Millisecond)
}

// TestSimOperator_OutboundDeliveryDoesNotScheduleAClear is the 115 give-ups.
//
// A manual_swap unloader's U2 leg carries the drained carrier AWAY — source
// FGN_001, destination SYN_PRESS_EMPTIES — and it is tracked at FGN_001's
// process node. Its delivery therefore scheduled a CLEAR at a node the same
// order had just emptied: the worker waited, found nothing, burned all eight
// retries in about three sim-seconds, and gave up nine sim-seconds before the
// next carrier arrived. The dedupe then let that spurious run swallow the real
// one.
//
// Measured on the sim, 2026-08-30: 115 give-ups in a single run, every one a
// clear that never happened — on a fixture whose empty pool only refills when
// carriers ARE cleared.
func TestSimOperator_OutboundDeliveryDoesNotScheduleAClear(t *testing.T) {
	db := testEngineDB(t)
	eng := testEngine(t, db)
	nodeID, err := db.CreateProcessNode(processes.NodeInput{
		ProcessID: 1, CoreNodeName: "FGN_001", Code: "FG1", Name: "FGN_001", Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")

	out, err := db.CreateOrder("u2-empty-out", orders.TypeMove, &nodeID, false, 1,
		"SYN_PRESS_EMPTIES", "", "FGN_001", "", true, "ASSY")
	testutil.MustNoErr(t, err, "create U2 order")
	in, err := db.CreateOrder("u1-full-in", orders.TypeRetrieve, &nodeID, false, 1,
		"FGN_001", "", "SYN_MARKET", "", true, "ASSY")
	testutil.MustNoErr(t, err, "create U1 order")

	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.e = eng

	if op.deliveryLandedHere(OrderDeliveredEvent{OrderID: out, ProcessNodeID: &nodeID}) {
		t.Error("the U2 empty-out leg counted as a delivery TO FGN_001 — it is the carrier leaving, " +
			"and scheduling a clear on it is what burned the retry budget before the real arrival")
	}
	if !op.deliveryLandedHere(OrderDeliveredEvent{OrderID: in, ProcessNodeID: &nodeID}) {
		t.Error("the U1 full-in leg did NOT count as a delivery to FGN_001 — this is the arrival the " +
			"clear exists for, and skipping it would stop every clear instead of the spurious ones")
	}

	// Unreadable inputs keep the old behaviour: this narrows a noisy trigger, it
	// does not add a gate with its own way to fail closed.
	missing := int64(999999)
	if !op.deliveryLandedHere(OrderDeliveredEvent{OrderID: in, ProcessNodeID: &missing}) {
		t.Error("an unresolvable process node must schedule as before, not be silently skipped")
	}
}

// TestSimOperator_ReconcileDrivesEveryScheduler is the restart-safety net's own
// totality check, and it exists because the net had a hole exactly the shape of
// a deadlock.
//
// ── WHAT IT MISSED ────────────────────────────────────────────────────────
//
// reconcile's whole job is stated in its doc: "re-derive pending operator
// actions from current state … so any order already mid-choreography when this
// operator starts is not invisible to the live-only handlers". It drove three of
// the four schedulers. scheduleFlip — the A/B cutover — had exactly ONE caller,
// onOrderCreated, so the cutover was live-event-only.
//
// MEASURED 2026-08-30, and it is not a corner case. A sequential press's evac
// cannot release until the line flips to the paired side:
//
//	[sim] operator auto-release order 164 rejected: the line is pulling from
//	      PLN_003; flip to PLN_004 first, or confirm to release anyway
//
// Nothing flips unless an order is CREATED against that node, and no order can
// be created for a cell whose swap is stuck. 444 refusals of that one message,
// 500 release-cap announcements, five robots pinned, and PANEL-B production
// stopped for four sim-hours — the operator retrying an action that could not
// succeed until it took a different one first.
//
// ── WHY A SOURCE SCAN AND NOT A BEHAVIOUR TEST ────────────────────────────
//
// The behaviour needs a seeded sequential press, a runtime with ActivePull, a
// paired node and a live claim — a fixture that would pin THIS deadlock and
// nothing about the next one. The property that actually failed is narrower and
// general: the restart-safety net must cover EVERY action the live handlers
// drive. A fifth scheduler added tomorrow with a live-only trigger is the same
// bug again, and this fires for it too.
//
// MUTATION (verified): remove the scheduleFlip call from reconcile. This names
// it and says what a live-only trigger costs.
func TestSimOperator_ReconcileDrivesEveryScheduler(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile(filepath.Clean("sim_operator.go"))
	if err != nil {
		t.Fatalf("read sim_operator.go: %v", err)
	}
	body := string(src)

	// Every scheduler the operator declares.
	decl := regexp.MustCompile(`func \(op \*simOperator\) (schedule\w*)\(`)
	var schedulers []string
	for _, m := range decl.FindAllStringSubmatch(body, -1) {
		schedulers = append(schedulers, m[1])
	}
	if len(schedulers) < 4 {
		t.Fatalf("found %d schedule* helpers (%v) — the scan has drifted from the file and this test "+
			"is checking nothing", len(schedulers), schedulers)
	}

	// reconcile's body, from its declaration to the next top-level func.
	start := strings.Index(body, "func (op *simOperator) reconcile()")
	if start < 0 {
		t.Fatal("reconcile() not found — this test has drifted from the file")
	}
	rest := body[start+1:]
	end := strings.Index(rest, "\nfunc ")
	if end < 0 {
		end = len(rest)
	}
	reconcileBody := rest[:end]

	for _, s := range schedulers {
		if !strings.Contains(reconcileBody, "op."+s+"(") {
			t.Errorf("reconcile() never calls %s, so that action fires ONLY on a live event. An "+
				"operator restart, or a cell whose state needs the action before any new order "+
				"arrives, strands it forever — which is the A/B cutover deadlock this test was "+
				"written for. The restart-safety net has to cover every action, not most of them.", s)
		}
	}
}
