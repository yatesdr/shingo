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
	"time"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingo/shared/scenefixtures"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/messaging"
	"shingoedge/store"
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
	_, handler, stop := www.NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	inbound := buildInboundRouter(eng, messaging.NewEdgeHandler(eng.OrderManager()), nopHeartbeater{}, func(string, ...any) {})
	frames := runningStream(t, srv.URL)

	views := map[string]json.RawMessage{}
	perBuild := map[string]int64{}
	for name, sid := range boards {
		body, etag := runningView(t, srv.URL, sid, "")
		views[name] = body
		counter.Reset()
		runningView(t, srv.URL, sid, etag)
		perBuild[name] = counter.Count()
	}
	drainFrames(frames, 300*time.Millisecond)

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
			got := drainFrames(frames, 450*time.Millisecond)
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
	id, err := db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &node, false, 1, coreNode, "", "MARKET", false, "PART-01", "", "")
	if err != nil {
		t.Fatalf("create order: %v", err)
	}
	if _, err := db.Exec(`UPDATE orders SET status='queued' WHERE id=?`, id); err != nil {
		t.Fatalf("queue order: %v", err)
	}
	return uuid
}

// runningView GETs a station's view, conditional when etag is set.
func runningView(t *testing.T, base string, sid int64, etag string) (json.RawMessage, string) {
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
	return body, res.Header.Get("ETag")
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

// drainFrames collects every frame that arrives within d.
func drainFrames(ch <-chan sseFrameW, d time.Duration) []sseFrameW {
	var out []sseFrameW
	deadline := time.After(d)
	for {
		select {
		case f := <-ch:
			out = append(out, f)
		case <-deadline:
			return out
		}
	}
}
