package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// fold_pins_test.go — what one single-window auto_push stage-1 cycle costs Core.
// At f9a854cb it was 4 node-bins reads, three of which pulled nothing: CLEAR's
// hadBin pre-read, the CLEAR gate, and a produce release's read while the next
// full was already in flight. Only the pickup gate pulls. The node-bins door is
// gone and the U1 is Core's to order, so the cost is in loader retrieve asks:
// the CLEAR and release gates check the order table first
// (unloaderPullsCovered) and ask nothing when it says Core would order nothing.

// TestPinFold_SingleWindowCycleReads: full on N → CLEAR → U2 lifts the carrier
// (the pickup gate asks for the next full) → an upstream produce release of the
// same payload → the U2 lands. One ask in the cycle, the pickup's, and no
// node-bins read. At f9a854cb: CLEAR 2, pickup 1, release 1, landing 0 reads = 4.
func TestPinFold_SingleWindowCycleReads(t *testing.T) {
	t.Parallel()
	f := newCycleUnloader(t, "FLD")
	f.deliveredFullAt(t, f.n, f.nCore, "PART-AC")
	total := 0
	step := func(name string, want int, do func()) {
		t.Helper()
		before, asked := f.core.nodeBinReads(), f.asks.asks.Load()
		do()
		awaitAsks(t, f.eng)
		if reads := f.core.nodeBinReads() - before; reads != 0 {
			t.Errorf("%s: node-bins reads = %d, want 0", name, reads)
		}
		got := int(f.asks.asks.Load() - asked)
		total += got
		if got != want {
			t.Errorf("%s: loader retrieve asks = %d, want %d", name, got, want)
		}
	}

	step("CLEAR", 0, func() { testutil.MustNoErr(t, f.eng.ClearBin(f.n, "", false, nil), "ClearBin") })
	u2 := f.onlyU2(t)
	step("pickup", 1, func() { f.pickUp(t, u2) })
	if got := f.fulls(t, f.nCore); got != 1 {
		t.Fatalf("after the pickup: U1s = %d, want 1", got)
	}
	step("produce release", 0, func() { f.eng.MaybeCreateUnloaderFullIn("PART-AC") })
	if got := f.fulls(t, f.nCore); got != 1 {
		t.Errorf("after the release: U1s = %d, want still 1 (the one in flight covers it)", got)
	}
	step("landing", 0, func() { scLand(t, f.eng, f.db, u2) })
	if total != 1 {
		t.Errorf("loader retrieve asks per cycle = %d, want 1", total)
	}
}

// TestPinFold_SingleWindowPushEmptyReads: PUSH EMPTY at a single-window
// auto_push unloader. The tap reads the window from the bins table (it must
// refuse a full carrier; site 5 of the reads, no node-bins read since); the gate
// counts the tapped window held, is covered, and asks nothing. At f9a854cb the
// gate read Core too and pulled nothing — the carrier is resident.
func TestPinFold_SingleWindowPushEmptyReads(t *testing.T) {
	t.Parallel()
	f := newCycleUnloader(t, "FLP")
	f.core.set(f.nCore, true, "")
	before := f.core.nodeBinReads()
	testutil.MustNoErr(t, f.eng.PushEmptyOut(f.n), "PushEmptyOut")
	if reads := f.core.nodeBinReads() - before; reads != 0 {
		t.Errorf("PUSH EMPTY: node-bins reads = %d, want 0 (the tap reads the table; the gate is covered locally)", reads)
	}
	if got := f.fulls(t, f.nCore); got != 0 {
		t.Errorf("PUSH EMPTY: U1s = %d, want 0 (carrier resident)", got)
	}
	if n := f.asks.asks.Load(); n != 0 {
		t.Errorf("PUSH EMPTY: loader retrieve asks = %d, want 0 (covered locally)", n)
	}
}

// TestPinFold_ProduceRelease: MaybeCreateUnloaderFullIn for one payload.
// Covered (a full-in of that payload already in flight): no ask, nothing ordered
// (at f9a854cb it read Core and fired nothing). Uncovered: one ask, and Core
// orders the full.
func TestPinFold_ProduceRelease(t *testing.T) {
	t.Parallel()
	t.Run("covered", func(t *testing.T) {
		t.Parallel()
		f := newCycleUnloader(t, "FRC")
		n := f.n
		id, err := f.db.CreateOrder("frc-u1", "retrieve", &n, false, 1, f.nCore, "", "FG-SUPER", "", false, "PART-AC", "", "")
		testutil.MustNoErr(t, err, "in-flight U1")
		testutil.MustNoErr(t, f.db.UpdateOrderStatus(id, string(protocol.StatusDispatched)), "dispatch")
		f.core.set(f.nCore, false, "")
		before := f.core.nodeBinReads()
		f.eng.MaybeCreateUnloaderFullIn("PART-AC")
		if reads := f.core.nodeBinReads() - before; reads != 0 {
			t.Errorf("covered release: node-bins reads = %d, want 0", reads)
		}
		if got := f.fulls(t, f.nCore); got != 1 {
			t.Errorf("covered release: U1s = %d, want still 1", got)
		}
		if n := f.asks.asks.Load(); n != 0 {
			t.Errorf("covered release: loader retrieve asks = %d, want 0", n)
		}
	})
	t.Run("uncovered", func(t *testing.T) {
		t.Parallel()
		f := newCycleUnloader(t, "FRU")
		f.core.set(f.nCore, false, "")
		before := f.core.nodeBinReads()
		f.eng.MaybeCreateUnloaderFullIn("PART-AC")
		if reads := f.core.nodeBinReads() - before; reads != 0 {
			t.Errorf("uncovered release: node-bins reads = %d, want 0 (the release asks Core)", reads)
		}
		if got := f.fulls(t, f.nCore); got != 1 {
			t.Errorf("uncovered release: U1s = %d, want 1", got)
		}
		if n := f.asks.asks.Load(); n != 1 {
			t.Errorf("uncovered release: loader retrieve asks = %d, want 1", n)
		}
	})
}

// clearShapeCore is a Core stand-in for ClearBin's two calls, with the shape of
// each answer chosen by the test: node-bins can fail (500), and bin-clear
// answers a fixed body.
type clearShapeCore struct {
	mu        sync.Mutex
	nodeBins  int
	failBins  bool
	clearBody map[string]any
	srv       *httptest.Server
}

func (c *clearShapeCore) reads() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nodeBins
}

func newClearShapeCore(t *testing.T, failBins bool, clearBody map[string]any) *clearShapeCore {
	t.Helper()
	c := &clearShapeCore{failBins: failBins, clearBody: clearBody}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			c.nodeBins++
			if c.failBins {
				http.Error(w, "boom", http.StatusInternalServerError)
				return
			}
			var out []NodeBinInfo
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				out = append(out, NodeBinInfo{NodeName: n, Occupied: true, PayloadCode: "PART-MX", BinID: 5})
			}
			_ = json.NewEncoder(w).Encode(out)
		case "/api/telemetry/bin-clear":
			_ = json.NewEncoder(w).Encode(c.clearBody)
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// TestPinFold_ClearAgainstTodaysCoreResponse: the bin-clear answer as a Core that
// predates cleared_payload_code sends it (status, bin_id, bin_label, delta_epoch).
// The U2 is still created — hadBin comes from the clear succeeding, not from any
// new field — and the Edge reads no node-bins.
func TestPinFold_ClearAgainstTodaysCoreResponse(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "FMX", "consume", "PART-MX", "EMPTY-TOTES")
	c := newClearShapeCore(t, false, map[string]any{"status": "ok", "bin_id": 5, "bin_label": "B5", "delta_epoch": 2})
	eng := testEngine(t, db)
	bridgeCore(t, eng, c.srv.URL)
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "", false, nil), "ClearBin")
	if n, _ := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 1 {
		t.Errorf("U2s after a CLEAR against today's Core answer = %d, want 1", n)
	}
	if got := c.reads(); got != 0 {
		t.Errorf("node-bins reads = %d, want 0 (the pre-read is gone)", got)
	}
}

// TestPinFold_ClearWhenThePreReadFails: node-bins would fail (Core answers 500)
// while the clear itself commits. The U2 is created. At f9a854cb the pre-read's
// failure read as "no bin", so NO U2 was created for a carrier Core had just
// cleared — it stranded at the window until someone tapped PUSH EMPTY.
func TestPinFold_ClearWhenThePreReadFails(t *testing.T) {
	t.Parallel()
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "FPF", "consume", "PART-MX", "EMPTY-TOTES")
	c := newClearShapeCore(t, true, map[string]any{"status": "ok", "bin_id": 5, "bin_label": "B5", "delta_epoch": 2})
	eng := testEngine(t, db)
	bridgeCore(t, eng, c.srv.URL)
	testutil.MustNoErr(t, eng.ClearBin(nodeID, "", false, nil), "ClearBin")
	if n, _ := countMovesTo(t, db, nodeID, "EMPTY-TOTES"); n != 1 {
		t.Errorf("U2s after a CLEAR with node-bins failing = %d, want 1", n)
	}
}
