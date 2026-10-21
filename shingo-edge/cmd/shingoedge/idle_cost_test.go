package main

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"path/filepath"
	"testing"
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	"shingoedge/www"
)

// idle_cost_test.go — what an idle 22-home loader board costs the Edge in an
// hour, with the 3 s poll it had and with the stream alone. Logged, not
// asserted: the numbers are for the report; the no-request property is pinned
// by operator-refresh.test.js ("an idle hour") and the walk's idle check.
//
// Measured through the real router on a counting store (store.OpenCounting):
// the statements of one conditional view GET answered 304 (what every idle
// poll was), and of one Touch (the poll's throttled write before, the
// heartbeat's write now).
func TestIdleCost_TwentyTwoHomeBoard(t *testing.T) {
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "cost.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.StationUID = "cost.edge"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	eng.Start()
	t.Cleanup(eng.Stop)
	sid := twentyTwoHomeBoard(t, db, eng)

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, handler, stop := www.NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	viewURL := fmt.Sprintf("%s/api/operator-stations/%d/view", srv.URL, sid)

	first, err := http.Get(viewURL)
	if err != nil || first.StatusCode != http.StatusOK {
		t.Fatalf("first view: %v %v", first, err)
	}
	first.Body.Close()
	etag := first.Header.Get("ETag")

	// One idle poll: the conditional GET the board made every 3 s.
	req, err := http.NewRequest(http.MethodGet, viewURL, nil)
	if err != nil {
		t.Fatalf("view request: %v", err)
	}
	req.Header.Set("If-None-Match", etag)
	req.Header.Set("Accept-Encoding", "gzip")
	reqBytes, err := httputil.DumpRequestOut(req, false)
	if err != nil {
		t.Fatalf("dump request: %v", err)
	}
	counter.Reset()
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional view: %v", err)
	}
	res.Body.Close()
	perPoll := counter.Count()
	resBytes, err := httputil.DumpResponse(res, false)
	if err != nil {
		t.Fatalf("dump response: %v", err)
	}
	if res.StatusCode != http.StatusNotModified || perPoll == 0 {
		t.Fatalf("the idle poll answered %d with %d statements: the measurement measured nothing", res.StatusCode, perPoll)
	}

	// One Touch, on a station never touched (the throttle does not apply).
	counter.Reset()
	if err := eng.StationService().Touch(sid, "online"); err != nil {
		t.Fatalf("touch: %v", err)
	}
	perTouch := counter.Count()

	// One heartbeat frame's bytes, as the station's stream carries it.
	hb := heartbeatBytes(t, srv.URL, sid)

	const hour = 3600
	pollsPerHour := int64(hour / 3)
	touchesBefore := int64(hour / 15) // the poll's Touch, throttled to one per 15 s
	heartbeats := int64(hour / 30)
	beforeStmts := pollsPerHour*perPoll + touchesBefore*perTouch
	afterStmts := heartbeats * perTouch
	t.Logf("22-home board, one idle hour:\n"+
		"  before (3 s poll): %d view GETs, %d statements (%d per 304 GET + %d Touch writes), %d bytes on the wire (%d request + %d response headers per poll)\n"+
		"  after (stream):    0 view GETs, %d statements (%d heartbeat Touch writes), %d bytes (%d heartbeats of %d bytes)",
		pollsPerHour, beforeStmts, perPoll, touchesBefore*perTouch, pollsPerHour*int64(len(reqBytes)+len(resBytes)), len(reqBytes), len(resBytes),
		afterStmts, heartbeats*perTouch, heartbeats*int64(hb), heartbeats, hb)
}

// heartbeatBytes is the size of one heartbeat frame on a station's stream,
// read off the connected frame: the two carry the same build id and seq, and
// "connected" and "heartbeat" are the same length, so the bytes are the same
// without waiting out the 30 s keepalive.
func heartbeatBytes(t *testing.T, base string, sid int64) int {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("%s/events?station=%d&topics=bins", base, sid), nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	defer res.Body.Close()
	sc := bufio.NewScanner(res.Body)
	n := 0
	in := false
	for sc.Scan() {
		line := sc.Text()
		if line == "event: connected" {
			in = true
		}
		if in {
			n += len(line) + 1
			if line == "" {
				return n
			}
		}
	}
	t.Fatalf("no connected frame")
	return 0
}

// twentyTwoHomeBoard is engine's buildTwentyTwoHomeBoard
// (station_view_loader_golden_test.go) through the same exported doors: 22
// homes, each pinned to its own part, 9 buffer spots, five served cells of four
// or five parts, one home's clear in flight. Made-up names.
func twentyTwoHomeBoard(t *testing.T, db *store.DB, eng *engine.Engine) int64 {
	t.Helper()
	procID, err := db.CreateProcess("LDR-22", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := db.CreateOperatorStation(stations.Input{ProcessID: procID, Code: "LDR-22-ST", Name: "LDR-22 Station", Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	var positions []protocol.LoaderPosition
	ids := map[string]int64{}
	for i := 1; i <= 22; i++ {
		home := fmt.Sprintf("SMN_H%02d", i)
		id, err := db.CreateProcessNode(processes.NodeInput{ProcessID: procID, OperatorStationID: &sid, CoreNodeName: home,
			Code: home, Name: home, Sequence: i, Enabled: true})
		if err != nil {
			t.Fatalf("create node: %v", err)
		}
		ids[home] = id
		positions = append(positions, protocol.LoaderPosition{CoreNodeName: home,
			PayloadCode: fmt.Sprintf("PART-%02d", i), HomeKind: protocol.LoaderHomeKindHome, Ordinal: i})
	}
	for i := 1; i <= 9; i++ {
		positions = append(positions, protocol.LoaderPosition{CoreNodeName: fmt.Sprintf("SMN_F%d", i),
			HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 22 + i})
	}
	for c := 0; c < 5; c++ {
		var parts []string
		for i := c*5 + 1; i <= c*5+5 && i <= 22; i++ {
			parts = append(parts, fmt.Sprintf("PART-%02d", i))
		}
		cell := fmt.Sprintf("CELL-%d", c+1)
		node := fmt.Sprintf("WLN_%d", c+1)
		cp, err := db.CreateProcess(cell, "", "", "", false)
		if err != nil {
			t.Fatalf("create cell: %v", err)
		}
		style, err := db.CreateStyle(cell+"-RUN", "", cp)
		if err != nil {
			t.Fatalf("create style: %v", err)
		}
		if err := db.SetActiveStyle(cp, &style); err != nil {
			t.Fatalf("activate style: %v", err)
		}
		nid, err := db.CreateProcessNode(processes.NodeInput{ProcessID: cp, CoreNodeName: node, Code: node, Name: node, Sequence: 1, Enabled: true})
		if err != nil {
			t.Fatalf("create cell node: %v", err)
		}
		if _, err := db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
			StyleID: style, CoreNodeName: node, Role: protocol.ClaimRoleConsume,
			SwapMode: protocol.SwapModeSingleRobot, PayloadCode: parts[0], AllowedPayloadCodes: parts,
			UOPCapacity: 100, InboundSource: "MARKET", OutboundDestination: "MARKET",
			InboundStaging: node + "-IN", OutboundStaging: node + "-OUT",
		}); err != nil {
			t.Fatalf("cell claim: %v", err)
		}
		if _, err := db.EnsureProcessNodeRuntime(nid); err != nil {
			t.Fatalf("cell runtime: %v", err)
		}
		if err := db.SetProcessNodeRuntime(nid, nil, 10*(c+1)); err != nil {
			t.Fatalf("cell runtime: %v", err)
		}
	}
	if err := eng.SetCoreLoaders([]protocol.LoaderInfo{{
		Name: "Homes-22", LoaderKey: "loader:22", Role: "produce", Layout: "dedicated_positions",
		Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER", Positions: positions,
	}}); err != nil {
		t.Fatalf("SetCoreLoaders: %v", err)
	}
	h03 := ids["SMN_H03"]
	oid, err := db.CreateOrder("homes22-order-a", protocol.OrderTypeMove, &h03, false, 1, "SMN_F2", "", "SMN_H03", "standard", false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET status='queued' WHERE id=?`, oid); err != nil {
		t.Fatalf("queue order: %v", err)
	}
	return sid
}
