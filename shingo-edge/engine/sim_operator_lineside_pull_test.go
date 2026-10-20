//go:build sim

package engine

import (
	"testing"
	"time"

	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/store/catalog"
)

// The sim operator pulls lineside, as the floor does (owner, 2026-10-10;
// BRIEF-unification-C2c-and-handoff Lane 0 item 2): at a consume release with
// the tracked count at or under LinesidePullPercent of the pack (rounded up),
// the remainder goes to the lineside pile through the operator's capture door
// and the bin is released empty; above it, a count release as before. The
// machine keeps cycling on the pile while its seat's carrier is at 0 or gone.

// pullFixture is machineFixture's consume node (ALN_003, PANEL-B, pack 30)
// with a fake Core reporting `tracked` on the carrier and the sim operator set
// to pull at `pct` percent.
func pullFixture(t *testing.T, tracked, pct int) (*simOperator, *machineFixture) {
	t.Helper()
	f := newMachineFixture(t)
	testutil.MustNoErr(t, f.db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "PANEL-B", Code: "PANEL-B", UOPCapacity: 30,
	}), "catalog PANEL-B")
	eng := testEngine(t, f.db)
	core := newSCCore(t)
	eng.coreClient = stubCoreClient(core.srv.URL)
	core.setCount("ALN_003", true, "PANEL-B", tracked)
	op := newTestSimOperator(clock.NewManual(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)))
	op.e = eng
	op.ops.LinesidePullPercent = pct
	return op, f
}

func TestSimOperator_PullsTheLastFivePercentLineside(t *testing.T) {
	t.Parallel()
	// ceil(5% of 30) = 2: a count of 1 or 2 is pulled; 3 is released with its count.
	for _, tracked := range []int{1, 2} {
		op, f := pullFixture(t, tracked, 5)
		disp, ok := op.consumeDisposition(f.consumeID)
		if !ok {
			t.Fatalf("tracked %d: not a consume node", tracked)
		}
		if disp.Mode != DispositionCaptureLineside {
			t.Errorf("tracked %d: mode %q, want %q (pull the remainder lineside, release empty)",
				tracked, disp.Mode, DispositionCaptureLineside)
			continue
		}
		if got := disp.LinesideCapture["PANEL-B"]; got != tracked || len(disp.LinesideCapture) != 1 {
			t.Errorf("tracked %d: capture %v, want {PANEL-B: %d}", tracked, disp.LinesideCapture, tracked)
		}
	}
}

func TestSimOperator_AboveThePullReleasesWithItsCount(t *testing.T) {
	t.Parallel()
	op, f := pullFixture(t, 3, 5)
	if disp, _ := op.consumeDisposition(f.consumeID); disp.Mode != DispositionSendPartialBack {
		t.Errorf("tracked 3 of 30 at 5%%: mode %q, want %q", disp.Mode, DispositionSendPartialBack)
	}
}

func TestSimOperator_NoPullWhenTheKnobIsZero(t *testing.T) {
	t.Parallel()
	op, f := pullFixture(t, 1, 0)
	if disp, _ := op.consumeDisposition(f.consumeID); disp.Mode != DispositionSendPartialBack {
		t.Errorf("tracked 1 with no pull configured: mode %q, want %q (today's count release)",
			disp.Mode, DispositionSendPartialBack)
	}
}

func TestSimOperator_AnEmptyCarrierIsStillReleasedUnderpack(t *testing.T) {
	t.Parallel()
	op, f := pullFixture(t, 0, 5)
	if disp, _ := op.consumeDisposition(f.consumeID); disp.Mode != DispositionReleaseUnderpack {
		t.Errorf("tracked 0: mode %q, want %q", disp.Mode, DispositionReleaseUnderpack)
	}
}

func TestSimMachineReady_ConsumeAtZeroRunsOnThePile(t *testing.T) {
	t.Parallel()
	f := newMachineFixture(t)
	f.setRuntime(t, f.consumeID, consumeClaimID(t, f), 23, 0)
	if SimMachineReady(f.db.DB, f.processID, f.styleID, false) {
		t.Fatal("control: a consume seat at 0 with no pile read as ready")
	}
	_, err := f.db.CaptureLinesideBucket(f.consumeID, "PANEL-B", 2)
	testutil.MustNoErr(t, err, "pile 2 PANEL-B")
	if !SimMachineReady(f.db.DB, f.processID, f.styleID, false) {
		t.Error("a consume seat at 0 with 2 on the pile read as NOT ready: the weld runs on the bucket")
	}
}

func TestSimMachineReady_ConsumeWithNoCarrierRunsOnThePile(t *testing.T) {
	t.Parallel()
	f := newMachineFixture(t)
	f.setRuntime(t, f.consumeID, consumeClaimID(t, f), 0, 0)
	_, err := f.db.CaptureLinesideBucket(f.consumeID, "PANEL-B", 2)
	testutil.MustNoErr(t, err, "pile 2 PANEL-B")
	if !SimMachineReady(f.db.DB, f.processID, f.styleID, false) {
		t.Error("a consume seat with no carrier and 2 on the pile read as NOT ready")
	}
}

func TestSimMachineReady_AnotherPartsPileDoesNotFeedTheSeat(t *testing.T) {
	t.Parallel()
	f := newMachineFixture(t)
	f.setRuntime(t, f.consumeID, consumeClaimID(t, f), 0, 0)
	_, err := f.db.CaptureLinesideBucket(f.consumeID, "OTHER-PART", 2)
	testutil.MustNoErr(t, err, "pile 2 OTHER-PART")
	if SimMachineReady(f.db.DB, f.processID, f.styleID, false) {
		t.Error("a pile of another part fed a PANEL-B seat")
	}
}

// consumeClaimID is the stored claim id of the fixture's consume node.
func consumeClaimID(t *testing.T, f *machineFixture) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, f.db.DB.QueryRow(
		`SELECT id FROM style_node_claims WHERE style_id = ? AND core_node_name = 'ALN_003'`, f.styleID).Scan(&id),
		"consume claim id")
	return id
}
