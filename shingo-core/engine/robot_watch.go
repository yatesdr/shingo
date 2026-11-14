package engine

import (
	"slices"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingocore/fleet"
	"shingocore/internal/lockorder"
	"shingocore/store/orders"
)

// robot_watch.go — Core's half of the live robot feed (protocol/ephemeral.go).
//
// An Edge that is showing robots sends robot.watch, naming the Core orders it
// is showing, every protocol.RobotWatchInterval. Core keeps each station's
// latest watch for protocol.RobotWatchLease (robotWatches) and, on every robot
// poll while at least one is live, sends each watching station one
// robot.positions built from that poll (publishRobotPositions).
//
// NO LEASE, NO WORK. With no live watch the poll reads no database row, builds
// no message and publishes nothing: one map length under a leaf lock is the
// whole cost. Pinned by TestRobotPositions_NoLeaseNoWork (docker, statements
// counted at pgx's tracer).

// robotWatches is every live watch, by the Edge station that sent it.
type robotWatches struct {
	mu     lockcheck.Mutex[lockorder.RobotWatches]
	leases map[string]robotLease
}

// robotLease is one station's latest watch and when it lapses.
type robotLease struct {
	uuids   []string
	expires time.Time
}

// stationWatch is one live lease as the poll reads it.
type stationWatch struct {
	station string
	uuids   []string
}

// renew replaces station's watch and runs its lease from now.
func (w *robotWatches) renew(station string, uuids []string, now time.Time) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.leases == nil {
		w.leases = map[string]robotLease{}
	}
	w.leases[station] = robotLease{uuids: uuids, expires: now.Add(protocol.RobotWatchLease)}
}

// live is every lease live at now, by station, and forgets the lapsed ones.
// nil when there is none.
func (w *robotWatches) live(now time.Time) []stationWatch {
	w.mu.Lock()
	defer w.mu.Unlock()
	var out []stationWatch
	for station, l := range w.leases {
		if !now.Before(l.expires) {
			delete(w.leases, station)
			continue
		}
		out = append(out, stationWatch{station: station, uuids: l.uuids})
	}
	slices.SortFunc(out, func(a, b stationWatch) int {
		switch {
		case a.station < b.station:
			return -1
		case a.station > b.station:
			return 1
		}
		return 0
	})
	return out
}

// HandleRobotWatch takes one Edge's robot.watch, read off the ephemeral topic.
// Memory only: it reads no row and sends nothing. The station is the
// envelope's, as every Edge-sent count's is (messaging.HandleProductionTicks);
// a watch whose body names another station is dropped. Whether each order is
// that station's is checked where the orders are read, on the poll
// (buildRobotPositions), so a watch costs nothing but the map write.
func (e *Engine) HandleRobotWatch(env *protocol.Envelope, w *protocol.RobotWatch) {
	station := env.Src.Station
	if station == "" || w.StationID != station {
		e.dbg("engine: robot watch from %q naming station %q: dropped", station, w.StationID)
		return
	}
	uuids := slices.Clone(w.OrderUUIDs)
	slices.Sort(uuids)
	e.robotWatches.renew(station, slices.Compact(uuids), time.Now())
}

// SetRobotPositionsPublisher wires the direct publish to the ephemeral topic
// (messaging.Client.PublishEphemeral). Unset, the poll does no feed work at
// all: Core has no verified topic to send on, and no Edge can have watched.
func (e *Engine) SetRobotPositionsPublisher(fn func(*protocol.Envelope) error) {
	e.robotPositionsPub.Store(&fn)
}

// publishRobotPositions is the poll's feed step: one robot.positions per
// station with a live watch, built from this poll. at is the poll's sample
// time. One statement reads every watched order's station and robot
// (store.OrderRobotsByUUID); nothing at all runs with no live watch.
func (e *Engine) publishRobotPositions(robots []fleet.RobotStatus, at time.Time) {
	pub := e.robotPositionsPub.Load()
	if pub == nil || *pub == nil {
		return
	}
	watches := e.robotWatches.live(at)
	if len(watches) == 0 {
		return
	}
	var uuids []string
	for _, w := range watches {
		uuids = append(uuids, w.uuids...)
	}
	slices.Sort(uuids)
	rows, err := e.db.OrderRobotsByUUID(slices.Compact(uuids))
	if err != nil {
		e.dbg("engine: robot positions: read the watched orders: %v", err)
		return
	}
	for _, sp := range buildRobotPositions(watches, rows, robots, at) {
		env, err := protocol.NewEnvelope(protocol.TypeRobotPositions,
			protocol.Address{Role: protocol.RoleCore, Station: e.cfg.Messaging.StationID},
			protocol.Address{Role: protocol.RoleEdge, Station: sp.station}, &sp.body)
		if err != nil {
			e.dbg("engine: robot positions for %s: build: %v", sp.station, err)
			continue
		}
		if err := (*pub)(env); err != nil {
			e.dbg("engine: robot positions for %s: send: %v", sp.station, err)
		}
	}
}

// stationPositions is one station's robot.positions body.
type stationPositions struct {
	station string
	body    protocol.RobotPositions
}

// buildRobotPositions is each watching station's message for one poll: every
// watched uuid whose order is that station's and on a robot (rows), and whose
// robot this poll listed connected, in the watch's order. An order the station
// does not own, one with no robot, and one whose robot is absent or
// disconnected are left out; a station with none of them still gets its
// message, with orders [].
func buildRobotPositions(watches []stationWatch, rows []orders.OrderRobot, robots []fleet.RobotStatus, at time.Time) []stationPositions {
	order := make(map[string]orders.OrderRobot, len(rows))
	for _, r := range rows {
		order[r.UUID] = r
	}
	polled := make(map[string]*fleet.RobotStatus, len(robots))
	for i := range robots {
		if r := &robots[i]; r.VehicleID != "" && r.Connected {
			polled[r.VehicleID] = r
		}
	}
	t := at.UTC().Truncate(time.Millisecond)
	out := make([]stationPositions, 0, len(watches))
	for _, w := range watches {
		sp := stationPositions{station: w.station, body: protocol.RobotPositions{
			T: t, IntervalMS: robotPollInterval.Milliseconds(), Orders: []protocol.RobotPosition{},
		}}
		for _, u := range w.uuids {
			o, ok := order[u]
			if !ok || o.StationID != w.station {
				continue
			}
			r, ok := polled[o.RobotID]
			if !ok {
				continue
			}
			sp.body.Orders = append(sp.body.Orders, protocol.RobotPosition{
				OrderUUID: u, RobotID: o.RobotID, X: r.X, Y: r.Y, Heading: r.Angle, Path: r.Path,
			})
		}
		out = append(out, sp)
	}
	return out
}
