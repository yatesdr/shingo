//go:build sim

package engine

import (
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"shingo/protocol/clock"
)

// TestSimOperator_NamesNoPlantsNodes is a tripwire for the defect class that
// killed a two-and-a-half-hour soak.
//
// `const negBinMarket = "SYN_MARKET"` lived in sim_operator.go with a comment
// admitting what it was: "hardcoded to the demo's combined market name. If the
// plant renames/splits its market group, update this... Untested." A second plant
// then arrived with SYN_STAMP and SYN_COMP, the negative-bin sweep looked up a
// group that does not exist, found nothing, and returned at its own length check
// on every tick for the whole run. It logs only when it clears something, so a
// sweep that could not find its own subject was indistinguishable from a sweep
// with nothing to do — and six of the plant's twelve carriers stayed stranded.
//
// THE FAILURE MODE IS SILENCE, which is why a test rather than a comment. Nothing
// about a hardcoded plant name fails loudly: it degrades to a no-op, and a no-op
// sweep looks exactly like a healthy one from outside.
//
// So: the sim operator may not name a plant's nodes. Everything it needs it
// derives — markets from the active claims' inbound/outbound, manual_swap nodes
// from the process-node table. A future helper that wants to know a node name has
// to ask the plant, and this is where it finds that out.
//
// The pattern is the house node-naming convention (a 3+ letter uppercase prefix
// then an underscore: SYN_MARKET, PLN_001, ALN_003, LSD_027, FGN_001). It
// deliberately does not try to be a general "is this a node name" oracle — it
// catches the shape every plant in this repo actually uses, which is the shape
// the bug had.
func TestSimOperator_NamesNoPlantsNodes(t *testing.T) {
	t.Parallel()
	src, err := os.ReadFile("sim_operator.go")
	if err != nil {
		t.Fatalf("read sim_operator.go: %v", err)
	}
	// Node-shaped identifiers inside double-quoted string literals only. A prose
	// comment may name SYN_MARKET all it likes — the tombstone above the deleted
	// constant does, and it should.
	literal := regexp.MustCompile(`"[^"]*"`)
	nodeish := regexp.MustCompile(`\b[A-Z]{3,}_[A-Z0-9]+\b`)

	var found []string
	for i, line := range strings.Split(string(src), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//") {
			continue
		}
		for _, lit := range literal.FindAllString(line, -1) {
			for _, hit := range nodeish.FindAllString(lit, -1) {
				found = append(found, hit+" (line "+itoaLocal(i+1)+")")
			}
		}
	}
	if len(found) > 0 {
		t.Errorf("sim_operator.go names plant nodes in string literals: %s\n"+
			"The sim operator runs against whatever plant is seeded, so a node name written down here "+
			"is a name that can disagree with the plant — and it fails SILENTLY when it does, because "+
			"every lookup degrades to an empty result that reads as 'nothing to do'. That is exactly "+
			"how negBinMarket=\"SYN_MARKET\" stranded half the carrier pool for a whole soak.\n"+
			"Derive it instead: markets from the active claims' InboundSource/OutboundDestination, "+
			"manual_swap nodes from ListProcessNodes.", strings.Join(found, ", "))
	}
}

// TestOperatorHasWorkAt pins the sweep's decision, including both boundaries.
//
// The two roles are exact opposites, so an inverted predicate does not fail
// visibly — it produces an operator that loads full bins and clears empty ones,
// a plant that looks busy and moves nothing.
//
// THE EMPTY-NODE ROWS ARE HERE BECAUSE THE FIRST LIVE RUN NEEDED THEM. This
// predicate originally took only the UOP, and FetchNodeBins returns a row for
// every node it was asked about — so a loader window with nothing on it came back
// as UOP 0 and read as an empty carrier. Four idle loaders were scheduled, each
// failed eight times with "no bin at node — request an empty bin first", and the
// whole thing repeated every reconcile tick. The bin id separates "a carrier is
// here and it is empty" from "nothing is here"; those two cases differ by one
// field and by the entire meaning.
//
// The negative-UOP rows matter for the other reason: an over-consumed carrier is
// still a carrier waiting to be filled, and a `== 0` test would skip precisely
// the bins the negative sweep exists to rescue — six of twelve, on this rig.
func TestOperatorHasWorkAt(t *testing.T) {
	t.Parallel()
	const someBin = int64(7)
	cases := []struct {
		name       string
		wantsEmpty bool
		binID      int64
		uop        int
		want       bool
	}{
		{"loader holding an empty carrier", true, someBin, 0, true},
		{"loader holding an over-consumed carrier", true, someBin, -2, true},
		{"loader holding a full bin", true, someBin, 40, false},
		{"unloader holding a full bin", false, someBin, 20, true},
		{"unloader holding a part-drained bin", false, someBin, 3, true},
		{"unloader holding an empty carrier", false, someBin, 0, false},
		{"unloader holding an over-consumed carrier", false, someBin, -1, false},
		// The rows the live run added.
		{"loader standing empty is NOT an empty carrier", true, 0, 0, false},
		{"unloader standing empty", false, 0, 0, false},
	}
	for _, c := range cases {
		if got := operatorHasWorkAt(c.wantsEmpty, c.binID, c.uop); got != c.want {
			t.Errorf("%s: operatorHasWorkAt(%v, bin=%d, %d) = %v, want %v",
				c.name, c.wantsEmpty, c.binID, c.uop, got, c.want)
		}
	}
}

// itoaLocal keeps the tripwire free of an strconv import for one call.
func itoaLocal(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestSimOperator_StopsPushingReleaseOnAnOrderCoreOwns pins the cap.
//
// The livelock this prevents was measured on the lane-stress rig: 240 refused
// releases in five minutes across four orders, one every 1.25 seconds, each
// costing an outbox row and a Kafka publish. It ran indefinitely.
//
// It cannot be caught by checking the release's error, and that is the whole
// reason a cap exists. ReleaseOrderWithLineside returns nil — the Edge transitions
// the order locally and logs a success — while Core's refusal ("only the lane
// evaluator advances one") arrives much later as an inbound error that puts the
// row back to `staged` and re-fires the trigger. From the operator's side every
// attempt looks like it worked.
//
// MUTATION (run 2026-08-10): remove the cap check from scheduleRelease. The
// attempt count runs away without bound instead of stopping at maxReleaseTries.
func TestSimOperator_StopsPushingReleaseOnAnOrderCoreOwns(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	op.releaseTries = make(map[int64]int)
	op.releasing = make(map[int64]bool)

	// Core keeps refusing, so the order keeps coming back staged and the trigger
	// keeps firing. Twenty firings stands in for "indefinitely".
	for i := 0; i < 20; i++ {
		op.scheduleRelease(99)
		op.mu.Lock()
		delete(op.releasing, 99) // the worker finishes; the order returns to staged
		op.mu.Unlock()
	}

	op.mu.Lock()
	tries := op.releaseTries[99]
	op.mu.Unlock()
	// maxReleaseTries attempts, plus the one increment that marks the cap announced.
	if tries > maxReleaseTries+1 {
		t.Fatalf("the operator pushed Release %d times on one order. Core refuses a lane-waiting "+
			"order every time and the Edge cannot see that it is a lane wait, so an uncapped trigger "+
			"is an infinite round trip — 240 in five minutes on the rig.", tries)
	}
}

// TestSimOperator_ReleaseCapIsForgottenWhenTheOrderMovesOn is the other half, and
// the half that keeps the cap from becoming a permanent give-up.
//
// A lane-waiting order stays `staged` and stays capped until Core lets it in. One
// that has moved on must be forgotten, so the same id staging again later starts
// with a fresh count rather than inheriting a refusal from a previous life.
func TestSimOperator_ReleaseCapIsForgottenWhenTheOrderMovesOn(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	op.releaseTries = map[int64]int{7: maxReleaseTries + 1, 8: 2}

	// Only order 8 is still staged; 7 has moved on.
	stagedNow := map[int64]bool{8: true}
	op.mu.Lock()
	for id := range op.releaseTries {
		if !stagedNow[id] {
			delete(op.releaseTries, id)
		}
	}
	op.mu.Unlock()

	if _, still := op.releaseTries[7]; still {
		t.Error("order 7 has left `staged`, so its refusal count must be forgotten — otherwise the " +
			"cap is a permanent give-up and a later staging inherits it")
	}
	if op.releaseTries[8] != 2 {
		t.Errorf("order 8 is still staged and must keep its count, got %d", op.releaseTries[8])
	}
}

// TestSimOperator_ReleaseCapReArmsSoAStationWaitIsNotAbandoned keeps the cap from
// being a permanent give-up on the population it cannot see.
//
// ── THE PREMISE THAT WAS HALF TRUE ────────────────────────────────────────
//
// The cap assumed a capped order is CORE's to release: "a lane-waiting order
// stays staged and stays capped until Core lets it in, at which point it leaves
// and the count is dropped." True of a LANE wait. False of a STATION wait — Core
// is explicit that it must never advance one ("the precondition is a fact only
// the station can observe"), and in the sim the station IS this operator. So a
// station-waiting order that hit the cap had its only possible releaser go quiet
// for good.
//
// MEASURED, 12e run 2026-08-31: order 91 took three pair-releases inside ONE
// SECOND, hit the cap, and held AMR-15 for the rest of the run — under a message
// telling the reader it was "most likely parked on a LANE wait" while its own
// plan said `wait SLN_010 wait_kind=station`. 24 orders hit the cap that run.
//
// MUTATION: drop the re-arm branch in reArmExpiredReleaseCaps. The order stays
// capped after the window and the operator never pushes again.
func TestSimOperator_ReleaseCapReArmsSoAStationWaitIsNotAbandoned(t *testing.T) {
	m := clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	op := newTestSimOperator(m)
	op.releaseTries = make(map[int64]int)
	op.releasing = make(map[int64]bool)

	// Burn the cap, exactly as a refused order does.
	for i := 0; i < 10; i++ {
		op.scheduleRelease(99)
		op.mu.Lock()
		delete(op.releasing, 99)
		op.mu.Unlock()
	}
	op.mu.Lock()
	capped := op.releaseTries[99] > maxReleaseTries
	_, stamped := op.cappedAt[99]
	op.mu.Unlock()
	if !capped || !stamped {
		t.Fatalf("fixture: the order must be capped and stamped (capped=%v stamped=%v)", capped, stamped)
	}

	// Before the window elapses: still capped. This is the anti-flap half, and it
	// is the behaviour the 2026-08-10 rig incident bought.
	op.reArmExpiredReleaseCaps(map[int64]bool{99: true})
	op.mu.Lock()
	stillCapped := op.releaseTries[99] > maxReleaseTries
	op.mu.Unlock()
	if !stillCapped {
		t.Fatal("the cap was dropped before its window elapsed — that is the flap the cap exists to " +
			"stop: 240 refusals in five minutes, 1796 outbox rows for 46 completed orders")
	}

	// After the window: re-armed, and the operator will push again.
	m.Advance(releaseCapReArm + time.Second)
	op.reArmExpiredReleaseCaps(map[int64]bool{99: true})
	op.mu.Lock()
	tries := op.releaseTries[99]
	_, stillStamped := op.cappedAt[99]
	op.mu.Unlock()
	if tries != 0 || stillStamped {
		t.Fatalf("after %s the cap must re-arm (tries=%d stamped=%v). A STATION wait has no other "+
			"releaser: if this operator stays quiet the order is abandoned holding its robot, which "+
			"is order 91 and AMR-15.", releaseCapReArm, tries, stillStamped)
	}
}
