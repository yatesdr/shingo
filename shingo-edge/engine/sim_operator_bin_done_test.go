//go:build sim

package engine

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingo/protocol/testutil"
	"shingoedge/config"
	"shingoedge/orders"
	"shingoedge/store/catalog"
)

// The sim operator stands in for a person, and a person exchanges a same-part
// bin when it is done: consume at 0, or at or under the lineside pull with the
// rest pulled lineside; produce at capacity. Until then it leaves the swap
// staged and looks again on its sweep. A changeover's legs go as before.
// Production does not gate a release on this (owner, 2026-10-12).
//
// RED before the rule: the operator pressed 3 s after staged, parts or not.

// binDoneWorld is machineFixture's cell (consume ALN_003 PANEL-B pack 30, pull
// at 5% = 2; produce ALN_005 ASSY, capacity 20 in the catalog) with a fake Core reporting
// `tracked` on the consume carrier, a staged swap leg on node `nodeID`, and an
// operator that reacts at once. It counts the operator's release attempts.
type binDoneWorld struct {
	f        *machineFixture
	op       *simOperator
	orderID  int64
	mu       sync.Mutex
	attempts int
}

func newBinDoneWorld(t *testing.T, tracked int, produce bool) *binDoneWorld {
	t.Helper()
	f := newMachineFixture(t)
	testutil.MustNoErr(t, f.db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 1, Name: "PANEL-B", Code: "PANEL-B", UOPCapacity: 30,
	}), "catalog PANEL-B")
	testutil.MustNoErr(t, f.db.UpsertPayloadCatalog(&catalog.CatalogEntry{
		ID: 2, Name: "ASSY", Code: "ASSY", UOPCapacity: 20,
	}), "catalog ASSY")
	eng := testEngine(t, f.db)
	core := newSCCore(t)
	eng.coreClient = stubCoreClient(core.srv.URL)
	core.setCount("ALN_003", true, "PANEL-B", tracked)

	w := &binDoneWorld{f: f}
	count := func(format string, a ...any) {
		if strings.Contains(fmt.Sprintf(format, a...), "operator auto-release order") {
			w.mu.Lock()
			w.attempts++
			w.mu.Unlock()
		}
	}
	eng.logFn, eng.debugFn = count, count

	nodeID, delivery := f.consumeID, "ALN_003"
	if produce {
		nodeID, delivery = f.produceID, "ALN_005"
	}
	id, err := f.db.CreateOrder("bin-done-swap", orders.TypeComplex, &nodeID, false, 1,
		delivery, "", "SYN_MARKET", "", false, "", "", "")
	testutil.MustNoErr(t, err, "create the swap leg")
	_, err = f.db.Exec(`UPDATE orders SET steps_json = '[{"action":"wait"}]' WHERE id = ?`, id)
	testutil.MustNoErr(t, err, "the leg's step plan (an Edge-authored leg is not lane-held)")
	testutil.MustNoErr(t, f.db.UpdateOrderStatus(id, string(protocol.StatusStaged)), "stage it")
	w.orderID = id

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	w.op = &simOperator{
		e: eng, clk: clock.Real(), ctx: ctx,
		ops:     config.SimOperatorsConfig{SwapRelease: time.Millisecond, LinesidePullPercent: 5},
		pending: map[int64]bool{}, releasing: map[int64]bool{}, confirming: map[int64]bool{},
	}
	return w
}

// pressed runs the operator's release worker once and reports whether it
// pressed RELEASE (accepted or refused by the door: either way it pressed).
func (w *binDoneWorld) pressed(t *testing.T) bool {
	t.Helper()
	if o, err := w.f.db.GetOrder(w.orderID); err != nil || o.LaneHeld {
		t.Fatalf("fixture: the swap leg is unreadable or lane-held (%v); the test checks nothing", err)
	}
	w.mu.Lock()
	before := w.attempts
	w.mu.Unlock()
	w.op.runRelease(w.orderID)
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.attempts > before
}

func TestSimOperator_ReleasesASamePartSwapWhenTheBinIsDone(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name    string
		tracked int
		produce bool
		count   int  // the produce seat's count
		noBin   bool // the produce seat holds no bin
		want    bool
	}{
		{name: "consume with parts left waits", tracked: 15, want: false},
		{name: "consume above the pull waits", tracked: 3, want: false},
		{name: "consume at the pull goes, the rest pulled lineside", tracked: 2, want: true},
		{name: "consume at 0 goes", tracked: 0, want: true},
		{name: "produce below capacity waits", produce: true, count: 19, want: false},
		{name: "produce at capacity goes", produce: true, count: 20, want: true},
		// The pair's evac took the full away; the supply bringing the empty
		// stages on a seat with no bin. Nothing is filling, so nothing waits.
		{name: "produce with no bin on the seat goes", produce: true, count: 0, noBin: true, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			w := newBinDoneWorld(t, c.tracked, c.produce)
			if c.produce {
				bin := int64(39)
				if c.noBin {
					bin = 0
				}
				w.f.setRuntime(t, w.f.produceID, produceClaimID(t, w.f), bin, c.count)
			}
			if got := w.pressed(t); got != c.want {
				t.Errorf("pressed RELEASE = %v, want %v", got, c.want)
			}
		})
	}
}

// A changeover's legs are not same-part swaps: the operator presses them as
// before, parts or not.
func TestSimOperator_AChangeoverLegGoesWithPartsLeft(t *testing.T) {
	t.Parallel()
	w := newBinDoneWorld(t, 15, false)
	toStyle, err := w.f.db.CreateStyle("MACHINE-STYLE-B", "", w.f.processID)
	testutil.MustNoErr(t, err, "the to-style")
	_, err = w.f.db.Exec(`INSERT INTO process_changeovers (process_id, to_style_id, state, called_by)
		VALUES (?, ?, 'active', 'test')`, w.f.processID, toStyle)
	testutil.MustNoErr(t, err, "start a changeover")
	if !w.pressed(t) {
		t.Error("a changeover leg with 15 left was left staged; want it pressed as before")
	}
}

// produceClaimID is the stored claim id of the fixture's produce node.
func produceClaimID(t *testing.T, f *machineFixture) int64 {
	t.Helper()
	var id int64
	testutil.MustNoErr(t, f.db.DB.QueryRow(
		`SELECT id FROM style_node_claims WHERE style_id = ? AND core_node_name = 'ALN_005'`, f.styleID).Scan(&id),
		"produce claim id")
	return id
}
