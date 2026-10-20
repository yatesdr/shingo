//go:build docker

package dispatch

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingocore/internal/testdb"
	"shingocore/store"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/payloads"
)

// ab_strand_docker_test.go — the A/B strand (FINDINGS-ab-strand-press10-2026-10-20): two orders want the same part from one
// lane. The holder H is dispatched, hard-claims the mouth bin X for a pickup
// after its station wait, and parks at the wait. The digger R sources the bin Y
// behind X and needs X dug, which the hard claim refused as "a robot is carrying
// it out" while no robot was. The claim exchange gives R the mouth bin and H the
// bin behind it, when H's pickup is unsent, H is not at this lane's mark, X is
// the only bin in the way, the parts match, and R has no station wait before its
// pickup. Every refusal keeps the claims as they were.

// abStrand is one variant of the fixture.
type abStrand struct {
	diggerWaits   bool // R has a station wait before its pickup
	mouthPart     bool // X carries another part (and H wants that part)
	releaseHolder bool // H's station wait is released: its pickup is with the fleet
	holderAtMark  bool // H's current wait is a lane wait on the dug lane
	secondBlocker bool // a second bin stands between X and Y
}

type abStrandRun struct {
	db                *store.DB
	d                 *Dispatcher
	backend           *testdb.MockTrackingBackend
	lane              *nodes.Node
	mouth, deep       *nodes.Node
	mouthBin, deepBin int64
	holder, digger    *orders.Order
	holderSteps       string // H's steps_json as dispatched
	holderReadAt      *orders.Order
}

func abStrandFixture(t *testing.T, prefix string, v abStrand) *abStrandRun {
	t.Helper()
	db := testDB(t)
	grp, lane, slots, _, bp := window4Lane(t, db, prefix)
	testutil.MustNoErr(t, db.SetNodeProperty(grp.ID, "retrieve_algorithm", "FIFO"), "set FIFO")
	backend := testdb.NewTrackingBackend()
	d := NewDispatcher(db, backend, &mockEmitter{}, "core", "shingo.dispatch", &DefaultResolver{DB: db})
	d.DebugLog = testDebugLog(t)
	pressA, pressB := lineNode(t, db, prefix+"-PRS-A"), lineNode(t, db, prefix+"-PRS-B")

	holderPart := bp.Code
	if v.mouthPart {
		other := prefix + "-Q"
		testutil.MustNoErr(t, db.CreatePayload(&payloads.Payload{Code: other}), "create the other part")
		holderPart = other
	}
	r := &abStrandRun{db: db, d: d, backend: backend, lane: lane, mouth: slots[0]}
	r.mouthBin = createTestBinAtNode(t, db, holderPart, slots[0].ID, prefix+"-MOUTH").ID
	deepSlot := slots[1]
	if v.secondBlocker {
		createTestBinAtNode(t, db, bp.Code+"-X", slots[1].ID, prefix+"-MID")
		deepSlot = slots[2]
	}
	r.deep = deepSlot
	r.deepBin = createTestBinAtNode(t, db, bp.Code, deepSlot.ID, prefix+"-DEEP").ID

	hUUID, gUUID := prefix+"-holder", prefix+"-digger"
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: hUUID, PayloadCode: holderPart, Quantity: 1, ProcessNode: pressA.Name,
		Steps: []protocol.ComplexOrderStep{prWait(pressA.Name), prPick(grp.Name), prDrop(pressA.Name)},
	})
	prScanPass(t, d, db, hUUID)
	h := prReloadUUID(t, db, hUUID)
	testutil.MustNoErr(t, d.lifecycle.MarkInTransit(h, "AMR-1", "fleet"), "the holder's robot drives to its wait")
	testutil.MustNoErr(t, d.lifecycle.MarkStaged(h, "fleet"), "the holder's robot parks at its station wait")
	h = prReloadUUID(t, db, hUUID)
	if b, err := db.GetBin(r.mouthBin); err != nil || b.ClaimedBy == nil || *b.ClaimedBy != h.ID {
		t.Fatalf("fixture: the holder (order %d, %s) must hard-claim the mouth bin %d; bin = %+v (%v)",
			h.ID, h.Status, r.mouthBin, b, err)
	}
	if v.holderAtMark {
		steps, ok := decodeSteps(h.StepsJSON)
		if !ok || steps[0].Action != protocol.ActionWait {
			t.Fatalf("fixture: holder steps %q", h.StepsJSON)
		}
		steps[0].WaitKind, steps[0].WaitLane = WaitKindLane, lane.ID
		raw, err := json.Marshal(steps)
		testutil.MustNoErr(t, err, "marshal the holder's steps")
		testutil.MustNoErr(t, db.UpdateOrderStepsJSON(h.ID, string(raw)), "park the holder at the dug lane's mark")
		h = prReloadUUID(t, db, hUUID)
	}
	if v.releaseHolder {
		steps, _ := decodeSteps(h.StepsJSON)
		seg, more, off := splitSegment(steps, h.WaitIndex)
		testutil.MustNoErr(t, d.appendSegmentAndAdvance(h, seg, more, off, "test release"), "release the holder")
		h = prReloadUUID(t, db, hUUID)
	}
	r.holderSteps, r.holderReadAt = h.StepsJSON, h

	gSteps := []protocol.ComplexOrderStep{prPick(grp.Name), prDrop(pressB.Name)}
	if v.diggerWaits {
		gSteps = append([]protocol.ComplexOrderStep{prWait(pressB.Name)}, gSteps...)
	}
	d.HandleComplexOrderRequest(testEnvelope(), &protocol.ComplexOrderRequest{
		OrderUUID: gUUID, PayloadCode: bp.Code, Quantity: 1, ProcessNode: pressB.Name, Steps: gSteps,
	})
	prScanPass(t, d, db, gUUID)
	prScanPass(t, d, db, gUUID)
	r.holder, r.digger = prReloadUUID(t, db, hUUID), prReloadUUID(t, db, gUUID)
	return r
}

func (r *abStrandRun) claimant(t *testing.T, binID int64) int64 {
	t.Helper()
	b, err := r.db.GetBin(binID)
	testutil.MustNoErr(t, err, "read bin")
	if b.ClaimedBy == nil {
		return 0
	}
	return *b.ClaimedBy
}

// assertNotSwapped: the claims are as the holder took them and its plan is
// untouched, and the digger waits under cause.
func (r *abStrandRun) assertNotSwapped(t *testing.T, cause QueueCause) {
	t.Helper()
	if got := r.claimant(t, r.mouthBin); got != r.holder.ID {
		t.Errorf("mouth bin %d claimed by %d, want still the holder %d", r.mouthBin, got, r.holder.ID)
	}
	if got := r.claimant(t, r.deepBin); got == r.holder.ID {
		t.Errorf("deep bin %d claimed by the holder %d: swapped", r.deepBin, r.holder.ID)
	}
	if r.holder.StepsJSON != r.holderSteps {
		t.Errorf("holder plan rewritten:\n  was %s\n  now %s", r.holderSteps, r.holder.StepsJSON)
	}
	if r.digger.QueueCause != string(cause) {
		t.Errorf("digger %d is %s under %q, want %q", r.digger.ID, r.digger.Status, r.digger.QueueCause, cause)
	}
}

// TestABStrand_ADiggerWithNoStationWaitTakesTheMouth — the swap: the digger is
// dispatched holding the mouth bin with no dig; the holder, still at its wait,
// holds the bin behind it, its pickup step and junction row re-pointed there;
// both orders' histories and both bins' audit rows name the exchange. At the
// base the digger queues under dig-blocker-claimed.
func TestABStrand_ADiggerWithNoStationWaitTakesTheMouth(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS1", abStrand{})
	h, g := r.holder, r.digger

	if g.QueueCause == string(CauseDigBlockerClaimed) {
		t.Fatalf("the cycle: digger %d is %s under %q behind the mouth bin %d, which holder %d (%s, at its "+
			"station wait) claims for a pickup its robot has not reached", g.ID, g.Status, g.QueueCause,
			r.mouthBin, h.ID, h.Status)
	}
	if got := r.claimant(t, r.mouthBin); got != g.ID {
		t.Errorf("mouth bin %d claimed by %d, want the digger %d", r.mouthBin, got, g.ID)
	}
	if got := r.claimant(t, r.deepBin); got != h.ID {
		t.Errorf("deep bin %d claimed by %d, want the holder %d", r.deepBin, got, h.ID)
	}
	if h.Status != StatusStaged || h.WaitIndex != 0 {
		t.Errorf("holder %d is %s at wait %d, want still staged at its wait", h.ID, h.Status, h.WaitIndex)
	}
	if g.Status == StatusReshuffling || protocol.IsAcquiring(g.Status) || g.ParentOrderID != nil {
		t.Errorf("digger %d is %s (%q), want dispatched with no dig", g.ID, g.Status, g.QueueCause)
	}
	if !strings.Contains(h.StepsJSON, `"`+r.deep.Name+`"`) || strings.Contains(h.StepsJSON, `"`+r.mouth.Name+`"`) {
		t.Errorf("holder plan %s, want its pickup at %s, not %s", h.StepsJSON, r.deep.Name, r.mouth.Name)
	}
	obs, err := r.db.ListOrderBins(h.ID)
	testutil.MustNoErr(t, err, "list holder order_bins")
	for _, ob := range obs {
		if ob.BinID == r.mouthBin {
			t.Errorf("holder junction still names the mouth bin: %+v", ob)
		}
		if ob.BinID == r.deepBin && ob.NodeName != r.deep.Name {
			t.Errorf("holder junction for the deep bin names %s, want %s", ob.NodeName, r.deep.Name)
		}
	}
	for _, row := range []struct {
		entity string
		id     int64
	}{{"order", h.ID}, {"order", g.ID}, {"bin", r.mouthBin}, {"bin", r.deepBin}} {
		var n int
		testutil.MustNoErr(t, r.db.DB.QueryRow(`SELECT count(*) FROM audit_log
			WHERE entity_type=$1 AND entity_id=$2 AND action='claim_exchanged'`, row.entity, row.id).Scan(&n), "audit")
		if n != 1 {
			t.Errorf("%s %d has %d claim_exchanged ledger rows, want 1", row.entity, row.id, n)
		}
	}
}

// TestABStrand_ADiggerWithAStationWaitParksUnderTheHonestCause — the finding's
// cycle at Core: the digger has its own station wait, so the swap could build
// the mirror deadlock: Core cannot see which side the Edge's gate holds. Not swapped; the digger waits under
// dig-blocker-held-at-station, which names the holder's station release.
func TestABStrand_ADiggerWithAStationWaitParksUnderTheHonestCause(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS2", abStrand{diggerWaits: true})
	r.assertNotSwapped(t, CauseDigBlockerHeldAtStation)
	if want := fmt.Sprintf("order %d", r.holder.ID); !strings.Contains(r.digger.QueueReason, want) {
		t.Errorf("digger's sentence %q does not name the holder (%s)", r.digger.QueueReason, want)
	}
}

// TestABStrand_AHolderPastItsWaitIsNotSwapped — the holder's station wait was
// released and its pickup went to the fleet under the mouth bin: its robot is
// carrying that bin out, so the wait is the claimed one and nothing moves.
func TestABStrand_AHolderPastItsWaitIsNotSwapped(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS3", abStrand{releaseHolder: true})
	r.assertNotSwapped(t, CauseDigBlockerClaimed)
}

// TestABStrand_MixedPartsAreNotSwapped — the mouth bin carries another part,
// for a holder that wants that part: neither bin serves the other order. Not
// swapped; the holder is at its station wait, so the honest cause.
func TestABStrand_MixedPartsAreNotSwapped(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS4", abStrand{mouthPart: true})
	r.assertNotSwapped(t, CauseDigBlockerHeldAtStation)
}

// TestABStrand_AHolderAtTheDugLanesMarkIsNotSwapped — the holder is parked at
// this lane's mark: it is the nearer robot, about to lift the mouth bin.
func TestABStrand_AHolderAtTheDugLanesMarkIsNotSwapped(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS5", abStrand{holderAtMark: true})
	r.assertNotSwapped(t, CauseDigBlockerClaimed)
}

// TestABStrand_ASecondBlockerIsNotSwapped — another bin stands between the
// mouth bin and the digger's: taking the mouth bin would not reach the digger's
// part with no dig, and the holder's re-pointed bin would still be buried.
func TestABStrand_ASecondBlockerIsNotSwapped(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS6", abStrand{secondBlocker: true})
	r.assertNotSwapped(t, CauseDigBlockerHeldAtStation)
}

// TestABStrand_AReleaseThatReadThePlanBeforeTheSwapSendsTheNewPickup — the
// race the exchange's two guards close. A release that read the holder's plan
// before the exchange committed must not append a segment cut from it: the CAS
// refuses the stale plan (ErrPlanChanged) and nothing reaches the fleet. The
// station release re-cuts from the stored plan and goes out once; the later
// tail carries the deep slot, never the mouth slot the digger now holds.
func TestABStrand_AReleaseThatReadThePlanBeforeTheSwapSendsTheNewPickup(t *testing.T) {
	t.Parallel()
	r := abStrandFixture(t, "ABS7", abStrand{})
	stale := *r.holderReadAt
	if r.holder.StepsJSON == stale.StepsJSON {
		t.Fatalf("fixture: no exchange happened (digger %s under %q)", r.digger.Status, r.digger.QueueCause)
	}
	steps, _ := decodeSteps(stale.StepsJSON)
	seg, more, off := splitSegment(steps, stale.WaitIndex)
	before := len(r.backend.ReleaseCalls())
	probe := stale
	if err := r.d.appendSegmentAndAdvance(&probe, seg, more, off, "stale probe"); !errors.Is(err, ErrPlanChanged) {
		t.Fatalf("stale append = %v, want ErrPlanChanged", err)
	}
	if n := len(r.backend.ReleaseCalls()) - before; n != 0 {
		t.Fatalf("the stale append reached the fleet %d time(s)", n)
	}

	release := stale
	r.d.dispatchFleetRelease(testEnvelope(), &release, seg, more, off)
	if n := len(r.backend.ReleaseCalls()) - before; n != 1 {
		t.Fatalf("station release appends = %d, want 1 (re-cut once)", n)
	}
	fresh := reloadOrder(t, r.db, r.holder.ID)
	if IsGateStaged(fresh) {
		testutil.MustNoErr(t, r.d.appendGateTail(fresh, "test lane release"), "the lane wait's tail")
	}
	var locs []string
	for _, c := range r.backend.ReleaseCalls()[before:] {
		for _, b := range c.Blocks {
			locs = append(locs, b.Location)
		}
	}
	joined := " " + strings.Join(locs, " ") + " "
	if strings.Contains(joined, " "+r.mouth.Name+" ") || !strings.Contains(joined, " "+r.deep.Name+" ") {
		t.Errorf("the fleet received %v, want the deep slot %s and never the mouth %s", locs, r.deep.Name, r.mouth.Name)
	}
}

// TestABStrand_TheExchangeRefusesAStaleHolderOrAThirdReservation — the store
// transaction re-checks under locks what the decision read: a holder written
// since (a release's CAS bumps it) or a third order's hold on the deep bin
// refuses the exchange and writes nothing.
func TestABStrand_TheExchangeRefusesAStaleHolderOrAThirdReservation(t *testing.T) {
	t.Parallel()
	for _, tc := range []string{"holder written since", "a third order holds the deep bin"} {
		t.Run(tc, func(t *testing.T) {
			t.Parallel()
			r := abStrandFixture(t, "ABS8"+strings.ReplaceAll(tc[:6], " ", ""), abStrand{diggerWaits: true})
			c, read := r.d.readExchange(r.digger, r.holderReadAt, r.lane, r.mouthBin, r.deepBin)
			ex, ok := c.exchange()
			if !read || !ok {
				t.Fatalf("fixture: the exchange's decision refused before the store: %+v", ex)
			}
			switch tc {
			case "holder written since":
				_, err := r.db.ClaimAppend(r.holder.ID, r.holder.WaitIndex, r.holder.StepsJSON)
				testutil.MustNoErr(t, err, "a release's CAS lands")
			default:
				third := testdb.CreateOrder(t, r.db, func(o *orders.Order) { o.Status = StatusQueued })
				testdb.ReserveBin(t, r.db, third.ID, r.deepBin)
			}
			done, err := r.db.ExchangeHeldPickup(ex)
			if done || !errors.Is(err, store.ErrExchangeRefused) {
				t.Fatalf("exchange = %t (%v), want refused", done, err)
			}
			if got := r.claimant(t, r.mouthBin); got != r.holder.ID {
				t.Errorf("mouth bin claimed by %d, want still the holder %d", got, r.holder.ID)
			}
		})
	}
}
