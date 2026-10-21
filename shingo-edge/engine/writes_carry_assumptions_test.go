package engine

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"shingo/protocol"
	"shingo/protocol/testutil"
)

// writes_carry_assumptions_test.go — every Edge write says what it read
// (bin_id, epoch, expect), and a refusal's rows redraw the tile before its
// sentence is shown (the one-transport contract, protocol/bins_feed.go).

// assumeCore is a Core that records every write's body by path and answers
// each with the reply its path's function gives (an ok reply by default).
type assumeCore struct {
	mu     sync.Mutex
	srv    *httptest.Server
	bodies map[string][]map[string]any
	reply  map[string]func(n int, body map[string]any) (int, any)
}

func newAssumeCore(t *testing.T) *assumeCore {
	t.Helper()
	c := &assumeCore{bodies: map[string][]map[string]any{}, reply: map[string]func(int, map[string]any) (int, any){}}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		c.mu.Lock()
		c.bodies[r.URL.Path] = append(c.bodies[r.URL.Path], body)
		n := len(c.bodies[r.URL.Path])
		fn := c.reply[r.URL.Path]
		c.mu.Unlock()
		code, out := http.StatusOK, any(map[string]any{"status": "ok", "bin_id": 4242, "delta_epoch": 9})
		if fn != nil {
			code, out = fn(n, body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(out)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// sent is the bodies of the writes to path, in order.
func (c *assumeCore) sent(path string) []map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]map[string]any(nil), c.bodies[path]...)
}

// assumeEngine is an engine at a consume manual_swap node ASM-MSWAP-NODE,
// bound to bin 4242 at epoch 9, against an assumeCore.
func assumeEngine(t *testing.T) (*Engine, *assumeCore, int64) {
	t.Helper()
	core := newAssumeCore(t)
	db := testEngineDB(t)
	nodeID, _ := seedManualSwapClaim(t, db, "ASM", "consume", "PART-AS", "EMPTY-TOTES")
	eng := testEngine(t, db)
	eng.SetInventoryDeltaSink(&fakeDeltaSink{db: db})
	eng.coreClient = stubCoreClient(core.srv.URL)
	rt, err := db.EnsureProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "ensure runtime")
	bin := int64(4242)
	testutil.MustNoErr(t, db.SetProcessNodeRuntimeWithBinAndEpoch(nodeID, rt.ActiveClaimID, &bin, 9, 25), "bind the carrier")
	return eng, core, nodeID
}

// holdBoard gives the engine a bins copy showing row at the node.
func holdBoard(t *testing.T, eng *Engine, rows ...protocol.NodeBinsRow) {
	t.Helper()
	testutil.MustNoErr(t, eng.ApplyNodeBinsSnapshot(protocol.NodeBinsSnapshot{Digest: "d-board", Rows: rows}), "hold the copy")
}

func occupiedRow(node string, binID, epoch int64) protocol.NodeBinsRow {
	return protocol.NodeBinsRow{Node: node, State: protocol.BinsStateOccupied, Occupied: true, BinID: binID,
		BinLabel: "AS-BIN", Epoch: epoch, Stocked: true}
}

// binIDSent is the bin_id a write's body carried; 0 when the key was absent.
func binIDSent(body map[string]any) int64 {
	v, _ := body["bin_id"].(float64)
	return int64(v)
}

// TestWrites_CountAndClearNameTheBinOnTheTile: the count and the CLEAR name the
// carrier the station's tile shows (the bins copy's row), so Core refuses them
// when another stands there instead of writing that one.
func TestWrites_CountAndClearNameTheBinOnTheTile(t *testing.T) {
	t.Parallel()
	eng, core, nodeID := assumeEngine(t)
	holdBoard(t, eng, occupiedRow("ASM-MSWAP-NODE", 4242, 9))

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 20, "op"), "RecordBinCount")
	_ = eng.ClearBin(nodeID, "", false, nil)

	for _, path := range []string{"/api/telemetry/bin-count", "/api/telemetry/bin-clear"} {
		got := core.sent(path)
		if len(got) == 0 || binIDSent(got[0]) != 4242 {
			t.Errorf("%s bodies = %v, want the first naming bin 4242, the tile's", path, got)
		}
	}
}

// TestWrites_WithNoCopyHeldNameNoBin: with no bins copy held there is no tile
// row to name, and the writes send no bin_id, as before.
func TestWrites_WithNoCopyHeldNameNoBin(t *testing.T) {
	t.Parallel()
	eng, core, nodeID := assumeEngine(t)

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 20, "op"), "RecordBinCount")
	_ = eng.ClearBin(nodeID, "", false, nil)

	for _, path := range []string{"/api/telemetry/bin-count", "/api/telemetry/bin-clear"} {
		got := core.sent(path)
		if len(got) == 0 {
			t.Fatalf("%s: nothing sent", path)
		}
		if _, named := got[0]["bin_id"]; named {
			t.Errorf("%s body = %v, want no bin_id with no copy held", path, got[0])
		}
	}
}

// epochRefused is Core's refusal of a count from an ended generation: 409 with
// the carrier's current stamp (www.apiBinCount).
func epochRefused(epoch int64) (int, any) {
	return http.StatusConflict, map[string]any{
		"epoch_refresh": protocol.BinEpochRefresh{BinID: 4242, CoreNodeName: "ASM-MSWAP-NODE", Epoch: epoch},
		"error":         "bin AS-BIN changed since that count was taken — nothing was recorded"}
}

// epochSent is the epoch a write's body carried; 0 when the key was absent.
func epochSent(body map[string]any) int64 {
	v, _ := body["epoch"].(float64)
	return int64(v)
}

// TestWrites_CountSaysTheSeatsGeneration: a count of the carrier the seat
// holds carries the seat's generation, the one its count was taken in.
func TestWrites_CountSaysTheSeatsGeneration(t *testing.T) {
	t.Parallel()
	eng, core, nodeID := assumeEngine(t)
	holdBoard(t, eng, occupiedRow("ASM-MSWAP-NODE", 4242, 9))

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 20, "op"), "RecordBinCount")

	if got := core.sent("/api/telemetry/bin-count"); len(got) != 1 || epochSent(got[0]) != 9 {
		t.Errorf("bin-count bodies = %v, want one at the seat's epoch 9", got)
	}
}

// TestWrites_CountFromAnEndedGenerationAdoptsAndCountsAgain: Core refuses the
// count with the carrier's current stamp; the Edge adopts it on the seat, as a
// bin epoch refresh is adopted, and sends the count once more under it.
func TestWrites_CountFromAnEndedGenerationAdoptsAndCountsAgain(t *testing.T) {
	t.Parallel()
	eng, core, nodeID := assumeEngine(t)
	holdBoard(t, eng, occupiedRow("ASM-MSWAP-NODE", 4242, 9))
	core.reply["/api/telemetry/bin-count"] = func(n int, body map[string]any) (int, any) {
		if n == 1 {
			return epochRefused(12)
		}
		return http.StatusOK, map[string]any{"status": "ok", "bin_id": 4242, "uop_remaining": 20, "delta_epoch": 12}
	}

	testutil.MustNoErr(t, eng.RecordBinCount(nodeID, 20, "op"), "RecordBinCount")

	got := core.sent("/api/telemetry/bin-count")
	if len(got) != 2 || epochSent(got[0]) != 9 || epochSent(got[1]) != 12 {
		t.Fatalf("bin-count bodies = %v, want epoch 9 and then 12", got)
	}
	rt, err := eng.db.GetProcessNodeRuntime(nodeID)
	testutil.MustNoErr(t, err, "runtime")
	if rt.ActiveBinEpoch != 12 || rt.RemainingUOPCached != 20 {
		t.Errorf("seat = epoch %d count %d, want 12 and 20", rt.ActiveBinEpoch, rt.RemainingUOPCached)
	}
}

// TestWrites_ASecondEndedGenerationIsShownNotRetried: a count refused again
// under the adopted stamp goes to the screen as Core's sentence; it is sent
// twice and no more.
func TestWrites_ASecondEndedGenerationIsShownNotRetried(t *testing.T) {
	t.Parallel()
	eng, core, nodeID := assumeEngine(t)
	holdBoard(t, eng, occupiedRow("ASM-MSWAP-NODE", 4242, 9))
	core.reply["/api/telemetry/bin-count"] = func(n int, _ map[string]any) (int, any) { return epochRefused(12 + int64(n)) }

	err := eng.RecordBinCount(nodeID, 20, "op")
	if err == nil || !strings.Contains(err.Error(), "changed since that count was taken") {
		t.Errorf("err = %v, want Core's sentence", err)
	}
	if got := core.sent("/api/telemetry/bin-count"); len(got) != 2 {
		t.Errorf("bin-count sent %d times, want 2", len(got))
	}
}

// TestWrites_ARefusedTapRedrawsCoresRow: a count or CLEAR Core refuses comes
// back with Core's row for the node, and by the time the refusal's sentence
// reaches the screen the bins copy, which the tile draws, holds Core's row
// and not the one the tap assumed.
func TestWrites_ARefusedTapRedrawsCoresRow(t *testing.T) {
	t.Parallel()
	for _, door := range []struct {
		path string
		tap  func(eng *Engine, nodeID int64) error
	}{
		{"/api/telemetry/bin-count", func(eng *Engine, nodeID int64) error { return eng.RecordBinCount(nodeID, 20, "op") }},
		{"/api/telemetry/bin-clear", func(eng *Engine, nodeID int64) error { return eng.ClearBin(nodeID, "", false, nil) }},
	} {
		t.Run(door.path, func(t *testing.T) {
			t.Parallel()
			eng, core, nodeID := assumeEngine(t)
			holdBoard(t, eng, occupiedRow("ASM-MSWAP-NODE", 4242, 9))
			cores := occupiedRow("ASM-MSWAP-NODE", 5151, 3)
			core.reply[door.path] = func(int, map[string]any) (int, any) {
				return http.StatusConflict, map[string]any{"error": "bin 4242 is not at node ASM-MSWAP-NODE",
					"rows": []protocol.NodeBinsRow{cores}}
			}
			var redrawn []string
			eng.Events.SubscribeTypes(func(ev Event) {
				redrawn = append(redrawn, ev.Payload.(NodeBinsUpdatedEvent).Nodes...)
			}, EventNodeBinsUpdated)

			err := door.tap(eng, nodeID)
			if err == nil || !strings.Contains(err.Error(), "bin 4242 is not at node") {
				t.Fatalf("err = %v, want Core's sentence", err)
			}
			if got := eng.LocalNodeBins().Rows["ASM-MSWAP-NODE"]; got.BinID != 5151 || got.Epoch != 3 {
				t.Errorf("the tile's row = %+v, want Core's (bin 5151, epoch 3)", got)
			}
			if !slices.Contains(redrawn, "ASM-MSWAP-NODE") {
				t.Errorf("redrawn = %v, want the node", redrawn)
			}
		})
	}
}

// TestCoreClient_ARefusalCarriesItsRows: each write door's refusal decodes to a
// *CoreRefusal with Core's sentence and its rows.
func TestCoreClient_ARefusalCarriesItsRows(t *testing.T) {
	t.Parallel()
	row := occupiedRow("ASM-MSWAP-NODE", 5151, 3)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "refused", "rows": []protocol.NodeBinsRow{row}})
	}))
	t.Cleanup(srv.Close)
	c := NewCoreClient(srv.URL)
	_, loadErr := c.LoadBin(&BinLoadRequest{NodeName: "ASM-MSWAP-NODE"})
	_, countErr := c.RecordBinCount(protocol.BinCountRequest{NodeName: "ASM-MSWAP-NODE"})
	_, clearErr := c.ClearBin(protocol.BinClearRequest{NodeName: "ASM-MSWAP-NODE"})
	for name, err := range map[string]error{"load": loadErr, "count": countErr, "clear": clearErr} {
		var r *CoreRefusal
		if !errors.As(err, &r) || r.Status != http.StatusConflict || r.Sentence != "refused" || len(r.Rows) != 1 || r.Rows[0].BinID != 5151 {
			t.Errorf("%s: err = %#v, want a 409 refusal carrying Core's row", name, err)
		}
	}
}
