package www

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"shingo/protocol"
	"shingo/protocol/debuglog"
	"shingoedge/config"
	"shingoedge/domain"
	"shingoedge/engine"
	"shingoedge/store"
	"shingoedge/store/processes"
	"shingoedge/store/stations"
)

// station_bins_test.go — the station view's bins: one node-bins read per
// build for the board's whole list (its tiles, its dedicated loaders' buffer
// spots, its consume loaders' outbound-group members), stamped onto the
// tiles' bin_state, a home's buffer_partial, a dedicated loader's buffer strip
// and the board's empties; and the Core calls a build makes, counted on a
// Core stub.

// coreStub answers the three Core reads a station view can make and counts
// every request it sees, by path.
type coreStub struct {
	mu   sync.Mutex
	hits map[string]int
	// rows are the node-bins rows by node name; an asked name with no row is
	// answered unoccupied, as Core answers a node it does not know.
	rows map[string]protocol.NodeBinInfo
}

func newCoreStub(t *testing.T, rows map[string]protocol.NodeBinInfo) (*coreStub, *httptest.Server) {
	t.Helper()
	s := &coreStub{hits: map[string]int{}, rows: rows}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		key := r.URL.Path
		if strings.HasSuffix(key, "/children") {
			key = "children"
		}
		s.hits[key]++
		s.mu.Unlock()
		switch {
		case r.URL.Path == "/api/telemetry/node-bins":
			var out []protocol.NodeBinInfo
			for _, n := range strings.Split(r.URL.Query().Get("nodes"), ",") {
				row, ok := s.rows[n]
				if !ok {
					row = protocol.NodeBinInfo{NodeName: n}
				}
				out = append(out, row)
			}
			_ = json.NewEncoder(w).Encode(out)
		case r.URL.Path == "/api/containment":
			_ = json.NewEncoder(w).Encode(map[string]any{"containment": []any{}, "held_bins": []any{}})
		case key == "children":
			_ = json.NewEncoder(w).Encode([]any{})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return s, srv
}

// take returns the hits since the last take and zeroes them.
func (s *coreStub) take() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.hits
	s.hits = map[string]int{}
	return out
}

// binsRig is a real engine over a counting store, its Core API pointed at a
// stub, and the router the Edge serves.
type binsRig struct {
	eng     *engine.Engine
	db      *store.DB
	counter *store.QueryCounter
	core    *coreStub
	router  *chi.Mux
	// stations by code.
	stations map[string]int64
}

// The plant: a dedicated loader (two homes on its station, two buffer spots
// that are not) and a consume shared-window loader (two windows on its
// station) whose outbound group EMPTY-GRP has three members.
var binsRigRows = map[string]protocol.NodeBinInfo{
	"SMN_H1": {NodeName: "SMN_H1", BinID: 1, BinLabel: "BIN-1", BinTypeCode: "KD-1", PayloadCode: "PART-A", UOPRemaining: 12, Occupied: true},
	"SMN_H2": {NodeName: "SMN_H2", BinID: 2, BinLabel: "BIN-2", BinTypeCode: "KD-1", Occupied: true},
	"SMN_F1": {NodeName: "SMN_F1", BinID: 3, BinLabel: "BIN-3", BinTypeCode: "KD-1", PayloadCode: "PART-A", UOPRemaining: 5, Occupied: true},
	"ULN_1":  {NodeName: "ULN_1", BinID: 4, BinLabel: "CART-4", BinTypeCode: "TOTE-1", PayloadCode: "PART-C", UOPRemaining: 40, Occupied: true},
	"E1":     {NodeName: "E1", BinID: 5, BinLabel: "CART-5", BinTypeCode: "TOTE-1", Occupied: true},
	"E2":     {NodeName: "E2", BinID: 6, BinLabel: "CART-6", BinTypeCode: "TOTE-1", Occupied: true},
}

func newBinsRig(t *testing.T) *binsRig {
	t.Helper()
	db, counter, err := store.OpenCounting(filepath.Join(t.TempDir(), "bins.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	core, srv := newCoreStub(t, binsRigRows)
	cfg := config.Defaults()
	cfg.CoreAPI = srv.URL
	eng := engine.New(engine.Config{AppConfig: cfg, DB: db, LogFunc: t.Logf})
	dbg, err := debuglog.New(64, nil)
	if err != nil {
		t.Fatalf("debuglog: %v", err)
	}
	_, router, stop := NewRouter(eng, dbg, nil)
	t.Cleanup(stop)
	mux, ok := router.(*chi.Mux)
	if !ok {
		t.Fatalf("NewRouter returned %T, not a *chi.Mux", router)
	}
	rig := &binsRig{eng: eng, db: db, counter: counter, core: core, router: mux, stations: map[string]int64{}}
	rig.station(t, "LDR", "SMN_H1", "SMN_H2")
	rig.station(t, "UNL", "ULN_1", "ULN_2")
	nodes := []protocol.NodeInfo{}
	for _, n := range []string{"SMN_H1", "SMN_H2", "SMN_F1", "SMN_F2", "ULN_1", "ULN_2",
		"EMPTY-GRP.E1", "EMPTY-GRP.E2", "EMPTY-GRP.E3"} {
		nodes = append(nodes, protocol.NodeInfo{Name: n})
	}
	eng.ApplyNodeList(&protocol.NodeListResponse{
		Nodes: nodes,
		Loaders: []protocol.LoaderInfo{
			{
				Name: "Homes", LoaderKey: "loader:1", Role: "produce", Layout: "dedicated_positions",
				Replenishment: "operator", OutboundDest: "FG-MARKET", InboundSource: "EMPTY-SUPER",
				Positions: []protocol.LoaderPosition{
					{CoreNodeName: "SMN_H1", PayloadCode: "PART-A", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 1},
					{CoreNodeName: "SMN_H2", PayloadCode: "PART-B", HomeKind: protocol.LoaderHomeKindHome, Ordinal: 2},
					{CoreNodeName: "SMN_F1", HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 3},
					{CoreNodeName: "SMN_F2", HomeKind: protocol.LoaderHomeKindBuffer, Ordinal: 4},
				},
			},
			{
				Name: "Unloader", LoaderKey: "loader:2", Role: "consume", Layout: "shared_window",
				Replenishment: "operator", OutboundDest: "EMPTY-GRP", InboundSource: "FG-MARKET",
				Positions: []protocol.LoaderPosition{{CoreNodeName: "ULN_1", Ordinal: 1}, {CoreNodeName: "ULN_2", Ordinal: 2}},
				Payloads:  []protocol.LoaderPayloadInfo{{PayloadCode: "PART-C"}},
			},
		},
	})
	return rig
}

// station seeds a process with one station holding the named nodes.
func (r *binsRig) station(t *testing.T, code string, nodes ...string) {
	t.Helper()
	pid, err := r.db.CreateProcess(code+"-PROC", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	sid, err := r.db.CreateOperatorStation(stations.Input{ProcessID: pid, Code: code, Name: code, Sequence: 1, Enabled: true})
	if err != nil {
		t.Fatalf("create station: %v", err)
	}
	for i, n := range nodes {
		if _, err := r.db.CreateProcessNode(processes.NodeInput{
			ProcessID: pid, OperatorStationID: &sid, CoreNodeName: n, Code: n, Name: n, Sequence: i + 1, Enabled: true,
		}); err != nil {
			t.Fatalf("create node %s: %v", n, err)
		}
	}
	r.stations[code] = sid
}

// view GETs a station's view and returns its body decoded.
func (r *binsRig) view(t *testing.T, code string) map[string]any {
	t.Helper()
	resp := doRequest(t, r.router, "GET", "/api/operator-stations/"+itoa(r.stations[code])+"/view", nil, nil)
	assertStatus(t, resp, http.StatusOK)
	var out map[string]any
	decodeJSON(t, resp, &out)
	return out
}

// tileByNode is the view's tile at a core node.
func tileByNode(t *testing.T, view map[string]any, coreNode string) map[string]any {
	t.Helper()
	nodes, _ := view["nodes"].([]any)
	for _, n := range nodes {
		tile, _ := n.(map[string]any)
		if node, _ := tile["node"].(map[string]any); node != nil && node["core_node_name"] == coreNode {
			return tile
		}
	}
	t.Fatalf("no tile at %s", coreNode)
	return nil
}

func jsonOf(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

// TestStationView_OneNodeBinsReadPerBuild: a station view build reads the bins
// of its whole list in one node-bins call, the dedicated board included (two
// before: its tiles, then its buffer spots), and against a Core that speaks
// feeds that is the only call it makes to Core.
func TestStationView_OneNodeBinsReadPerBuild(t *testing.T) {
	rig := newBinsRig(t)

	// A Core with nil Feeds: the containment read stands (no claim names a
	// containment destination here, so no children read).
	for _, code := range []string{"LDR", "UNL"} {
		rig.core.take()
		rig.view(t, code)
		got := rig.core.take()
		want := map[string]int{"/api/telemetry/node-bins": 1, "/api/containment": 1}
		if jsonOf(t, got) != jsonOf(t, want) {
			t.Errorf("%s: Core calls per build against a Core without feeds = %v, want %v", code, got, want)
		}
	}

	// A Core that speaks feeds, its containment copy held: one call.
	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if err := rig.eng.ApplyContainmentSnapshot(protocol.ContainmentSnapshot{Digest: "c1",
		Flags: []protocol.PayloadContainmentRow{}, HeldBins: []protocol.HeldBinRow{},
		Destinations: []protocol.ContainmentDestination{}}); err != nil {
		t.Fatalf("apply containment: %v", err)
	}
	for _, code := range []string{"LDR", "UNL"} {
		rig.core.take()
		rig.view(t, code)
		got := rig.core.take()
		want := map[string]int{"/api/telemetry/node-bins": 1}
		if jsonOf(t, got) != jsonOf(t, want) {
			t.Errorf("%s: Core calls per build against a Core with feeds = %v, want %v", code, got, want)
		}
	}
}

// TestStationView_BinsStampedFromTheOneRead: what the one read stamps. The
// home holding a stocked PART-A bin names the buffer spot holding a stocked
// PART-A partial; the dedicated loader's first home carries its buffer strip,
// in the plant's order, and no other tile does; the unloader's board carries
// its outbound group's empties.
func TestStationView_BinsStampedFromTheOneRead(t *testing.T) {
	rig := newBinsRig(t)

	ldr := rig.view(t, "LDR")
	h1, h2 := tileByNode(t, ldr, "SMN_H1"), tileByNode(t, ldr, "SMN_H2")
	if got := jsonOf(t, h1["buffer_partial"]); got != `{"node":"SMN_F1"}` {
		t.Errorf("SMN_H1 buffer_partial = %s, want the spot holding its part", got)
	}
	if _, ok := h2["buffer_partial"]; ok {
		t.Errorf("SMN_H2 (an empty home) carries buffer_partial %s", jsonOf(t, h2["buffer_partial"]))
	}
	if _, ok := h1["has_buffer_partial"]; ok {
		t.Error("the tile still carries has_buffer_partial")
	}
	wantStrip := `[{"node":"SMN_F1","state":"partial"},{"node":"SMN_F2","state":"open"}]`
	if got := jsonOf(t, h1["loader"].(map[string]any)["buffer"]); got != wantStrip {
		t.Errorf("SMN_H1 loader.buffer = %s, want %s", got, wantStrip)
	}
	if b, ok := h2["loader"].(map[string]any)["buffer"]; ok {
		t.Errorf("SMN_H2 carries the strip too: %s (once per loader, on its first home)", jsonOf(t, b))
	}
	if bs, _ := h1["bin_state"].(map[string]any); bs == nil || bs["payload_code"] != "PART-A" || bs["stocked"] != true {
		t.Errorf("SMN_H1 bin_state = %s", jsonOf(t, h1["bin_state"]))
	}
	if _, ok := ldr["empties"]; ok {
		t.Errorf("a loader board carries empties %s", jsonOf(t, ldr["empties"]))
	}

	unl := rig.view(t, "UNL")
	want := `{"group":"EMPTY-GRP","rows":[{"bin_type":"TOTE-1","have":2,"spots":1}]}`
	if got := jsonOf(t, unl["empties"]); got != want {
		t.Errorf("UNL empties = %s, want %s", got, want)
	}
	if bs, _ := tileByNode(t, unl, "ULN_1")["bin_state"].(map[string]any); bs == nil || bs["payload_code"] != "PART-C" {
		t.Errorf("ULN_1 bin_state = %v", bs)
	}
}

// containmentRig adds a hold group HOLD-GRP with one child H1, a tile on the
// QA station, and a producing claim whose contained PART-S bins go there and
// are released to ULN-X. The bin at H1 holds PART-S.
func containmentRig(t *testing.T) *binsRig {
	t.Helper()
	rig := newBinsRig(t)
	rig.core.rows["H1"] = protocol.NodeBinInfo{NodeName: "H1", BinID: 20, BinLabel: "BIN-20", PayloadCode: "PART-S", UOPRemaining: 9, Occupied: true}
	rig.station(t, "QA", "H1")
	pid, err := rig.db.CreateProcess("PRESS-PROC", "", "", "", false)
	if err != nil {
		t.Fatalf("create process: %v", err)
	}
	styleID, err := rig.db.CreateStyle("PRESS-RUN", "", pid)
	if err != nil {
		t.Fatalf("create style: %v", err)
	}
	if _, err := rig.db.UpsertStyleNodeClaim(domain.CoreNodeKinds{}, processes.NodeClaimInput{
		StyleID: styleID, CoreNodeName: "PLN-1", Role: "produce",
		SwapMode: "two_robot_press_index", PayloadCode: "PART-S", UOPCapacity: 20,
		PairedCoreNode: "PLN-1B", OutboundDestination: "ULN-X", ContainmentDestination: "HOLD-GRP",
	}); err != nil {
		t.Fatalf("upsert claim: %v", err)
	}
	return rig
}

// TestContainmentStamp_ReadsTheHeldCopyWhenCoreSpeaksFeeds: against a Core
// that speaks feeds the station view and the Production page stamp the
// release target and the CONTAINED chip from the Edge's copy, a group
// destination's children included, and make no containment or children read.
// With nothing held there is no fallback read: the chip is not drawn and a
// tile under a group destination has no target. Against a Core with nil Feeds
// today's two reads stand.
func TestContainmentStamp_ReadsTheHeldCopyWhenCoreSpeaksFeeds(t *testing.T) {
	rig := containmentRig(t)
	stamp := func() (string, bool, map[string]int) {
		rig.core.take()
		tile := tileByNode(t, rig.view(t, "QA"), "H1")
		target, _ := tile["containment_release_target"].(string)
		flagged, _ := tile["containment_flagged"].(bool)
		return target, flagged, rig.core.take()
	}

	// A Core with nil Feeds: the containment read and one children read per
	// destination, as today (the stub's group has no children to give).
	_, _, calls := stamp()
	if want := `{"/api/containment":1,"/api/telemetry/node-bins":1,"children":1}`; jsonOf(t, calls) != want {
		t.Errorf("Core calls against a Core without feeds = %s, want %s", jsonOf(t, calls), want)
	}

	// Feeds, nothing held: no read, no chip, no group target.
	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	target, flagged, calls := stamp()
	if target != "" || flagged {
		t.Errorf("nothing held: target %q flagged %v, want neither", target, flagged)
	}
	if want := `{"/api/telemetry/node-bins":1}`; jsonOf(t, calls) != want {
		t.Errorf("Core calls with nothing held = %s, want %s", jsonOf(t, calls), want)
	}

	// Feeds, the copy held: the group's child is matched and the flag drawn.
	if err := rig.eng.ApplyContainmentSnapshot(protocol.ContainmentSnapshot{Digest: "c1",
		Flags:    []protocol.PayloadContainmentRow{{PayloadCode: "PART-S", Active: true}},
		HeldBins: []protocol.HeldBinRow{},
		Destinations: []protocol.ContainmentDestination{{Node: "HOLD-GRP", Children: []string{"HOLD-GRP.H1"},
			Bins: []protocol.ContainmentBin{}}},
	}); err != nil {
		t.Fatalf("apply containment: %v", err)
	}
	target, flagged, calls = stamp()
	if target != "ULN-X" || !flagged {
		t.Errorf("copy held: target %q flagged %v, want ULN-X and flagged", target, flagged)
	}
	if want := `{"/api/telemetry/node-bins":1}`; jsonOf(t, calls) != want {
		t.Errorf("Core calls with the copy held = %s, want %s", jsonOf(t, calls), want)
	}

	// The Production page's enrichment reads the same copy.
	views := []domain.OperatorStationView{{Nodes: []domain.StationNodeView{
		{Node: domain.Node{CoreNodeName: "H1"}, BinState: &domain.NodeBinState{Occupied: true, PayloadCode: "PART-S"}},
	}}}
	rig.core.take()
	enrichViewContainmentTargets(rig.eng, views)
	if n := views[0].Nodes[0]; n.ContainmentReleaseTarget != "ULN-X" || !n.ContainmentFlagged {
		t.Errorf("Production stamp = %q flagged %v, want ULN-X and flagged", n.ContainmentReleaseTarget, n.ContainmentFlagged)
	}
	if calls := rig.core.take(); len(calls) != 0 {
		t.Errorf("Production stamp called Core: %v", calls)
	}
}

// The engine's node-bins event reaches the station as the SSE `bins` event,
// naming the nodes and groups that changed, on the durable queue.
func TestSSE_NodeBinsEventIsSentAsBins(t *testing.T) {
	t.Parallel()
	if lossySSETopics["bins"] {
		t.Error("bins is classified lossy")
	}
	hub := NewEventHub()
	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub.SetupEngineListeners(eng)
	eng.Events.Emit(engine.Event{Type: engine.EventNodeBinsUpdated,
		Payload: engine.NodeBinsUpdatedEvent{Nodes: []string{"SMN_F1"}, Groups: []string{"GRP"}}})
	select {
	case evt := <-hub.broadcast:
		if got := jsonOf(t, evt.Data); evt.Type != "bins" || got != `{"nodes":["SMN_F1"],"groups":["GRP"]}` {
			t.Errorf("broadcast %q %s, want bins naming SMN_F1 and GRP", evt.Type, got)
		}
	default:
		t.Error("no SSE event for EventNodeBinsUpdated")
	}
}

// TestStationView_BinsFromTheCopyWhileCoreServesTheFeed walks one Edge
// through the mixed versions of the node-bins feed:
//
//   - a Core with no feeds: bins and containment by HTTP, nothing published;
//   - a Core with feeds and no BinWatch: bins by HTTP, nothing published;
//   - a Core serving the feed: the first ack publishes the watch list, and
//     until the snapshot arrives the bins are not known, with no fallback
//     read; once applied (one round trip after the first ack) the view is
//     stamped from the copy with no call to Core;
//   - Core rolled back under the running Edge: the next ack lacks the field
//     and the next build reads HTTP.
func TestStationView_BinsFromTheCopyWhileCoreServesTheFeed(t *testing.T) {
	rig := newBinsRig(t)
	var published []*protocol.Envelope
	rig.eng.SetSendFunc(func(env *protocol.Envelope) error {
		published = append(published, env)
		return nil
	})
	build := func() (map[string]any, map[string]int) {
		rig.core.take()
		v := rig.view(t, "LDR")
		return v, rig.core.take()
	}
	httpCalls := func(withContainment bool) string {
		if withContainment {
			return `{"/api/containment":1,"/api/telemetry/node-bins":1}`
		}
		return `{"/api/telemetry/node-bins":1}`
	}

	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{})
	v, calls := build()
	if jsonOf(t, calls) != httpCalls(true) || jsonOf(t, v["bins"]) != `{"known":true}` || len(published) != 0 {
		t.Errorf("no feeds: calls %s bins %s published %d", jsonOf(t, calls), jsonOf(t, v["bins"]), len(published))
	}

	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	v, calls = build()
	if jsonOf(t, calls) != httpCalls(false) || jsonOf(t, v["bins"]) != `{"known":true}` || len(published) != 0 {
		t.Errorf("feeds, no BinWatch: calls %s bins %s published %d", jsonOf(t, calls), jsonOf(t, v["bins"]), len(published))
	}

	empty := ""
	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: &empty})
	if len(published) != 1 {
		t.Fatalf("the first ack carrying BinWatch published %d envelopes, want the watch list", len(published))
	}
	v, calls = build()
	h1 := tileByNode(t, v, "SMN_H1")
	if len(calls) != 0 || jsonOf(t, v["bins"]) != `{"known":false}` || h1["bin_state"] != nil {
		t.Errorf("nothing held: calls %s bins %s SMN_H1 bin %s", jsonOf(t, calls), jsonOf(t, v["bins"]), jsonOf(t, h1["bin_state"]))
	}
	if got := jsonOf(t, h1["loader"].(map[string]any)["buffer"]); got != `[{"node":"SMN_F1","state":"unknown"},{"node":"SMN_F2","state":"unknown"}]` {
		t.Errorf("nothing held: strip %s, want every spot unknown", got)
	}

	snap := protocol.NodeBinsSnapshot{Rows: []protocol.NodeBinsRow{
		{Node: "SMN_F1", Occupied: true, BinID: 3, BinLabel: "BIN-3", BinTypeCode: "KD-1", PayloadCode: "PART-A", Stocked: true},
		{Node: "SMN_F2"},
		{Node: "SMN_H1", Occupied: true, BinID: 1, BinLabel: "BIN-1", BinTypeCode: "KD-1", PayloadCode: "PART-A", Stocked: true},
		{Node: "SMN_H2", Occupied: true, BinID: 2, BinLabel: "BIN-2", BinTypeCode: "KD-1"},
	}, Groups: []protocol.NodeBinsGroup{}}
	d, err := protocol.NodeBinsDigest(snap)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	snap.Digest = d
	if err := rig.eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	v, calls = build()
	h1 = tileByNode(t, v, "SMN_H1")
	if len(calls) != 0 || jsonOf(t, v["bins"]) != `{"known":true}` {
		t.Errorf("copy held: calls %s bins %s", jsonOf(t, calls), jsonOf(t, v["bins"]))
	}
	if got := jsonOf(t, h1["bin_state"]); got != `{"bin_id":1,"bin_label":"BIN-1","bin_type_code":"KD-1","manifest_confirmed":false,"occupied":true,"payload_code":"PART-A","stocked":true}` {
		t.Errorf("copy held: SMN_H1 bin_state %s", got)
	}
	if got := jsonOf(t, h1["buffer_partial"]); got != `{"node":"SMN_F1"}` {
		t.Errorf("copy held: buffer_partial %s", got)
	}
	if got := jsonOf(t, h1["loader"].(map[string]any)["buffer"]); got != `[{"node":"SMN_F1","state":"partial"},{"node":"SMN_F2","state":"open"}]` {
		t.Errorf("copy held: strip %s", got)
	}

	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{protocol.FeedBins: d}})
	v, calls = build()
	if jsonOf(t, calls) != httpCalls(false) || jsonOf(t, v["bins"]) != `{"known":true}` {
		t.Errorf("Core rolled back: calls %s bins %s, want the HTTP read", jsonOf(t, calls), jsonOf(t, v["bins"]))
	}
	if len(published) != 1 {
		t.Errorf("published %d envelopes in all, want the one watch list", len(published))
	}
}

// TestBinsAsOf: a held copy is drawn whatever its age; as_of is set only when
// it has not been current (arrived or confirmed) for longer than
// engine.FeedAsOfAfter.
func TestBinsAsOf(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	old := now.Add(-engine.FeedAsOfAfter - time.Second)
	fresh := now.Add(-engine.FeedAsOfAfter + time.Second)
	cases := []struct {
		name string
		st   engine.NodeBinsState
		want string
	}{
		{"nothing held", engine.NodeBinsState{}, "null"},
		{"confirmed recently", engine.NodeBinsState{Held: true, ReceivedAt: old, ConfirmedAt: fresh}, "null"},
		{"arrived recently, never confirmed", engine.NodeBinsState{Held: true, ReceivedAt: fresh}, "null"},
		{"not current for longer", engine.NodeBinsState{Held: true, ReceivedAt: old, ConfirmedAt: old}, jsonOf(t, old)},
	}
	for _, tc := range cases {
		if got := jsonOf(t, binsAsOf(tc.st, now)); got != tc.want {
			t.Errorf("%s: as_of %s, want %s", tc.name, got, tc.want)
		}
	}
}

// TestStationView_SaysWhenContainmentIsNotKnown: against a Core that speaks
// feeds, with nothing held, the view says containment is not known
// (view.containment.known false), so the station's sign can say so; with the
// copy held, or against a Core with no feeds, the key is absent.
func TestStationView_SaysWhenContainmentIsNotKnown(t *testing.T) {
	rig := containmentRig(t)
	if _, ok := rig.view(t, "QA")["containment"]; ok {
		t.Error("a Core with no feeds: the view carries containment; want it absent")
	}
	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}})
	if got := jsonOf(t, rig.view(t, "QA")["containment"]); got != `{"known":false}` {
		t.Errorf("feeds, nothing held: containment = %s, want {\"known\":false}", got)
	}
	if err := rig.eng.ApplyContainmentSnapshot(protocol.ContainmentSnapshot{Digest: "c1",
		Flags: []protocol.PayloadContainmentRow{}, HeldBins: []protocol.HeldBinRow{},
		Destinations: []protocol.ContainmentDestination{}}); err != nil {
		t.Fatalf("apply containment: %v", err)
	}
	if _, ok := rig.view(t, "QA")["containment"]; ok {
		t.Error("feeds, copy held: the view carries containment; want it absent")
	}
}

// The refusal table and sourcing events reach the stations as supply-refusal
// and sourcing, naming what changed, on the durable queue.
func TestSSE_RefusalAndSourcingEventsAreSent(t *testing.T) {
	t.Parallel()
	for _, topic := range []string{"supply-refusal", "sourcing"} {
		if lossySSETopics[topic] {
			t.Errorf("%s is classified lossy", topic)
		}
	}
	hub := NewEventHub()
	eng := &engine.Engine{Events: engine.NewEventBus()}
	hub.SetupEngineListeners(eng)
	for _, tc := range []struct {
		evt  engine.Event
		typ  string
		data string
	}{
		{engine.Event{Type: engine.EventSupplyRefusalChanged,
			Payload: engine.SupplyRefusalChangedEvent{LoaderNode: "SMN_A1", PayloadCode: "PART-A"}},
			"supply-refusal", `{"loader_node":"SMN_A1","payload_code":"PART-A"}`},
		{engine.Event{Type: engine.EventSourcingStateChanged,
			Payload: engine.SourcingStateChangedEvent{Processes: []string{"PRESS-1"}}},
			"sourcing", `{"processes":["PRESS-1"]}`},
	} {
		eng.Events.Emit(tc.evt)
		select {
		case evt := <-hub.broadcast:
			if got := jsonOf(t, evt.Data); evt.Type != tc.typ || got != tc.data {
				t.Errorf("broadcast %q %s, want %s %s", evt.Type, got, tc.typ, tc.data)
			}
		default:
			t.Errorf("no SSE event for %s", tc.typ)
		}
	}
}

// TestStationView_AssignmentsAndHeightsFromTheCopy: from the Edge's copy of a
// Core that serves the feed, an unloader's view carries each window's
// assignment, each cart's tote numbers on its bin, and the empties' wants per
// type and per wanted height; over HTTP (a Core that sends no group lines)
// it carries the tote numbers and nothing else of it.
func TestStationView_AssignmentsAndHeightsFromTheCopy(t *testing.T) {
	rig := newBinsRig(t)
	rig.eng.SetSendFunc(func(*protocol.Envelope) error { return nil })
	n := func(v int) *int { return &v }
	rig.core.rows["ULN_1"] = protocol.NodeBinInfo{NodeName: "ULN_1", BinID: 4, BinLabel: "CART-4", BinTypeCode: "TOTE-1",
		PayloadCode: "PART-C", UOPRemaining: 40, Occupied: true, TotesOnCart: n(16), TotesPerLayer: n(8)}

	v := rig.view(t, "UNL")
	if got := jsonOf(t, tileByNode(t, v, "ULN_1")["bin_state"].(map[string]any)["totes_on_cart"]); got != "16" {
		t.Errorf("over HTTP: ULN_1 totes_on_cart %s, want 16", got)
	}
	if a, ok := tileByNode(t, v, "ULN_1")["assignment"]; ok {
		t.Errorf("over HTTP: an assignment %s", jsonOf(t, a))
	}

	empty := ""
	rig.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{Feeds: map[string]string{}, BinWatch: &empty})
	snap := protocol.NodeBinsSnapshot{Rows: []protocol.NodeBinsRow{
		{Node: "E1", Occupied: true, BinID: 5, BinTypeCode: "TOTE-1", TotesOnCart: n(24), TotesPerLayer: n(8)},
		{Node: "E2"},
		{Node: "E3"},
		{Node: "ULN_1", Occupied: true, BinID: 4, BinLabel: "CART-4", BinTypeCode: "TOTE-1", PayloadCode: "PART-C",
			Stocked: true, TotesOnCart: n(16), TotesPerLayer: n(8)},
		{Node: "ULN_2"},
	}, Groups: []protocol.NodeBinsGroup{{Group: "EMPTY-GRP",
		Wants:       []protocol.NodeBinsWant{{BinType: "TOTE-1", Want: 2}, {BinType: "TOTE-1", Layers: n(3), Want: 1}},
		Assignments: []protocol.NodeBinsAssignment{{Node: "ULN_1", BinType: "TOTE-1", Layers: n(3)}},
	}}}
	d, err := protocol.NodeBinsDigest(snap)
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	snap.Digest = d
	if err := rig.eng.ApplyNodeBinsSnapshot(snap); err != nil {
		t.Fatalf("apply: %v", err)
	}
	v = rig.view(t, "UNL")
	u1 := tileByNode(t, v, "ULN_1")
	if got := jsonOf(t, u1["assignment"]); got != `{"bin_type":"TOTE-1","layers":3}` {
		t.Errorf("copy: ULN_1 assignment %s", got)
	}
	if got := jsonOf(t, u1["bin_state"].(map[string]any)["totes_per_layer"]); got != "8" {
		t.Errorf("copy: ULN_1 totes_per_layer %s, want 8", got)
	}
	want := `{"group":"EMPTY-GRP","rows":[{"bin_type":"TOTE-1","have":1,"spots":2,"want":2},` +
		`{"bin_type":"TOTE-1","have":1,"layers":3,"spots":2,"want":1}]}`
	if got := jsonOf(t, v["empties"]); got != want {
		t.Errorf("copy: empties %s\nwant %s", got, want)
	}
}
