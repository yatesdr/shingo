package engine

import (
	"strings"
	"testing"

	"shingo/protocol"
)

// TestPin_LoaderRefusesALabelledCarrierAtZero: a final release at 1, one tick
// before the pickup, and the carrier comes back to its loader still labelled
// but reading 0 (RULINGS-C2b-rebase6-and-loader-orphan §3). LOAD refuses it and
// tells the operator what will free it: confirm it is empty, CLEAR it, then
// load. It used to name an outbound move that never comes; row L item 5 changed
// the message, not the check. The Core half (the pool refuses the same carrier
// both ways) is shingo-core dispatch/binsource
// TestPin_LabelledCarrierAtZeroIsNeitherStockNorContainer.
func TestPin_LoaderRefusesALabelledCarrierAtZero(t *testing.T) {
	t.Parallel()
	const window = "SC-LZ-W1"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "SHIM", 0)

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil {
		t.Fatal("LoadBin accepted a labelled carrier at 0; it is refused until the operator clears it")
	}
	if !strings.Contains(err.Error(), "confirm it is empty, CLEAR it, then load") {
		t.Errorf("LoadBin refusal = %q, want it to name the clear", err)
	}
	if strings.Contains(err.Error(), "wait for outbound move") {
		t.Errorf("LoadBin refusal = %q names an outbound move that never comes", err)
	}
	if n := core.clearCount(); n != 0 {
		t.Errorf("LoadBin sent %d clear(s); the Edge's LOAD clears nothing", n)
	}
}

// A labelled carrier WITH units is stock: it still waits for its outbound move.
func TestPin_LoaderRefusesALabelledCarrierWithUnits(t *testing.T) {
	t.Parallel()
	const window = "SC-LZ-W2"
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	core.setCount(window, true, "SHIM", 5)

	err := eng.LoadBin(nodeID, "", nil, scManifest)
	if err == nil || !strings.Contains(err.Error(), "already loaded with SHIM — wait for outbound move") {
		t.Errorf("LoadBin refusal = %v, want the already-loaded refusal", err)
	}
}

// scHomeEngine is scLoaderEngine's window made a home: its loader sends what it
// loads nowhere (no outbound), as a supermarket home's.
func scHomeEngine(t *testing.T, window string) (*Engine, *scCore, int64) {
	t.Helper()
	eng, _, core, _, nodeID := scLoaderEngine(t, window)
	home := sharedLoaderInfo(window, "produce", "operator", "PART-SC", 0, 0)
	home.OutboundDest = ""
	seedCoreLoader(t, eng, home)
	return eng, core, nodeID
}

// loadsSent is every bin-load request the fake Core has been sent.
func loadsSent(core *scCore) []BinLoadRequest {
	core.mu.Lock()
	defer core.mu.Unlock()
	return append([]BinLoadRequest(nil), core.loads...)
}

// TestPin_ALoadOnAPartialOfItsPartReplacesIt: the floor takes a partial tote
// off a home and sets a full one on, then taps LOAD (REPLACE PARTIAL). The
// Edge sends Core one bin-load expect=replace naming the carrier it read (bin
// 77, epoch 5) and the declared count, and seats Core's answer. At a home no
// L2 is filed; at a window with an outbound, one. It used to read the partial
// as a lost reply and refuse "wait for outbound move", a move a home never
// makes (SPR SMN_029, 2026-11-17).
func TestPin_ALoadOnAPartialOfItsPartReplacesIt(t *testing.T) {
	t.Parallel()
	full := int64(100)
	wantReplace := func(t *testing.T, core *scCore) {
		t.Helper()
		loads := loadsSent(core)
		if len(loads) != 1 {
			t.Fatalf("bin-load requests = %d, want 1", len(loads))
		}
		l := loads[0]
		if l.Expect != protocol.LoadExpectReplace || l.BinID != 77 || l.Epoch != 5 || l.PayloadCode != "PART-SC" ||
			l.UOPCount == nil || *l.UOPCount != full {
			t.Errorf("bin-load = expect %q bin %d epoch %d part %q count %v, want replace, 77, 5, PART-SC, 100",
				l.Expect, l.BinID, l.Epoch, l.PayloadCode, l.UOPCount)
		}
	}
	t.Run("at a home", func(t *testing.T) {
		t.Parallel()
		const window = "SC-RP-HOME"
		eng, core, nodeID := scHomeEngine(t, window)
		core.setPartial(window, "PART-SC", 30, 100)
		if err := eng.LoadBin(nodeID, "", &full, scManifest); err != nil {
			t.Fatalf("LoadBin on a partial of its part: %v; it replaces it", err)
		}
		wantReplace(t, core)
		if n := len(scMovesFrom(t, eng.db, window)); n != 0 {
			t.Errorf("outbound moves from the home = %d, want 0: a home sends nothing out", n)
		}
	})
	t.Run("with an outbound", func(t *testing.T) {
		t.Parallel()
		const window = "SC-RP-OUT"
		eng, db, core, _, nodeID := scLoaderEngine(t, window)
		core.setPartial(window, "PART-SC", 30, 100)
		if err := eng.LoadBin(nodeID, "PART-SC", &full, scManifest); err != nil {
			t.Fatalf("LoadBin on a partial of its part: %v; it replaces it", err)
		}
		wantReplace(t, core)
		if n := len(scMovesFrom(t, db, window)); n != 1 {
			t.Errorf("outbound moves = %d, want 1: the replaced carrier leaves as any LOAD's", n)
		}
	})
}

// TestPin_AHomeSaysWhatStandsThere: a LOAD a home refuses names what stands
// on it, never an outbound move it does not make: another part than the one
// asked, or a full bin of its part. Nothing is sent to Core. Core's own
// refusal of a replace is shown as its sentence.
func TestPin_AHomeSaysWhatStandsThere(t *testing.T) {
	t.Parallel()
	full := int64(100)
	for _, tc := range []struct {
		name, part string
		uop        int
		refuse     string
		want       string
		loads      int
	}{
		{name: "another part", part: "PART-OTHER", uop: 30, want: "SC-HS-W holds PART-SC, not PART-OTHER"},
		{name: "a full bin of its part", uop: 100, want: "SC-HS-W already holds a full bin of PART-SC"},
		{name: "Core refuses the replace", uop: 30, loads: 1,
			refuse: "the carrier changed since you looked — it now holds CARRIER-1 (PART-SC)",
			want:   "the carrier changed since you looked — it now holds CARRIER-1 (PART-SC)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const window = "SC-HS-W"
			eng, core, nodeID := scHomeEngine(t, window)
			core.setPartial(window, "PART-SC", tc.uop, 100)
			core.mu.Lock()
			core.refuseLoad = tc.refuse
			core.mu.Unlock()
			err := eng.LoadBin(nodeID, tc.part, &full, scManifest)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadBin = %v, want a refusal saying %q", err, tc.want)
			}
			if strings.Contains(err.Error(), "wait for outbound move") {
				t.Errorf("LoadBin refusal = %q names an outbound move a home never makes", err)
			}
			if n := len(loadsSent(core)); n != tc.loads {
				t.Errorf("bin-load requests = %d, want %d", n, tc.loads)
			}
		})
	}
}
