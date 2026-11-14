package www

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"shingo/protocol"
	"shingoedge/config"
	"shingoedge/engine"
	"shingoedge/internal/testdb"
	"shingoedge/store"
)

// robot_stream_test.go — the robot panel's stream (robot_stream.go) over the
// real engine: a started engine on SQLite whose ephemeral link is recorded,
// the real hub serving /events, and the stream wired as NewRouter wires it.

// robotLink is the engine's ephemeral link, recorded: what the robot watch
// sender sends.
type robotLink struct {
	mu      sync.Mutex
	open    bool
	watches []protocol.RobotWatch
}

func (l *robotLink) Start(string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open = true
	return nil
}

func (l *robotLink) Stop() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.open = false
}

func (l *robotLink) Publish(env *protocol.Envelope) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.open || env.Type != protocol.TypeRobotWatch {
		return fmt.Errorf("published %s on a link open=%v", env.Type, l.open)
	}
	var w protocol.RobotWatch
	if err := env.DecodePayload(&w); err != nil {
		return err
	}
	l.watches = append(l.watches, w)
	return nil
}

// sent is how many watches went and the last one's uuids.
func (l *robotLink) sent() (int, []string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.watches) == 0 {
		return 0, nil
	}
	return len(l.watches), slices.Clone(l.watches[len(l.watches)-1].OrderUUIDs)
}

// robotRig is one plant: operator stations n and m on one process, their
// orders, and the stream serving /events.
type robotRig struct {
	t    *testing.T
	eng  *engine.Engine
	db   *store.DB
	link *robotLink
	hub  *EventHub
	url  string
	n, m int64
	// uuid is each seeded order's Core uuid by its name; id its Edge id.
	uuid map[string]string
	id   map[string]int64
}

// newRobotRig builds the plant (robotRigOrders), starts the engine and the
// stream, and has Core name the ephemeral topic unless closed is set. now is
// the stream's clock (nil: time.Now).
func newRobotRig(t *testing.T, now func() time.Time, closed bool) *robotRig {
	t.Helper()
	db := testdb.Open(t)
	r := &robotRig{t: t, db: db, link: &robotLink{}, uuid: map[string]string{}, id: map[string]int64{}}
	r.seed()
	r.eng = engine.New(engine.Config{AppConfig: config.Defaults(), DB: db, LogFunc: func(string, ...any) {}})
	r.eng.SetEphemeralLink(r.link)
	r.eng.Start()
	t.Cleanup(r.eng.Stop)
	r.hub = NewEventHub()
	r.hub.Start()
	t.Cleanup(r.hub.Stop)
	// As NewRouter wires it, with a short burst window.
	stream := newRobotStream(r.eng, r.eng.StationService().RobotWatchOrders, r.hub.Broadcast, 10*time.Millisecond, r.hub.stopChan)
	if now != nil {
		stream.now = now
	}
	r.hub.robotPos = stream
	stream.start(r.eng.Events)
	srv := httptest.NewServer(http.HandlerFunc(r.hub.HandleSSE))
	t.Cleanup(srv.Close)
	r.url = srv.URL + "/events"
	if !closed {
		r.ack("shingo.ephemeral")
		waitTrue(t, "the link opened on Core's topic", r.eng.RobotPositionsAvailable)
	}
	return r
}

// robotRigOrders: name → (station, position, source, status). "both" sits at
// M's position and sources from N's, so both boards draw it.
var robotRigOrders = []struct {
	name, station, node, source string
	status                      protocol.Status
}{
	{"n-1", "n", "RN01", "", protocol.StatusDispatched},
	{"n-2", "n", "RN02", "", protocol.StatusInTransit},
	{"n-queued", "n", "RN01", "", protocol.StatusQueued},
	{"m-1", "m", "RM01", "", protocol.StatusStaged},
	{"both", "m", "RM01", "RN02", protocol.StatusInTransit},
}

func (r *robotRig) seed() {
	t := r.t
	pid := seedProcess(t, r.db, "RobotLine")
	r.n = seedOperatorStation(t, r.db, pid, "OS-ROBOT-N", "RobotN")
	r.m = seedOperatorStation(t, r.db, pid, "OS-ROBOT-M", "RobotM")
	nodes := map[string]int64{
		"RN01": seedProcessNode(t, r.db, pid, r.n, "RN01"),
		"RN02": seedProcessNode(t, r.db, pid, r.n, "RN02"),
		"RM01": seedProcessNode(t, r.db, pid, r.m, "RM01"),
	}
	for i, o := range robotRigOrders {
		// Full-length uuids, as the Edge mints them, so the frames measure true.
		uuid := fmt.Sprintf("%08d-0000-4000-8000-%012d", i+1, i+1)
		pn := nodes[o.node]
		id, err := r.db.CreateOrder(uuid, protocol.OrderTypeRetrieve, &pn, false, 1, o.node, "", o.source, protocol.Height{}, false, "", "", "")
		if err != nil {
			t.Fatalf("seed order %s: %v", o.name, err)
		}
		if err := r.db.UpdateOrderStatus(id, string(o.status)); err != nil {
			t.Fatalf("seed order %s status: %v", o.name, err)
		}
		r.uuid[o.name], r.id[o.name] = uuid, id
	}
}

// ack is a heartbeat ack from Core naming topic ("" names none).
func (r *robotRig) ack(topic string) {
	r.eng.OnCoreAck(&protocol.EdgeHeartbeatAck{ServerTS: time.Now().UTC(), Feeds: map[string]string{}, EphemeralTopic: topic})
}

// uuids is the named orders' uuids, sorted, as a watch carries them.
func (r *robotRig) uuids(names ...string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, r.uuid[n])
	}
	slices.Sort(out)
	return out
}

// panel opens station's robot-pos stream, as the panel's own EventSource
// does, and returns its frames after connected and its first frame's body.
func (r *robotRig) panel(station int64) (<-chan sseFrame, string, context.CancelFunc) {
	r.t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	r.t.Cleanup(cancel)
	ch := sseFrames(r.t, ctx, r.url+"?topics="+robotPosTopic(station))
	if _, ok := nextFrame(r.t, ch, "connected", 2*time.Second); !ok {
		r.t.Fatalf("station %d's panel: no connected frame", station)
	}
	first, ok := nextFrame(r.t, ch, robotPosTopic(station), 2*time.Second)
	if !ok {
		r.t.Fatalf("station %d's panel: no first frame", station)
	}
	return ch, first.data, cancel
}

// watching waits until the last watch sent carries exactly want.
func (r *robotRig) watching(what string, want []string) {
	r.t.Helper()
	waitTrue(r.t, what, func() bool { _, last := r.link.sent(); return slices.Equal(last, want) })
}

// position is a robot.positions message for the named orders, each on its
// own robot, with a 22-point path.
func (r *robotRig) position(at time.Time, names ...string) *protocol.RobotPositions {
	p := &protocol.RobotPositions{T: at, IntervalMS: 2000}
	for i, n := range names {
		var path []string
		for k := range 22 {
			path = append(path, fmt.Sprintf("LM%d", 101+k))
		}
		p.Orders = append(p.Orders, protocol.RobotPosition{
			OrderUUID: r.uuid[n], RobotID: fmt.Sprintf("AMR-%02d", i+1),
			X: 12.345 + float64(i), Y: 67.891, Heading: 1.5708, Path: path,
		})
	}
	return p
}

// waitTrue waits up to 3 s for cond.
func waitTrue(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); !cond(); {
		if time.Now().After(deadline) {
			t.Fatalf("after 3 s: %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRobotStream_OpenWatchesCloseClears pins the watch lifecycle on the real
// engine. Nothing is watched while no panel is open. A panel's first client
// sets its station's set (the board's tracked orders, never another
// station's); a second client of the same station shares it; the last client
// to close clears it, the held positions go, and the Edge's sender sends no
// further watch: Core's lease lapses.
func TestRobotStream_OpenWatchesCloseClears(t *testing.T) {
	t.Parallel()
	r := newRobotRig(t, nil, false)
	time.Sleep(100 * time.Millisecond)
	if n, _ := r.link.sent(); n != 0 {
		t.Fatalf("%d watches sent with no panel open; hidden is zero work", n)
	}

	_, firstM, closeM := r.panel(r.m)
	if firstM != `{"available":true}` {
		t.Errorf("M's first frame %s, want {\"available\":true}", firstM)
	}
	r.watching("M's panel watches M's board alone", r.uuids("m-1", "both"))

	_, _, closeA := r.panel(r.n)
	r.watching("N's panel adds N's board", r.uuids("m-1", "both", "n-1", "n-2"))
	_, _, closeB := r.panel(r.n)

	closeM()
	r.watching("M's panel closed: N's set alone", r.uuids("both", "n-1", "n-2"))
	closeA()
	r.eng.HandleRobotPositions(nil, r.position(time.Now().UTC(), "n-1"))
	time.Sleep(50 * time.Millisecond)
	if _, last := r.link.sent(); !slices.Equal(last, r.uuids("both", "n-1", "n-2")) {
		t.Fatalf("one of N's two panels closed and the watch is %v; the other still shows the set", last)
	}
	if _, held := r.eng.RobotPosition(r.uuid["n-1"]); !held {
		t.Fatal("with N's second panel open, n-1's position was not held")
	}

	closeB()
	waitTrue(t, "N's last panel closed: the set cleared and the held position gone", func() bool {
		_, held := r.eng.RobotPosition(r.uuid["n-1"])
		return !held
	})
	// A send the sender had already begun may still land; none may follow it.
	time.Sleep(50 * time.Millisecond)
	before, _ := r.link.sent()
	time.Sleep(protocol.RobotWatchInterval + time.Second)
	if after, last := r.link.sent(); after != before {
		t.Errorf("after the last panel closed %d more watches went (last %v); the sender stops and Core's lease lapses",
			after-before, last)
	}
}

// TestRobotStream_FramesJoinTheEdgeOrderAndNoClientNoFrame: one robot-pos
// frame per robot.positions message that carries any of the station's
// orders, each order with this Edge's order id and the position's robot; none
// for a message that carries none; none at all, built or sent, for a station
// with no open panel, even for an order on its board; and a status crossing
// into the tracked set widens the watch while the panel is open. Bytes on the
// wire, SSE framing included, are logged for one order and for a swap.
func TestRobotStream_FramesJoinTheEdgeOrderAndNoClientNoFrame(t *testing.T) {
	t.Parallel()
	r := newRobotRig(t, nil, false)
	// A probe on M's topic that is no panel: the hub would hand it any frame
	// built for M, but the stream is never told of it.
	probe := &sseClient{events: make(chan SSEEvent, 64), lossy: make(chan SSEEvent, 64),
		topics: map[string]bool{robotPosTopic(r.m): true}}
	r.hub.register(probe)
	defer r.hub.unregister(probe)

	at := time.Date(2026, 11, 14, 15, 4, 5, 678_000_000, time.UTC)
	r.eng.HandleRobotPositions(nil, r.position(at, "n-1", "both"))
	ch, _, _ := r.panel(r.n)
	r.watching("N's panel watches N's board", r.uuids("both", "n-1", "n-2"))

	// One order: n-1 and two others the watch does not hold.
	one := r.position(at, "n-1", "m-1", "n-queued")
	r.eng.HandleRobotPositions(nil, one)
	f, ok := nextFrame(t, ch, robotPosTopic(r.n), 2*time.Second)
	if !ok {
		t.Fatal("N's panel got no frame for a message carrying n-1")
	}
	var got robotPosFrame
	if err := json.Unmarshal([]byte(f.data), &got); err != nil {
		t.Fatalf("decode %s: %v", f.data, err)
	}
	want := robotPosFrame{T: at, IntervalMS: 2000, Orders: []robotPosOrder{{OrderID: r.id["n-1"], RobotPosition: one.Orders[0]}}}
	if !got.T.Equal(want.T) || got.IntervalMS != want.IntervalMS || len(got.Orders) != 1 ||
		got.Orders[0].OrderID != want.Orders[0].OrderID || got.Orders[0].OrderUUID != r.uuid["n-1"] ||
		got.Orders[0].RobotID != "AMR-01" || !slices.Equal(got.Orders[0].Path, one.Orders[0].Path) {
		t.Fatalf("frame %s, want n-1 alone joined to Edge order %d on AMR-01", f.data, r.id["n-1"])
	}
	t.Logf("one order, 22-point path: %d bytes on the wire (SSE framing, id %s)", f.wire, f.id)

	// A swap: both of N's legs on one frame.
	r.eng.HandleRobotPositions(nil, r.position(at.Add(2*time.Second), "n-1", "n-2"))
	swap, ok := nextFrame(t, ch, robotPosTopic(r.n), 2*time.Second)
	if !ok {
		t.Fatal("N's panel got no frame for the swap")
	}
	if err := json.Unmarshal([]byte(swap.data), &got); err != nil || len(got.Orders) != 2 {
		t.Fatalf("swap frame %s (%v), want both legs", swap.data, err)
	}
	t.Logf("a swap, two orders, 22-point paths: %d bytes on the wire (SSE framing, id %s)", swap.wire, swap.id)

	// The order on both boards: N's panel takes it; M has no panel, so the
	// stream builds nothing for M though the watch holds the order.
	r.eng.HandleRobotPositions(nil, r.position(at.Add(4*time.Second), "both"))
	if f, ok := nextFrame(t, ch, robotPosTopic(r.n), 2*time.Second); !ok || !json.Valid([]byte(f.data)) {
		t.Fatal("N's panel lost the order its board shares with M's")
	}
	// A message with none of N's orders is no frame.
	r.eng.HandleRobotPositions(nil, r.position(at.Add(6*time.Second), "m-1"))
	if f, ok := nextFrame(t, ch, robotPosTopic(r.n), 200*time.Millisecond); ok {
		t.Fatalf("a message carrying none of N's orders made the frame %s", f.data)
	}
	if n := len(probe.lossy) + len(probe.events); n != 0 {
		t.Fatalf("%d frames reached M's topic with no panel of M's open: a frame was built for it", n)
	}

	// A status crossing into the tracked set, through the real lifecycle,
	// widens the watch and the frames.
	if err := r.eng.OrderManager().TransitionOrder(r.id["n-queued"], protocol.StatusDispatched, "test"); err != nil {
		t.Fatalf("dispatch n-queued: %v", err)
	}
	r.watching("the dispatched order joined N's watch", r.uuids("both", "n-1", "n-2", "n-queued"))
	r.eng.HandleRobotPositions(nil, r.position(at.Add(8*time.Second), "n-queued"))
	f, ok = nextFrame(t, ch, robotPosTopic(r.n), 2*time.Second)
	if !ok || json.Unmarshal([]byte(f.data), &got) != nil || len(got.Orders) != 1 || got.Orders[0].OrderID != r.id["n-queued"] {
		t.Fatalf("after its dispatch, n-queued's position made %+v (%v), want its frame joined to %d", f, ok, r.id["n-queued"])
	}

	// An order Core authored, whose row lands already driving (a projection
	// writes its status with no transition), joins the watch too.
	authored := "00000099-0000-4000-8000-000000000099"
	if _, err := r.eng.ApplyOrderProjection(protocol.OrderProjection{OrderUUID: authored,
		OrderType: protocol.OrderTypeRetrieve, Status: string(protocol.StatusInTransit), DeliveryNode: "RN02"}); err != nil {
		t.Fatalf("project an authored order: %v", err)
	}
	want4 := append(r.uuids("both", "n-1", "n-2", "n-queued"), authored)
	slices.Sort(want4)
	r.watching("the projected order joined N's watch", want4)
}

// TestRobotStream_AvailableOnConnectAndOnEveryChange: {available} is each
// panel's first frame and is sent again whenever it moves: false until Core
// names the ephemeral topic, true once the link opens on a current ack, false
// when the last ack goes stale by the screens' rule (FeedAsOfAfter), true on
// the next ack, false when Core stops naming the topic.
func TestRobotStream_AvailableOnConnectAndOnEveryChange(t *testing.T) {
	t.Parallel()
	// The stream's clock runs FeedAsOfAfter less 3 s ahead, so an ack goes
	// stale 3 s after it lands.
	ahead := engine.FeedAsOfAfter - 3*time.Second
	r := newRobotRig(t, func() time.Time { return time.Now().Add(ahead) }, true)

	ch, first, _ := r.panel(r.n)
	if first != `{"available":false}` {
		t.Fatalf("first frame before Core named the topic: %s, want {\"available\":false}", first)
	}
	next := func(what, want string) {
		t.Helper()
		f, ok := nextFrame(t, ch, robotPosTopic(r.n), 5*time.Second)
		if !ok || f.data != want {
			t.Fatalf("%s: frame %+v (%v), want %s", what, f, ok, want)
		}
	}
	r.ack("shingo.ephemeral")
	next("the link opened on a current ack", `{"available":true}`)
	next("the ack went stale", `{"available":false}`)
	r.ack("shingo.ephemeral")
	next("the next ack", `{"available":true}`)
	r.ack("")
	next("Core stopped naming the topic", `{"available":false}`)
	if _, second, _ := r.panel(r.n); second != `{"available":false}` {
		t.Errorf("a second panel's first frame %s, want {\"available\":false}", second)
	}
}
