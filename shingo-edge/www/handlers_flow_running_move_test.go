package www

import (
	"net/http"
	"strings"
	"testing"

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
// engineer actually reads — "<style> is running on PLN_01 — move it to PLN_03
// after the next changeover" — is built in engine.refuseRunningPositionMove
// out of the style's name and the two position sets, and a stub never runs it.
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

	h, router := realFlowRouter(t, db)

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

	// THE SENTENCE, spelled here out of the same three parts the engine builds
	// it from — not a substring of it. The point of this refusal is that the
	// engineer is told which style, which position it is leaving and which it
	// would arrive at; a test matching only "is running" would pass on a
	// sentence that had lost two of the three.
	want := style.Name + " is running on " + leaving + " — move it to " + free + " after the next changeover"

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

// realFlowRouter is the real engine behind the real router, with Core
// unreachable on purpose — refusePressIndexWhenCoreUnavailable keys on "CoreAPI
// is not empty", so a blank one would refuse every press-index preview by name
// and this test would never reach the save.
func realFlowRouter(t *testing.T, db *store.DB) (*Handlers, *chi.Mux) {
	t.Helper()
	cfg := config.Defaults()
	cfg.CoreAPI = "http://core.invalid"
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
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
	return h, mux
}
