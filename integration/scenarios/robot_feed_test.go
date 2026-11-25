// The live robot feed end to end across the two modules (shared-map brief step
// 8): an Edge's robot.watch reaches Core, Core's robot poll answers it with
// robot.positions built from the fleet and the stored scene, and the Edge holds
// the latest position per order and hands it to its subscriber.
//
// Core is the real engine and its real 2 s robot poll, over a fleet that reports
// what the simulator cannot (LEAD-NOTES "Sim cannot show a path"): robots at
// positions, with the route RDS has left them as bare point numbers. The scene
// they are named from is synthetic. The capability travels as production
// carries it, on Core's heartbeat ack over the bus; the ephemeral topic is
// harness.EphemeralTopic, which keeps Kafka's two properties that matter here
// (one partition; readers with no group open at the last offset) and carries
// the signed bytes each side's real decode reads.
//
//go:build docker

package scenarios

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"shingo/integration/harness"
	"shingo/protocol"
	"shingo/protocol/router"

	coreconfig "shingocore/config"
	coreengine "shingocore/engine"
	"shingocore/fleet"
	"shingocore/fleet/simulator"
	corestore "shingocore/store"
	coreorders "shingocore/store/orders"
	coreharness "shingocore/testharness"

	edgeengine "shingoedge/engine"
	edgeharness "shingoedge/testharness"
)

const (
	feedStation = "edge.feed"
	feedOther   = "edge.other"
	feedTopic   = "shingo.ephemeral"
)

var feedKey = []byte("feed-test-signing-key")

// feedRoute is a synthetic drive of 22 points, LM101 → LM122, and feedIDs the
// bare numbers RDS reports the same route as. Invented names on an invented
// line, the length of a real drive (U4 measured on the same shape).
func feedRoute() (names, ids []string) {
	for i := 101; i <= 122; i++ {
		names = append(names, fmt.Sprintf("LM%d", i))
		ids = append(ids, fmt.Sprintf("%d", i))
	}
	return names, ids
}

// pathFleet is the simulator, reporting the robots the scenario places.
type pathFleet struct {
	*simulator.SimulatorBackend
	mu     sync.Mutex
	robots []fleet.RobotStatus
}

var _ fleet.RobotLister = (*pathFleet)(nil)

func (f *pathFleet) GetRobotsStatus() ([]fleet.RobotStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fleet.RobotStatus, len(f.robots))
	for i, r := range f.robots {
		r.PathIDs = slices.Clone(r.PathIDs)
		out[i] = r
	}
	return out, nil
}
func (f *pathFleet) SetAvailability(string, bool) error { return nil }
func (f *pathFleet) RetryFailed(string) error           { return nil }
func (f *pathFleet) ForceComplete(string) error         { return nil }

// feedCore is a running Core, its data service and tables, and the topic.
type feedCore struct {
	db    *corestore.DB
	eng   *coreengine.Engine
	side  *coreharness.Core
	topic *harness.EphemeralTopic
	uuids map[string]string // order name → its uuid
}

// startFeedCore starts Core with the scene synced and three orders: "own" of
// feedStation on AMR-01 (driving the 22-point route), "theirs" of feedOther on
// AMR-02, and "unpolled" of feedStation on a robot the fleet does not list.
// verified says whether this Core holds a verified ephemeral topic: one that
// does publishes, reads and names it; one that does not is the older Core.
func startFeedCore(t *testing.T, verified bool) *feedCore {
	t.Helper()
	db := coreharness.OpenDB(t)
	names, ids := feedRoute()
	fl := &pathFleet{SimulatorBackend: simulator.New(), robots: []fleet.RobotStatus{
		{VehicleID: "AMR-01", Connected: true, X: 12.5, Y: -3.25, Angle: 1.5708, PathIDs: ids},
		{VehicleID: "AMR-02", Connected: true, X: 40, Y: 8, PathIDs: ids[:2]},
	}}
	cfg := coreconfig.Defaults()
	cfg.Messaging.StationID = "core"
	eng := coreengine.New(coreengine.Config{AppConfig: cfg, DB: db, Fleet: fl, LogFunc: t.Logf})
	eng.Start()
	t.Cleanup(eng.Stop)

	var area fleet.SceneArea
	area.Name = "AREA-FEED"
	for i, n := range names {
		area.AdvancedPoints = append(area.AdvancedPoints, fleet.ScenePoint{ClassName: "LocationMark", InstanceName: n,
			PosX: float64(i), PropertiesJSON: "[]"})
		if i > 0 {
			area.Edges = append(area.Edges, fleet.SceneEdge{ClassName: "StraightPath", InstanceName: names[i-1] + "-" + n,
				FromName: names[i-1], ToName: n, FromX: float64(i - 1), ToX: float64(i)})
		}
	}
	eng.SyncScenePoints([]fleet.SceneArea{area})

	c := &feedCore{db: db, eng: eng, topic: harness.NewEphemeralTopic(feedKey), uuids: map[string]string{}}
	// Full-length uuids (synthetic), so the bytes measured are a real watch's.
	for i, o := range []struct{ name, station, robot string }{
		{"own", feedStation, "AMR-01"}, {"theirs", feedOther, "AMR-02"}, {"unpolled", feedStation, "AMR-09"},
	} {
		order := &coreorders.Order{EdgeUUID: fmt.Sprintf("00000000-0000-4000-8000-%012d", i+1), StationUID: o.station, OrderType: "move",
			Status: protocol.StatusInTransit, Quantity: 1, SourceNode: "FEED-A", DeliveryNode: "FEED-B"}
		mustNil(t, db.CreateOrder(order), "order "+o.name)
		mustNil(t, db.UpdateOrderVendor(order.ID, "v-"+o.name, "RUNNING", o.robot), "robot on "+o.name)
		c.uuids[o.name] = order.EdgeUUID
	}

	// Core's tables as main builds them, less what this cell leaves out: every
	// type and subject but the heartbeat, and every hook. The topic and its
	// reader are the Kafka client's in main, so the cell sets them itself.
	c.side = coreharness.NewCore(t, eng, coreharness.RoutesOnly(protocol.SubjectEdgeHeartbeat), coreharness.HooksOnly())
	if verified {
		c.side.Data.SetEphemeralTopic(func() string { return feedTopic })
		eng.SetRobotPositionsPublisher(c.topic.Publish)
		// Core's reader, as routers.go builds it (buildEphemeralReader).
		watches := router.New[string]()
		router.Register(watches, protocol.TypeRobotWatch, eng.HandleRobotWatch)
		c.topic.Reader(router.ReadEphemeral(feedKey, func(h *protocol.RawHeader) bool { return h.Dst.Role == protocol.RoleCore }, watches))
	}
	return c
}

// feedEdge is one Edge on the feed: its engine, its link onto the topic, what
// its reader was handed (by envelope id) and what its subscriber got.
type feedEdge struct {
	edge *edgeharness.Edge
	link *harness.EphemeralLink

	mu       sync.Mutex
	handed   map[string]bool
	got      []protocol.RobotPositions
	logLines []string
}

// startFeedEdge starts an Edge configured for the topic, wired as main.go
// wires it, and has Core's heartbeat ack reach it over the bus: the ack is
// what names the topic, or does not.
func startFeedEdge(t *testing.T, core *feedCore) *feedEdge {
	t.Helper()
	fe := &feedEdge{handed: map[string]bool{}}
	fe.edge = edgeharness.NewEdge(t, feedStation, func(c *edgeengine.Config) {
		c.AppConfig.Messaging.EphemeralTopic = feedTopic
		c.LogFunc = func(f string, a ...any) {
			fe.mu.Lock()
			fe.logLines = append(fe.logLines, fmt.Sprintf(f, a...))
			fe.mu.Unlock()
		}
	})
	eng := fe.edge.Engine
	// The Edge's reader, as main.go builds it.
	positions := router.New[string]()
	router.Register(positions, protocol.TypeRobotPositions, eng.HandleRobotPositions)
	read := router.ReadEphemeral(feedKey, func(h *protocol.RawHeader) bool { return h.Dst.Station == feedStation }, positions)
	fe.link = &harness.EphemeralLink{Topic: core.topic, Deliver: func(raw []byte) {
		if hdr, err := protocol.ParseHeader(raw, feedKey); err == nil {
			fe.mu.Lock()
			fe.handed[hdr.ID] = true
			fe.mu.Unlock()
		}
		read(raw)
	}}
	eng.SetRobotPositionsSubscriber(func(p protocol.RobotPositions) {
		fe.mu.Lock()
		fe.got = append(fe.got, p)
		fe.mu.Unlock()
	}, nil)
	eng.SetEphemeralLink(fe.link)

	// The heartbeat and its ack over the bus, routed as the two composition
	// roots route them: Core's by NewCore's tables (startFeedCore).
	edgeSubjects := router.NewSubject()
	router.RegisterSubject(edgeSubjects, protocol.SubjectEdgeHeartbeatAck, func(_ *protocol.Envelope, ack *protocol.EdgeHeartbeatAck) {
		eng.OnCoreAck(ack)
	})
	edgeIngestor := protocol.NewIngestor(func(h *protocol.RawHeader) bool { return h.Dst.Station == feedStation })
	edgeRouter := router.New[string]()
	router.Register(edgeRouter, protocol.TypeData, func(env *protocol.Envelope, p *protocol.Data) { edgeSubjects.Dispatch(env, p) })
	edgeIngestor.Dispatch = func(env *protocol.Envelope) { edgeRouter.Dispatch(env, env.Type) }
	bus := harness.NewBus(t,
		harness.EdgeSide{EdgeStore: fe.edge.DB, EdgeIngestor: edgeIngestor},
		harness.CoreSide{CoreStore: core.db, CoreIngestor: core.side.Ingestor})
	drainOutbox(t, fe.edge)
	hb, err := protocol.NewDataEnvelope(protocol.SubjectEdgeHeartbeat,
		protocol.Address{Role: protocol.RoleEdge, Station: feedStation}, protocol.Address{Role: protocol.RoleCore},
		&protocol.EdgeHeartbeat{StationID: feedStation, Feeds: map[string]string{}})
	mustNil(t, err, "build heartbeat")
	data, err := hb.Encode()
	mustNil(t, err, "encode heartbeat")
	_, err = fe.edge.DB.EnqueueOutbox(data, "data."+protocol.SubjectEdgeHeartbeat)
	mustNil(t, err, "enqueue heartbeat")
	bus.PumpAll()
	return fe
}

// firstWith waits up to timeout for a message the subscriber got after skip
// that carries uuid, and returns it.
func (fe *feedEdge) firstWith(t *testing.T, skip int, uuid string, timeout time.Duration) (protocol.RobotPositions, int) {
	t.Helper()
	for deadline := time.Now().Add(timeout); time.Now().Before(deadline); time.Sleep(50 * time.Millisecond) {
		fe.mu.Lock()
		for i := skip; i < len(fe.got); i++ {
			for _, o := range fe.got[i].Orders {
				if o.OrderUUID == uuid {
					p := fe.got[i]
					fe.mu.Unlock()
					return p, i
				}
			}
		}
		fe.mu.Unlock()
	}
	t.Fatalf("no robot.positions carrying %s reached the Edge in %s", uuid, timeout)
	return protocol.RobotPositions{}, 0
}

// positionsTo is every robot.positions on the topic addressed to station.
func positionsTo(topic *harness.EphemeralTopic, station string) []harness.EphemeralMessage {
	var out []harness.EphemeralMessage
	for _, m := range topic.Messages() {
		if m.Header.Type == protocol.TypeRobotPositions && m.Header.Dst.Station == station {
			out = append(out, m)
		}
	}
	return out
}

// TestScenario_RobotFeed_WatchPositionsRoundTripWithThePath pins the round
// trip and its content: the Edge's watch (the union over two operator
// stations) reaches Core, and Core's poll sends back, for the watching station
// alone, the order that is its own on a robot the fleet lists, with the robot's
// position, heading and the full route named from the stored scene in order;
// not another station's order, and not an order whose robot the poll did not
// list. Then an Edge outage: Core keeps sending on its lease, and the Edge that
// comes back is handed nothing published while it was gone. A station that
// never watched is sent nothing. Measures the feed's bytes per message.
func TestScenario_RobotFeed_WatchPositionsRoundTripWithThePath(t *testing.T) {
	t.Parallel()
	core := startFeedCore(t, true)
	fe := startFeedEdge(t, core)
	names, _ := feedRoute()

	fe.edge.Engine.SetRobotWatch(1, []string{core.uuids["own"], core.uuids["theirs"]})
	fe.edge.Engine.SetRobotWatch(2, []string{core.uuids["unpolled"]})
	p, _ := fe.firstWith(t, 0, core.uuids["own"], 10*time.Second)

	if len(p.Orders) != 1 {
		t.Fatalf("the station's message carries %+v, want its own polled order alone", p.Orders)
	}
	o := p.Orders[0]
	if o.RobotID != "AMR-01" || o.X != 12.5 || o.Y != -3.25 || o.Heading != 1.5708 {
		t.Errorf("own order = %+v, want AMR-01 at (12.5, -3.25) heading 1.5708", o)
	}
	if !slices.Equal(o.Path, names) {
		t.Errorf("own order's path = %v, want the 22 scene names LM101…LM122 in order", o.Path)
	}
	if p.IntervalMS != 2000 || p.T.IsZero() {
		t.Errorf("t/interval = %s/%d, want the poll's time and 2000", p.T, p.IntervalMS)
	}
	if h, ok := fe.edge.Engine.RobotPosition(core.uuids["own"]); !ok || !slices.Equal(h.Position.Path, names) {
		t.Errorf("the Edge holds %+v (%v) for its order, want the latest position with its path", h, ok)
	}

	var watch protocol.RobotWatch
	for _, m := range core.topic.Messages() {
		if m.Header.Type == protocol.TypeRobotWatch {
			env, ok := protocol.DecodeEphemeral(m.Raw, feedKey, nil)
			if ok {
				mustNil(t, env.DecodePayload(&watch), "decode watch")
			}
		}
	}
	want := []string{core.uuids["own"], core.uuids["theirs"], core.uuids["unpolled"]}
	slices.Sort(want)
	if watch.StationID != feedStation || !slices.Equal(watch.OrderUUIDs, want) {
		t.Errorf("the last watch = %+v, want %s watching the union %v", watch, feedStation, want)
	}

	// ── An Edge outage ──
	fe.edge.Engine.Stop()
	outageFrom := len(core.topic.Messages())
	deadline := time.Now().Add(8 * time.Second)
	for len(positionsTo(core.topic, feedStation)) == 0 ||
		positionsTo(core.topic, feedStation)[len(positionsTo(core.topic, feedStation))-1].Offset < outageFrom {
		if time.Now().After(deadline) {
			t.Fatal("Core sent nothing on the lease while the Edge was gone; the outage proves nothing")
		}
		time.Sleep(50 * time.Millisecond)
	}
	var sentDuring []string
	for _, m := range positionsTo(core.topic, feedStation) {
		if m.Offset >= outageFrom {
			sentDuring = append(sentDuring, m.Header.ID)
		}
	}

	back := startFeedEdge(t, core)
	back.edge.Engine.SetRobotWatch(1, []string{core.uuids["own"]})
	back.firstWith(t, 0, core.uuids["own"], 10*time.Second)
	back.mu.Lock()
	for _, id := range sentDuring {
		if back.handed[id] {
			t.Errorf("the Edge that came back was handed %s, published while it was gone: a replay", id)
		}
	}
	back.mu.Unlock()

	if n := len(positionsTo(core.topic, feedOther)); n != 0 {
		t.Errorf("Core sent %d robot.positions to %s, which never watched; want none", n, feedOther)
	}

	// The bytes and the rate, as the topic carried them (signed), over the run.
	measureFeed(t, core.topic, o)
}

// measureFeed logs the feed's bytes per message and per minute, from what the
// topic carried: one station watching one driving robot on a 22-point route.
func measureFeed(t *testing.T, topic *harness.EphemeralTopic, one protocol.RobotPosition) {
	t.Helper()
	var positions, watch, sent int
	var first, last time.Time
	for _, m := range topic.Messages() {
		switch m.Header.Type {
		case protocol.TypeRobotPositions:
			var p protocol.RobotPositions
			if env, ok := protocol.DecodeEphemeral(m.Raw, feedKey, nil); ok && env.DecodePayload(&p) == nil && len(p.Orders) == 1 {
				positions = len(m.Raw)
				if first.IsZero() {
					first = p.T
				}
				last = p.T
				sent++
			}
		case protocol.TypeRobotWatch:
			if watch == 0 { // the first Edge's, three orders
				watch = len(m.Raw)
			}
		}
	}
	entry, err := json.Marshal(one)
	mustNil(t, err, "marshal entry")
	perMin := int(time.Minute / (2 * time.Second))
	t.Logf("robot.positions, one driving robot, 22-point path: %d B signed (the order entry %d B, the rest %d B); "+
		"%d per minute per watching station = %d B/min; each further driving robot on the station adds %d B "+
		"(+%d B/min at this path length). robot.watch, three orders: %d B, %d per minute per Edge = %d B/min. "+
		"No watch: 0 messages, 0 B. Observed: %d robot.positions to the station over %s of polls.",
		positions, len(entry), positions-len(entry), perMin, perMin*positions, len(entry)+1, perMin*(len(entry)+1),
		watch, int(time.Minute/protocol.RobotWatchInterval), int(time.Minute/protocol.RobotWatchInterval)*watch, sent, last.Sub(first))
}

// TestScenario_RobotFeed_AnOlderCoreIsNeverTouched pins the mixed-version
// story's other half: a new Edge against a Core that names no ephemeral topic
// (an older Core, or one that could not verify it) opens no reader, sends no
// watch however many orders its stations show, and logs nothing about it.
func TestScenario_RobotFeed_AnOlderCoreIsNeverTouched(t *testing.T) {
	t.Parallel()
	core := startFeedCore(t, false)
	fe := startFeedEdge(t, core)
	fe.edge.Engine.SetRobotWatch(1, []string{core.uuids["own"]})
	time.Sleep(2*time.Second + 500*time.Millisecond) // past a robot poll and the watch's first send

	if n := fe.link.Opens(); n != 0 {
		t.Errorf("the Edge opened %d readers of a topic Core never named, want none", n)
	}
	if n := len(core.topic.Messages()); n != 0 {
		t.Errorf("%d messages reached the topic, want none: no watch, so nothing from Core either", n)
	}
	if fe.edge.Engine.RobotPositionsAvailable() {
		t.Error("the feed reads available against a Core that names no topic")
	}
	fe.mu.Lock()
	defer fe.mu.Unlock()
	for _, l := range fe.logLines {
		if strings.Contains(l, "robot feed") || strings.Contains(l, "ephemeral") {
			t.Errorf("the Edge logged %q against an older Core; it must say nothing", l)
		}
	}
}
