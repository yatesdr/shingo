package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
	"shingoedge/www"
)

// running_cost_test.go — what Core's order updates cost each board on a
// running plant, and the pin that a board rebuilds only for the orders it
// draws.
//
// Two boards on one Edge: the 22-home loader board and a press board seeded
// from a real plant's setup. Core sends order updates, half for orders at the
// loader's homes and half for orders at the press's positions, through the
// real inbound table. Each board decides whether to rebuild with its own
// predicate (operator.js stationEvents, operator-state.js eventConcerns, run in
// node), one decision per update (the board's 500 ms debounce folds the frames
// of one update into one build). A board must rebuild for every update of an
// order it draws and for no other.
//
// The cost is logged per board: builds per update, and the statements those
// builds send (one 304 view GET's statements each, measured on the counting
// store).
func TestRunningCost_ABoardRebuildsOnlyForItsOrders(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not on PATH: the board's eventConcerns runs in node")
	}
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "running.db"))
	if err != nil {
		t.Fatalf("open counting store: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	cfg := config.Defaults()
	cfg.StationUID = "running.edge"
	cfg.Messaging.StationID = cfg.StationUID
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: func(string, ...any) {}})
	eng.Start()
	t.Cleanup(eng.Stop)

	loader := twentyTwoHomeBoard(t, db, eng)
	seeded := testdb.SeedPlant(t, db, scenefixtures.A(), "Press A1")
	press := seeded.Stations["screen-a4"]
	boards := map[string]int64{"22-home loader": loader, "press": press}

	// The orders: one per home SMN_H01..H05 and one per press position.
	type ord struct {
		uuid  string
		board string
	}
	var orders []ord
	for i := 1; i <= 5; i++ {
		orders = append(orders, ord{uuid: runningOrder(t, db, loader, fmt.Sprintf("SMN_H%02d", i), i), board: "22-home loader"})
	}
	for i, n := range []string{"PLN_01", "PLN_04", "PLN_06"} {
		orders = append(orders, ord{uuid: runningOrder(t, db, press, n, 10+i), board: "press"})
	}

	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, handler, stop := www.NewRouter(eng, dbg, nil, www.WithCoalescingWindows(walkNudgeWindow, walkNudgeWindow))
	t.Cleanup(stop)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	inbound := buildInboundRouter(eng, messaging.NewEdgeHandler(eng.OrderManager()), nopHeartbeater{}, func(string, ...any) {})
	frames := runningStream(t, srv.URL)

	views := map[string]json.RawMessage{}
	perBuild := map[string]int64{}
	for name, sid := range boards {
		body, etag, _ := runningView(t, srv.URL, sid, "")
		views[name] = body
		counter.Reset()
		_, _, status := runningView(t, srv.URL, sid, etag)
		perBuild[name] = counter.Count()
		// What a rebuild costs is what the refetch of an unchanged board costs:
		// a 304, the build run and no body sent.
		if status != http.StatusNotModified || perBuild[name] == 0 {
			t.Fatalf("the %s board's conditional view answered %d with %d statements: the cost measures no 304", name, status, perBuild[name])
		}
	}
	// The router runs the walk's windows (door_walk_test.go), so an update's
	// frames are all in within walkWindow, as a door's are.
	collectFrames(frames, walkWindow)

	// Each update: a new reason on a queued order (the status does not move),
	// then a move to in transit (it does): the two shapes Core sends.
	var cases []walkCase
	var drawer []string
	var updates []string
	statuses := []protocol.OrderUpdate{
		{Status: string(protocol.StatusQueued), QueueReason: "Waiting for a slot", QueueCode: "dropoff-occupied"},
		{Status: string(protocol.StatusInTransit)},
	}
	for _, o := range orders {
		for _, u := range statuses {
			u.OrderUUID = o.uuid
			env, err := protocol.NewEnvelope(protocol.TypeOrderUpdate, protocol.Address{Role: protocol.RoleCore},
				protocol.Address{Role: protocol.RoleEdge, Station: cfg.StationUID}, u)
			if err != nil {
				t.Fatalf("envelope: %v", err)
			}
			inbound.Dispatch(env, env.Type)
			got := collectFrames(frames, walkWindow)
			for name, sid := range boards {
				cases = append(cases, walkCase{Door: o.uuid + " " + u.Status, Station: sid, View: views[name], Frames: got})
				drawer = append(drawer, o.board)
				updates = append(updates, name)
			}
		}
	}
	verdicts := judgeCases(t, cases)
	builds := map[string]int{}
	n := len(orders) * len(statuses)
	for i, rebuilt := range verdicts {
		board := updates[i]
		if rebuilt {
			builds[board]++
		}
		if rebuilt != (drawer[i] == board) {
			t.Errorf("%s: the %s board rebuilt = %v; it draws that order: %v", cases[i].Door, board, rebuilt, drawer[i] == board)
		}
	}
	var lines []string
	for name := range boards {
		lines = append(lines, fmt.Sprintf("  %-15s %d builds for %d order updates (%d of them its own), %d statements each, %d statements",
			name, builds[name], n, ownUpdates(drawer, updates, name), perBuild[name], int64(builds[name])*perBuild[name]))
	}
	t.Logf("Core order updates, both boards open:\n%s", strings.Join(lines, "\n"))
}

func ownUpdates(drawer, updates []string, board string) int {
	n := 0
	for i := range drawer {
		if updates[i] == board && drawer[i] == board {
			n++
		}
	}
	return n
}

// runningOrder files a queued retrieve at a station's position and returns its
// uuid.
func runningOrder(t *testing.T, db *store.DB, station int64, coreNode string, i int) string {
	t.Helper()
	var node int64
	if err := db.QueryRow(`SELECT id FROM process_nodes WHERE core_node_name=? AND operator_station_id=?`, coreNode, station).Scan(&node); err != nil {
		t.Fatalf("node %s on station %d: %v", coreNode, station, err)
	}
	uuid := fmt.Sprintf("running-%d", i)
	id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &node, false, 1, coreNode, "", "MARKET", protocol.Height{}, false, "PART-01", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET status='queued' WHERE id=?`, id); err != nil {
		t.Fatalf("queue order: %v", err)
	}
	return uuid
}

// runningView GETs a station's view, conditional when etag is set, and answers
// its body, ETag and status.
func runningView(t *testing.T, base string, sid int64, etag string) (json.RawMessage, string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("%s/api/operator-stations/%d/view", base, sid), nil)
	if err != nil {
		t.Fatalf("view request: %v", err)
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("view %d: %v", sid, err)
	}
	defer res.Body.Close()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read view %d: %v", sid, err)
	}
	if etag == "" && res.StatusCode != http.StatusOK {
		t.Fatalf("view %d: %d %s", sid, res.StatusCode, body)
	}
	return body, res.Header.Get("ETag"), res.StatusCode
}

// runningStream opens /events with every topic and streams its frames.
func runningStream(t *testing.T, base string) <-chan sseFrameW {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/events", nil)
	if err != nil {
		t.Fatalf("stream request: %v", err)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("open /events: %v", err)
	}
	out := make(chan sseFrameW, 1024)
	go func() {
		defer res.Body.Close()
		sc := bufio.NewScanner(res.Body)
		sc.Buffer(make([]byte, 1<<20), 1<<24)
		var typ string
		var data []byte
		for sc.Scan() {
			line := sc.Text()
			switch {
			case line == "":
				if typ != "" && typ != "heartbeat" && typ != "connected" {
					out <- sseFrameW{Type: typ, Data: append(json.RawMessage(nil), data...)}
				}
				typ, data = "", nil
			case strings.HasPrefix(line, "event: "):
				typ = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "data: "):
				data = []byte(strings.TrimPrefix(line, "data: "))
			}
		}
	}()
	return out
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
	oid, err := db.CreateOrder("homes22-order-a", protocol.OrderTypeMove, &h03, false, 1, "SMN_F2", "", "SMN_H03", protocol.Height{}, false, "", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET status='queued' WHERE id=?`, oid); err != nil {
		t.Fatalf("queue order: %v", err)
	}
	return sid
}
