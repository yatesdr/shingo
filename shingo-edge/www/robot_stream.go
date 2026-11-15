package www

import (
	"encoding/json"
	"fmt"
	"log"
	"maps"
	"runtime/debug"
	"slices"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingoedge/engine"
	"shingoedge/internal/lockorder"
)

// robot_stream.go — the robot panel's own stream (LEAD-NOTES D12, D13).
//
// The panel opens /events?topics=robot-pos.<operatorStationID> on its own
// EventSource while it is shown, and closes it when hidden. That stream is the
// only thing that sets and clears the Edge's watched-order set for the
// station (engine.SetRobotWatch, U8):
//
//   - the station's first client computes its set
//     (service.StationService.RobotWatchOrders: the board's orders with a Core
//     uuid and a status Core sends positions for) and sets it; further clients
//     of the station share it; the last one to close clears it, and with every
//     station cleared the Edge's watch sender sends nothing (Core's lease
//     lapses);
//   - while a station has a client, an order change recomputes its set: a
//     status crossing in or out of protocol.IsVendorTracked, or an order write
//     no transition announces (a projection, an order.update) about an order no
//     open station watches;
//   - each robot.positions message the engine hands over becomes one robot-pos
//     frame per open station holding any of its orders, with this Edge's order
//     id joined on; a station with no client is never looked at, so no frame is
//     built for it;
//   - {available, interval_ms} is each client's first frame, and is sent
//     again whenever available changes (interval_ms is Core's robot poll,
//     protocol.RobotPollInterval, so a panel has it before any position):
//     the ephemeral capability held (engine.RobotPositionsAvailable)
//     and the Core link up. The Core link is the one Diagnostics shows: the
//     last heartbeat ack (engine.LastCoreAck), current by the rule every screen
//     applies to what it holds from Core (engine.CurrentAt: no older than
//     engine.FeedAsOfAfter). The ack observer says when an ack arrives; a timer
//     says when the last one goes stale.
//
// NOTHING IS STORED. The sets and the joins are memory; the only reads are a
// recompute's (StationService.RobotWatchOrders), one per open station per
// burst of order events.
//
// HIDDEN IS ZERO WORK. With no client there is no set, no recompute, no frame
// and no timer, and loop waits on its doorbell; an ack or an order event costs
// one atomic load.
//
// ONE GOROUTINE (loop) makes every watch call, every recompute and every
// {available} change, so SetRobotWatch is never called out of order. Callers
// change state under mu and ring the doorbell; mu is never held across an
// engine call, a read or a broadcast.

// robotFeed is the engine's half of the live robot feed, as the stream
// drives it (shingoedge/engine robot_watch.go and feeds.go).
type robotFeed interface {
	SetRobotWatch(operatorStationID int64, orderUUIDs []string)
	SetRobotPositionsSubscriber(onPositions func(protocol.RobotPositions), onAvailable func(bool))
	RobotPositionsAvailable() bool
	LastCoreAck() (local, server time.Time)
	SetCoreAckObserver(fn func())
}

// robotPosFrame is one robot-pos frame: one robot.positions message cut to one
// operator station's orders (D12).
type robotPosFrame struct {
	T          time.Time       `json:"t"`
	IntervalMS int64           `json:"interval_ms"`
	Orders     []robotPosOrder `json:"orders"`
}

// robotPosOrder is one order's position with this Edge's order id, the key
// the panel joins its own order cards on. The robot is the position's: the
// live one (D14), never the stored order's.
type robotPosOrder struct {
	OrderID int64 `json:"order_id"`
	protocol.RobotPosition
}

// robotPosAvailable is the {available} frame's body: whether positions can
// come, and interval_ms, the interval Core polls its robots at
// (protocol.RobotPollInterval, the interval_ms every position frame carries).
// A panel then has an interval to count a missing first position against
// before any position frame has come (LEAD-NOTES D17).
func robotPosAvailable(available bool) json.RawMessage {
	return fmt.Appendf(nil, `{"available":%t,"interval_ms":%d}`, available, protocol.RobotPollInterval.Milliseconds())
}

// robotStation is one operator station's panels.
type robotStation struct {
	// clients is the open robot-pos streams naming the station.
	clients int
	// stale asks loop to recompute the set.
	stale bool
	// watched is whether the engine holds a set for the station.
	watched bool
	// orders is the set: watched Core uuid → this Edge's order id.
	orders map[string]int64
}

// robotStream is the robot panel's stream: see the file header.
type robotStream struct {
	feed robotFeed
	// watchedOrders computes one station's set
	// (StationService.RobotWatchOrders).
	watchedOrders func(stationID int64) (map[string]int64, error)
	broadcast     func(SSEEvent)
	// window is how long loop waits out a burst of doorbells before it acts.
	window time.Duration
	// now is the clock the Core link is judged on: time.Now, or a test's.
	now  func() time.Time
	stop <-chan struct{}

	mu       lockcheck.Mutex[lockorder.RobotStream]
	stations map[int64]*robotStation
	// available is the {available} last said, while any client is open.
	available bool

	// open counts robot-pos clients across stations; moved says an order
	// changed since the last recompute; shown is every uuid an open station
	// watches. All three are read by callbacks on other goroutines, lock free.
	open  atomic.Int64
	moved atomic.Bool
	shown atomic.Pointer[map[string]struct{}]

	bell chan struct{}
}

// newRobotStream builds the stream; start wires it and runs it.
func newRobotStream(feed robotFeed, watchedOrders func(int64) (map[string]int64, error),
	broadcast func(SSEEvent), window time.Duration, stop <-chan struct{}) *robotStream {
	return &robotStream{
		feed: feed, watchedOrders: watchedOrders, broadcast: broadcast,
		window: window, now: time.Now, stop: stop,
		stations: map[int64]*robotStation{},
		bell:     make(chan struct{}, 1),
	}
}

// start installs the stream's callbacks on the engine and runs loop. Call
// once, before serving. events nil leaves order changes unheard.
func (rs *robotStream) start(events *engine.EventBus) {
	rs.feed.SetRobotPositionsSubscriber(rs.positions, func(bool) { rs.ringIfOpen() })
	rs.feed.SetCoreAckObserver(rs.ringIfOpen)
	if events != nil {
		events.SubscribeTypes(rs.orderEvent, engine.EventOrderStatusChanged, engine.EventBoardNudge)
	}
	go rs.loop()
}

// ring wakes loop without blocking.
func (rs *robotStream) ring() {
	select {
	case rs.bell <- struct{}{}:
	default:
	}
}

// ringIfOpen rings while any client is open: the engine's callbacks, which
// must not block and are worth nothing with no panel shown.
func (rs *robotStream) ringIfOpen() {
	if rs.open.Load() > 0 {
		rs.ring()
	}
}

// orderEvent is the engine's order events, on the emitter's goroutine: it
// takes no lock. A status crossing in or out of the tracked set, or an order
// write no transition announced about an order no open station watches (a
// projection may write a status with no transition), asks for a recompute.
func (rs *robotStream) orderEvent(evt engine.Event) {
	if rs.open.Load() == 0 {
		return
	}
	switch p := evt.Payload.(type) {
	case engine.OrderStatusChangedEvent:
		if protocol.IsVendorTracked(protocol.Status(p.OldStatus)) == protocol.IsVendorTracked(protocol.Status(p.NewStatus)) {
			return
		}
	case engine.BoardNudgeEvent:
		if shown := rs.shown.Load(); shown != nil {
			if _, ok := (*shown)[p.OrderUUID]; ok {
				return
			}
		}
	default:
		return
	}
	rs.moved.Store(true)
	rs.ring()
}

// opened is a robot-pos client of stationID registering (EventHub.HandleSSE).
// It returns the client's first {available}.
func (rs *robotStream) opened(stationID int64) bool {
	available, _ := rs.availableAt(rs.now())
	rs.mu.Lock()
	st := rs.stations[stationID]
	if st == nil {
		st = &robotStation{}
		rs.stations[stationID] = st
	}
	st.clients++
	first := st.clients == 1
	if first {
		st.stale = true
	}
	if rs.open.Add(1) == 1 {
		rs.available = available
	}
	rs.mu.Unlock()
	if first {
		rs.ring()
	}
	return available
}

// closed is a robot-pos client of stationID going (EventHub.HandleSSE).
func (rs *robotStream) closed(stationID int64) {
	rs.mu.Lock()
	st := rs.stations[stationID]
	if st == nil || st.clients == 0 {
		rs.mu.Unlock()
		return
	}
	st.clients--
	rs.open.Add(-1)
	last := st.clients == 0
	rs.mu.Unlock()
	if last {
		rs.ring()
	}
}

// positions is one robot.positions message, cut to the watched orders, on
// the engine's read goroutine. One frame per open station holding any of its
// orders, encoded once; a station with no client is not looked at.
func (rs *robotStream) positions(p protocol.RobotPositions) {
	type cut struct {
		station int64
		orders  []robotPosOrder
	}
	var cuts []cut
	rs.mu.Lock()
	for id, st := range rs.stations {
		if st.clients == 0 {
			continue
		}
		var mine []robotPosOrder
		for _, pos := range p.Orders {
			if orderID, ok := st.orders[pos.OrderUUID]; ok {
				mine = append(mine, robotPosOrder{OrderID: orderID, RobotPosition: pos})
			}
		}
		if len(mine) > 0 {
			cuts = append(cuts, cut{id, mine})
		}
	}
	rs.mu.Unlock()
	for _, c := range cuts {
		data, err := json.Marshal(robotPosFrame{T: p.T, IntervalMS: p.IntervalMS, Orders: c.orders})
		if err != nil {
			continue
		}
		rs.broadcast(SSEEvent{Type: robotPosTopic(c.station), Data: json.RawMessage(data)})
	}
}

// loop is the stream's one goroutine: on a doorbell (after its burst) or the
// Core link's stale moment it recomputes what is stale and says {available}
// when it moved. With no client it holds no timer.
func (rs *robotStream) loop() {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("PANIC robot-stream: %v\n%s", r, debug.Stack())
		}
	}()
	var stale *time.Timer
	defer func() {
		if stale != nil {
			stale.Stop()
		}
	}()
	for {
		var fire <-chan time.Time
		if stale != nil {
			fire = stale.C
		}
		select {
		case <-rs.stop:
			return
		case <-rs.bell:
			if !rs.waitOutBurst() {
				return
			}
		case <-fire:
		}
		rs.recompute()
		if stale != nil {
			stale.Stop()
			stale = nil
		}
		if due := rs.sayAvailable(); due > 0 {
			stale = time.NewTimer(due)
		}
	}
}

// waitOutBurst absorbs doorbells for the window. False when stopped.
func (rs *robotStream) waitOutBurst() bool {
	if rs.window <= 0 {
		return true
	}
	timer := time.NewTimer(rs.window)
	defer timer.Stop()
	for {
		select {
		case <-rs.stop:
			return false
		case <-rs.bell:
		case <-timer.C:
			return true
		}
	}
}

// recompute clears every station whose last client went and computes every
// open station's set that is stale (all of them after an order change).
func (rs *robotStream) recompute() {
	moved := rs.moved.Swap(false)
	var clear, compute []int64
	rs.mu.Lock()
	for id, st := range rs.stations {
		switch {
		case st.clients == 0:
			clear = append(clear, id)
		case st.stale || moved:
			st.stale = false
			compute = append(compute, id)
		}
	}
	rs.mu.Unlock()
	for _, id := range clear {
		rs.clearStation(id)
	}
	for _, id := range compute {
		rs.computeStation(id)
	}
	if len(clear)+len(compute) > 0 {
		rs.publishShown()
	}
}

// clearStation drops a station whose last client went, and its set. A client
// that came back meanwhile keeps it (opened marked it stale and rang).
func (rs *robotStream) clearStation(id int64) {
	rs.mu.Lock()
	st := rs.stations[id]
	if st == nil || st.clients > 0 {
		rs.mu.Unlock()
		return
	}
	watched := st.watched
	delete(rs.stations, id)
	rs.mu.Unlock()
	if watched {
		rs.feed.SetRobotWatch(id, nil)
	}
}

// computeStation reads one station's set and hands it to the engine. A
// station whose last client went meanwhile is left for the next pass to clear
// (closed rang). A read that fails watches nothing for the station.
func (rs *robotStream) computeStation(id int64) {
	set, err := rs.watchedOrders(id)
	if err != nil {
		log.Printf("robot stream: operator station %d's watched orders: %v; it watches none", id, err)
		set = map[string]int64{}
	}
	rs.mu.Lock()
	st := rs.stations[id]
	if st == nil || st.clients == 0 {
		rs.mu.Unlock()
		return
	}
	st.orders, st.watched = set, true
	rs.mu.Unlock()
	rs.feed.SetRobotWatch(id, slices.Sorted(maps.Keys(set)))
}

// publishShown replaces the lock-free copy of every uuid an open station
// watches (orderEvent reads it).
func (rs *robotStream) publishShown() {
	shown := map[string]struct{}{}
	rs.mu.Lock()
	for _, st := range rs.stations {
		if st.clients > 0 {
			for u := range st.orders {
				shown[u] = struct{}{}
			}
		}
	}
	rs.mu.Unlock()
	rs.shown.Store(&shown)
}

// availableAt is {available} at now, and the ack it was judged on.
func (rs *robotStream) availableAt(now time.Time) (bool, time.Time) {
	local, _ := rs.feed.LastCoreAck()
	at, old := engine.CurrentAt(local, time.Time{}, now)
	return rs.feed.RobotPositionsAvailable() && !old, at
}

// sayAvailable sends {available} to every open station's clients when it
// moved, and returns how long until the last ack goes stale while it is true
// (0: no timer).
func (rs *robotStream) sayAvailable() time.Duration {
	now := rs.now()
	available, at := rs.availableAt(now)
	var open []int64
	rs.mu.Lock()
	for id, st := range rs.stations {
		if st.clients > 0 {
			open = append(open, id)
		}
	}
	moved := len(open) > 0 && available != rs.available
	if len(open) > 0 {
		rs.available = available
	}
	rs.mu.Unlock()
	if moved {
		for _, id := range open {
			rs.broadcast(SSEEvent{Type: robotPosTopic(id), Data: robotPosAvailable(available)})
		}
	}
	if len(open) == 0 || !available {
		return 0
	}
	return at.Add(engine.FeedAsOfAfter).Sub(now) + time.Millisecond
}
