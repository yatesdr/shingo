package engine

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
	"shingoedge/store/processes"
)

// operator_bin_ops_bare_test.go — what ClearBin sends Core as bin_type_code at
// a Core-owned unloader window (no stored claim; the doors run on SynthClaim),
// and what it costs in Core round trips.

// clearCaptureCore serves a full at every node-bins read and records every
// bin-clear body and every request path.
type clearCaptureCore struct {
	mu     sync.Mutex
	clears []map[string]string
	paths  []string
	srv    *httptest.Server
}

func newClearCaptureCore(t *testing.T) *clearCaptureCore {
	t.Helper()
	c := &clearCaptureCore{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.paths = append(c.paths, r.URL.Path)
		switch r.URL.Path {
		case "/api/telemetry/node-bins":
			_ = json.NewEncoder(w).Encode([]NodeBinInfo{{
				NodeName: r.URL.Query().Get("nodes"), Occupied: true, PayloadCode: "PART-HL", BinID: 91,
			}})
		case "/api/telemetry/bin-clear":
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			c.clears = append(c.clears, body)
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "bin_id": 91, "delta_epoch": 2})
		default:
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
		}
	}))
	t.Cleanup(c.srv.Close)
	return c
}

func (c *clearCaptureCore) snapshot() ([]map[string]string, []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]string(nil), c.clears...), append([]string(nil), c.paths...)
}

// coreUnloaderWindow seeds a Core-owned shared_window loader over one window
// with no stored claim, and returns the window's process-node id.
func coreUnloaderWindow(t *testing.T, eng *Engine, window string, info protocol.LoaderInfo) int64 {
	t.Helper()
	procID, err := eng.db.CreateProcess(window+"-PROC", "", "active_production", "", "", false)
	testutil.MustNoErr(t, err, "create process")
	nodeID, err := eng.db.CreateProcessNode(processes.NodeInput{
		ProcessID: procID, CoreNodeName: window, Code: "W1", Name: window, Sequence: 1, Enabled: true,
	})
	testutil.MustNoErr(t, err, "create node")
	seedCoreLoader(t, eng, info)
	if _, _, claim, err := eng.loadActiveNode(nodeID); err != nil || claim == nil || claim.ID != 0 {
		t.Fatalf("fixture: want a synthesized claim at %s, got %+v (err %v)", window, claim, err)
	}
	return nodeID
}

// TestClearBin_BinTypeCodeAtACoreOwnedUnloader: a blank code reaches Core as
// no bin_type_code at all, and an explicit code is sent as given. At an
// unloader with a bare type, a blank code sends the bare type and an explicit
// code still wins. A produce loader never substitutes. Every case costs one
// node-bins read (consume) and one bin-clear POST: the bare type comes from
// the loader snapshot, not from Core.
func TestClearBin_BinTypeCodeAtACoreOwnedUnloader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, window, role, bare, code string
		want                           string
		wantKey                        bool
		wantReads                      int
	}{
		{name: "blank code", window: "HLC-B", role: "consume", code: "", want: "", wantKey: false, wantReads: 1},
		{name: "explicit code", window: "HLC-X", role: "consume", code: "TOTE-S", want: "TOTE-S", wantKey: true, wantReads: 1},
		{name: "blank code at a bare unloader", window: "HLC-BB", role: "consume", bare: "HALF-TOTE", code: "", want: "HALF-TOTE", wantKey: true, wantReads: 1},
		{name: "explicit code at a bare unloader wins", window: "HLC-BX", role: "consume", bare: "HALF-TOTE", code: "TOTE-S", want: "TOTE-S", wantKey: true, wantReads: 1},
		// A produce loader's CLEAR reads no node-bins and never substitutes.
		{name: "blank code at a produce loader with a bare type", window: "HLC-PB", role: "produce", bare: "HALF-TOTE", code: "", want: "", wantKey: false, wantReads: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			eng := testEngine(t, testEngineDB(t))
			core := newClearCaptureCore(t)
			eng.coreClient = NewCoreClient(core.srv.URL)
			info := sharedLoaderInfo(tc.window, tc.role, "operator", "PART-HL", 0, 0)
			info.InboundSource = ""
			info.OutboundDest = "EMPTY-TOTES"
			info.BareBinTypeCode = tc.bare
			nodeID := coreUnloaderWindow(t, eng, tc.window, info)

			testutil.MustNoErr(t, eng.ClearBin(nodeID, tc.code), "ClearBin")

			clears, paths := core.snapshot()
			if len(clears) != 1 {
				t.Fatalf("bin-clear POSTs = %d, want 1", len(clears))
			}
			got, hasKey := clears[0]["bin_type_code"]
			if hasKey != tc.wantKey || got != tc.want {
				t.Errorf("bin-clear bin_type_code = %q (present %v), want %q (present %v)", got, hasKey, tc.want, tc.wantKey)
			}
			var reads int
			for _, p := range paths {
				if p == "/api/telemetry/node-bins" {
					reads++
				}
			}
			if reads != tc.wantReads {
				t.Errorf("node-bins reads = %d, want %d (paths %v)", reads, tc.wantReads, paths)
			}
		})
	}
}
