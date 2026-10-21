package www

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store"
	"shingoedge/store/processes"
)

// handlers_flow_running_move_test.go — the running-position refusal, through
// the real engine and the real router, at BOTH doors.
//
// WHY NOT THE STUB. handlers_flow_test.go pins the status mapping by handing
// the seam a sentinel error, which proves 409 and proves the handler passes
// the seam's words through. It cannot prove the words. The sentence an
// engineer actually reads — "PLN_01 holds BIN-1. Move the bin in Core first
// (Bins → Move to PLN_03), then change the position here." — is built in
// engine.refuseRunningPositionMove out of the bins copy and the two position
// sets, and a stub never runs it.
//
// AND AT BOTH DOORS, because they are the same route. The handler decides
// source from the session and never from the body (owner ruling R1): an admin
// session is a person on the desktop, no session is the kiosk on the floor.
// S3's Q4 makes the composer GATE apply to the HMI alone, and this refusal is
// not that gate — it is about what the press is running, which is true whoever
// is typing. A refusal that reached one screen and not the other would be a
// rule an engineer could walk around by using the other window.

func TestSaveFlow_RunningPositionMoveRefusesBothDoorsByName(t *testing.T) {
	db := testdb.Open(t)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")

	// The routing set is derived and adopted (a flow naming an un-adopted
	// destination is refused before anything else looks at it) and the gate is
	// opened, which is what a save through the HMI door needs.
	testdb.SeedRoutingSet(t, db, seeded.ProcessID, "test")
	if err := db.SetFlowComposerEnabled(seeded.ProcessID, true); err != nil {
		t.Fatalf("open the gate: %v", err)
	}

	process, err := db.GetProcess(seeded.ProcessID)
	if err != nil {
		t.Fatalf("GetProcess: %v", err)
	}
	if process.ActiveStyleID == nil || *process.ActiveStyleID == 0 {
		t.Fatal("the fixture's press is running nothing — this test has no running style to refuse for")
	}
	runningID := *process.ActiveStyleID
	style, err := db.GetStyle(runningID)
	if err != nil {
		t.Fatalf("GetStyle: %v", err)
	}
	stored, err := db.ListStyleNodeClaims(runningID)
	if err != nil {
		t.Fatalf("ListStyleNodeClaims: %v", err)
	}
	if len(stored) < 2 {
		t.Fatalf("the running style has %d positions; a move needs one to leave and one to arrive", len(stored))
	}

	// A DRAFT THAT MOVES A POSITION: every stored cell, with the first one
	// re-pointed at a position the flow does not have. That is one leaving and
	// one arriving on the set of positions, which is what the gate fires on.
	free := freePositionName(t, db, seeded.ProcessID, stored)
	leaving := stored[0].CoreNodeName
	var cells []domain.FlowCell
	for i, c := range stored {
		cell := domain.Collapse(c)
		if i == 0 {
			cell.CoreNodeName = free
			// A back position pairs with a front one by name; moving the cell
			// and leaving the pairing behind would be refused for the pairing
			// instead, which is a different sentence.
			cell.PairedCoreNode = ""
		}
		cells = append(cells, cell)
	}

	h, router, eng := realFlowRouterEngine(t, db)

	// The fingerprint of the flow as stored, so the save below is refused for
	// the move and not for being stale — the other 409 on this route.
	previewResp := doRequest(t, router, "POST",
		"/api/processes/"+itoa(seeded.ProcessID)+"/flow/preview",
		map[string]any{"to_style_id": runningID}, nil)
	assertStatus(t, previewResp, http.StatusOK)
	preview := flowBody(t, previewResp)
	if preview["running"] != true {
		t.Error("a preview of the style on the press did not say so — R3 made this a 200 that names the state")
	}
	fingerprint, _ := preview["fingerprint"].(string)
	if fingerprint == "" {
		t.Fatal("the preview returned no fingerprint; the save below would be refused as stale")
	}

	body := map[string]any{
		"to_style_id": runningID,
		"cells":       cells,
		"fingerprint": fingerprint,
		"station_id":  seeded.Stations["screen-a4"],
	}

	// THE BIN STANDS ON THE LEAVING POSITION, in the Edge's copy of Core's bins
	// (every other node bare): the move would strand it, so it is refused. A
	// move that leaves no bin behind saves (engine flow_running_move_test.go).
	holdRunningMoveBins(t, eng, leaving, free)

	// THE SENTENCE, spelled here out of the parts the engine builds it from —
	// not a substring of it. The engineer is told which position holds which
	// bin, where to move it in Core and in what order; a test matching only
	// "holds" would pass on a sentence that had lost the order.
	_ = style
	want := leaving + " holds BIN-1. Move the bin in Core first (Bins → Move to " + free + "), then change the position here."

	for _, door := range []struct {
		name   string
		cookie *http.Cookie
	}{
		{"the HMI (no session — the kiosk on the floor)", nil},
		{"the desktop (an admin session)", authCookie(t, h)},
	} {
		resp := doRequest(t, router, "POST",
			"/api/processes/"+itoa(seeded.ProcessID)+"/flow/save", body, door.cookie)
		assertStatus(t, resp, http.StatusConflict)
		got := flowBody(t, resp)
		msg, _ := got["error"].(string)
		if !strings.Contains(msg, want) {
			t.Errorf("%s: the refusal reads %q;\n  it has to name the style and both positions: %q",
				door.name, msg, want)
		}
		if got["stale"] == true {
			t.Errorf("%s: the refusal came back as stale; the fingerprint matched and the move is the reason", door.name)
		}
	}

	// AND NOTHING MOVED. A refusal that had written half the flow first would
	// be worse than the move it refused.
	after, err := db.ListStyleNodeClaims(runningID)
	if err != nil {
		t.Fatalf("ListStyleNodeClaims after: %v", err)
	}
	if len(after) != len(stored) {
		t.Fatalf("the running style has %d positions after two refused saves, had %d", len(after), len(stored))
	}
	for i := range after {
		if after[i].CoreNodeName != stored[i].CoreNodeName {
			t.Errorf("position %d is %q after two refused saves, was %q",
				i, after[i].CoreNodeName, stored[i].CoreNodeName)
		}
	}
}

// freePositionName is a position of the process that the given claims do not
// name — somewhere for the moved cell to arrive at.
func freePositionName(t *testing.T, db *store.DB, processID int64, claims []processes.NodeClaim) string {
	t.Helper()
	nodes, err := db.ListProcessNodesByProcess(processID)
	if err != nil {
		t.Fatalf("ListProcessNodesByProcess: %v", err)
	}
	used := map[string]bool{}
	for _, c := range claims {
		used[c.CoreNodeName] = true
	}
	for _, n := range nodes {
		if !used[n.CoreNodeName] {
			return n.CoreNodeName
		}
	}
	t.Fatal("every position of the press is already on the running flow; nothing to move a cell to")
	return ""
}

// realFlowRouter is the real engine behind the real router, with Core's
// catalog marked arrived — refusePressIndexWhenCoreUnavailable keys on it, so
// without it every press-index preview is refused by name and this test would
// never reach the save.
func realFlowRouter(t *testing.T, db *store.DB) (*Handlers, *chi.Mux) {
	t.Helper()
	h, mux, _ := realFlowRouterEngine(t, db)
	return h, mux
}

// realFlowRouterEngine is realFlowRouter with the engine behind it, for a test
// that holds Core's bins copy on it.
func realFlowRouterEngine(t *testing.T, db *store.DB) (*Handlers, *chi.Mux, *engine.Engine) {
	t.Helper()
	cfg := config.Defaults()
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	// Core's catalog has arrived (empty): the press-index changeover's gate
	// opens on it, as it opened on a configured Core address.
	eng.SetPayloadBinTypes(nil)
	eng.Start()
	t.Cleanup(eng.Stop)
	var pbt []protocol.PayloadBinTypeInfo
	for code, types := range scenefixtures.A().PayloadBinTypeCodes() {
		for _, bt := range types {
			pbt = append(pbt, protocol.PayloadBinTypeInfo{PayloadCode: code, BinTypeCode: bt})
		}
	}
	eng.SetPayloadBinTypes(pbt)
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	h, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	mux, ok := router.(*chi.Mux)
	if !ok {
		t.Fatalf("NewRouter returned %T, not a *chi.Mux", router)
	}
	return h, mux, eng
}

// holdRunningMoveBins holds, as the engine's bins copy, every node of its
// watch list (and extra) bare except leaving, which holds BIN-1, and returns
// once the copy may be decided from. The engine is started, so its watch list
// can be rebuilt between the read of the list and the apply; a copy answering
// a list that moved is not actable, so the hold is taken again until it is.
func holdRunningMoveBins(t *testing.T, eng *engine.Engine, leaving string, extra ...string) {
	t.Helper()
	var why string
	for i := 0; i < 50; i++ {
		watched := eng.StationService().WatchedBinNodes()
		listDigest, err := protocol.BinWatchDigest(watched)
		if err != nil {
			t.Fatalf("watch digest: %v", err)
		}
		snap := protocol.NodeBinsSnapshot{ListDigest: listDigest, Groups: []protocol.NodeBinsGroup{}}
		for _, n := range append(append([]string(nil), watched...), extra...) {
			row := protocol.NodeBinsRow{Node: n, State: protocol.BinsStateEmpty}
			if n == leaving {
				row = protocol.NodeBinsRow{Node: n, State: protocol.BinsStateOccupied, Occupied: true, BinLabel: "BIN-1"}
			}
			snap.Rows = append(snap.Rows, row)
		}
		if snap.Digest, err = protocol.NodeBinsDigest(snap); err != nil {
			t.Fatalf("digest: %v", err)
		}
		if err := eng.ApplyNodeBinsSnapshot(snap); err != nil {
			t.Fatalf("apply the bins copy: %v", err)
		}
		// The ack quotes the copy's digest, as Core's does once it holds the
		// same snapshot: that confirms the copy at the ack, whatever the clock's
		// resolution (Actable).
		eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: snap.Digest}, BinWatch: &listDigest})
		var ok bool
		if ok, why = eng.BinsActable(); ok {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("the bins copy never became actable: %s", why)
}
