package engine

import (
	"slices"
	_ "sync" // the plain lock inlines only where sync is imported (protocol/lockcheck)
	"time"

	"shingo/protocol"
	"shingo/protocol/lockcheck"
	"shingoedge/internal/lockorder"
)

// robot_watch.go — the Edge's half of the live robot feed (protocol/ephemeral.go).
//
// THE API U9's STREAM DRIVES. SetRobotWatch says which Core orders one
// operator station is showing a robot for; the watch sent to Core is the union
// across this Edge's operator stations. While that union is not empty and Core
// has named the ephemeral topic, the Edge sends robot.watch at once on a change
// and every protocol.RobotWatchInterval; on an empty union it sends nothing and
// Core's lease lapses (protocol.RobotWatchLease). The positions Core sends back
// are held in memory, the latest per order uuid, and handed to the subscriber
// (SetRobotPositionsSubscriber). Nothing here is written to SQLite.
//
// THE TOPIC IS TOUCHED ONLY WHILE CORE NAMES IT. Every registration and every
// heartbeat ack states Core's verified topic afresh (OfferEphemeralTopic): a
// name opens the link (reader at the last offset, writer), its absence closes
// it, and with no name there is no reader, no writer and no log line. The Edge
// also opens it only when the name is the one its own config holds
// (messaging.ephemeral_topic), so an Edge configured off, or for another topic,
// never touches what Core named.
//
// ONE GOROUTINE OWNS THE LINK (robotWatchLoop). Callers change the state under
// the leaf lock and poke the loop; the loop opens, closes and publishes outside
// any lock, so the messaging client's lock is never taken under this one.

// EphemeralLink is the ephemeral topic as the engine drives it. The
// composition root's adapter over the messaging client implements it; Start's
// reader hands what it reads to HandleRobotPositions.
type EphemeralLink interface {
	// Start opens the topic: its writer, and its reader at the last offset.
	Start(topic string) error
	// Stop closes both. Idempotent.
	Stop()
	// Publish sends one envelope: direct, one attempt, never the outbox.
	Publish(env *protocol.Envelope) error
}

// RobotPositionHeld is the latest position held for one watched order.
type RobotPositionHeld struct {
	// T and IntervalMS are the robot.positions message's (Core's poll time and
	// interval).
	T          time.Time
	IntervalMS int64
	Position   protocol.RobotPosition
}

// robotWatch is the engine's state for the feed. Guarded by mu except where a
// field says otherwise.
type robotWatch struct {
	mu lockcheck.Mutex[lockorder.RobotWatch]
	// sets is what each operator station is showing (SetRobotWatch).
	sets map[int64][]string
	// offered is the topic the last registration or ack named; "" for none.
	offered string
	// active is the topic the loop has the link open on; "" while closed.
	// Written by the loop only.
	active string
	// held is the latest position per watched order uuid.
	held map[string]RobotPositionHeld
	// onPositions and onAvailable are the subscriber's (U9's) callbacks.
	onPositions func(protocol.RobotPositions)
	onAvailable func(bool)
	// link is the composition root's adapter; nil keeps the feed off.
	link EphemeralLink
	// mismatchLogged is the (offered, configured) pair last logged as not
	// matching, so a mismatch is said once and not on every ack.
	mismatchLogged [2]string

	// poke wakes the loop; buffered 1, never blocks a caller. Set once,
	// before the loop starts.
	poke chan struct{}
}

// startRobotWatchLoop starts robotWatchLoop, resending every `every`
// (protocol.RobotWatchInterval; a test passes a shorter one).
func (e *Engine) startRobotWatchLoop(every time.Duration) {
	e.robotWatch.poke = make(chan struct{}, 1)
	go e.robotWatchLoop(every)
}

// pokeRobotWatch wakes the loop without blocking.
func (e *Engine) pokeRobotWatch() {
	select {
	case e.robotWatch.poke <- struct{}{}:
	default:
	}
}

// robotWatchLoop opens and closes the ephemeral link as Core names the topic,
// and sends robot.watch at once when the shown orders change and again each
// interval (every) while any are shown, until the engine stops. Idle (no link,
// nothing shown, or no topic) it waits on a poke and holds no timer.
func (e *Engine) robotWatchLoop(every time.Duration) {
	rw := &e.robotWatch
	var (
		sent   []string // the union last sent; nil when none is
		timer  *time.Timer
		resend <-chan time.Time
	)
	stopTimer := func() {
		if timer != nil {
			timer.Stop()
		}
		timer, resend = nil, nil
	}
	defer stopTimer()
	for {
		select {
		case <-e.stopChan:
			e.closeRobotLink()
			return
		case <-rw.poke:
		case <-resend:
			sent = nil
		}
		topic, union, link := e.robotWatchWant()
		if link == nil {
			continue
		}
		if e.syncRobotLink(link, topic) {
			sent = nil
		}
		if e.robotWatchActive() == "" || len(union) == 0 {
			stopTimer()
			sent = nil
			continue
		}
		if slices.Equal(union, sent) {
			continue
		}
		e.sendRobotWatch(link, union)
		sent = union
		stopTimer()
		timer = time.NewTimer(every)
		resend = timer.C
	}
}

// robotWatchWant is what the loop should hold: the topic to have open (the
// one Core named, when this Edge's config names the same one), the union to
// watch, and the link.
func (e *Engine) robotWatchWant() (topic string, union []string, link EphemeralLink) {
	rw := &e.robotWatch
	configured := e.cfg.MessagingSettings().EphemeralTopic
	rw.mu.Lock()
	offered := rw.offered
	mismatch := offered != "" && configured != "" && offered != configured
	tell := mismatch && rw.mismatchLogged != [2]string{offered, configured}
	if tell {
		rw.mismatchLogged = [2]string{offered, configured}
	}
	union, link = rw.unionLocked(), rw.link
	rw.mu.Unlock()
	if tell {
		e.logFn("robot feed: Core names the ephemeral topic %q and this Edge is configured for %q "+
			"(messaging.ephemeral_topic): the live robot feed is off", offered, configured)
	}
	if offered != "" && offered == configured {
		topic = offered
	}
	return topic, union, link
}

// syncRobotLink opens or closes the link to match topic; true when it moved.
func (e *Engine) syncRobotLink(link EphemeralLink, topic string) bool {
	active := e.robotWatchActive()
	if topic == active {
		return false
	}
	if active != "" {
		e.closeRobotLink()
	}
	if topic == "" {
		return true
	}
	if err := link.Start(topic); err != nil {
		e.logFn("robot feed: open the ephemeral topic %s: %v", topic, err)
		return true
	}
	e.setRobotLinkActive(topic)
	return true
}

// closeRobotLink closes an open link, drops what it held and tells the
// subscriber the feed is gone.
func (e *Engine) closeRobotLink() {
	rw := &e.robotWatch
	rw.mu.Lock()
	link, was := rw.link, rw.active
	rw.active = ""
	rw.held = nil
	onAvailable := rw.onAvailable
	rw.mu.Unlock()
	if was == "" {
		return
	}
	link.Stop()
	if onAvailable != nil {
		onAvailable(false)
	}
}

func (e *Engine) setRobotLinkActive(topic string) {
	rw := &e.robotWatch
	rw.mu.Lock()
	rw.active = topic
	onAvailable := rw.onAvailable
	rw.mu.Unlock()
	if onAvailable != nil {
		onAvailable(true)
	}
}

func (e *Engine) robotWatchActive() string {
	rw := &e.robotWatch
	rw.mu.Lock()
	defer rw.mu.Unlock()
	return rw.active
}

// sendRobotWatch publishes one robot.watch for union. A failure is dropped:
// the next resend carries the same union, and Core's lease outlasts two lost
// watches.
func (e *Engine) sendRobotWatch(link EphemeralLink, union []string) {
	station := e.cfg.StationID()
	env, err := protocol.NewEnvelope(protocol.TypeRobotWatch,
		protocol.Address{Role: protocol.RoleEdge, Station: station},
		protocol.Address{Role: protocol.RoleCore},
		&protocol.RobotWatch{OrderUUIDs: union})
	if err != nil {
		e.debugFn("robot feed: build watch: %v", err)
		return
	}
	if err := link.Publish(env); err != nil {
		e.debugFn("robot feed: send watch: %v", err)
	}
}

// unionLocked is every operator station's shown orders together, sorted,
// each once; nil when nothing is shown.
func (rw *robotWatch) unionLocked() []string {
	var out []string
	for _, uuids := range rw.sets {
		out = append(out, uuids...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// SetEphemeralLink wires the composition root's adapter over the messaging
// client. Set once, before Kafka delivers: the first registration's topic opens
// it. nil (tests, an Edge without Kafka) keeps the feed off.
func (e *Engine) SetEphemeralLink(link EphemeralLink) {
	e.robotWatch.mu.Lock()
	e.robotWatch.link = link
	e.robotWatch.mu.Unlock()
	e.pokeRobotWatch()
}

// OfferEphemeralTopic takes the ephemeral topic a registration or heartbeat ack
// named ("" when it named none). Each states Core's topic afresh, so "" from a
// later one (an older Core, or one that could not verify it) closes the link.
func (e *Engine) OfferEphemeralTopic(topic string) {
	rw := &e.robotWatch
	rw.mu.Lock()
	changed := rw.offered != topic
	rw.offered = topic
	rw.mu.Unlock()
	if changed {
		e.pokeRobotWatch()
	}
}

// SetRobotWatch says which Core order uuids one operator station is showing a
// robot for; an empty list clears it. The watch Core receives is the union
// across this Edge's operator stations, sent at once when the union changes.
// Positions held for an order no longer shown are dropped.
func (e *Engine) SetRobotWatch(operatorStationID int64, orderUUIDs []string) {
	shown := make([]string, 0, len(orderUUIDs))
	for _, u := range orderUUIDs {
		if u != "" {
			shown = append(shown, u)
		}
	}
	slices.Sort(shown)
	shown = slices.Compact(shown)

	rw := &e.robotWatch
	rw.mu.Lock()
	if len(shown) == 0 {
		delete(rw.sets, operatorStationID)
	} else {
		if rw.sets == nil {
			rw.sets = map[int64][]string{}
		}
		rw.sets[operatorStationID] = shown
	}
	union := rw.unionLocked()
	for u := range rw.held {
		if _, ok := slices.BinarySearch(union, u); !ok {
			delete(rw.held, u)
		}
	}
	rw.mu.Unlock()
	e.pokeRobotWatch()
}

// SetRobotPositionsSubscriber installs the subscriber: onPositions gets every
// robot.positions message read for this station, cut to the watched orders,
// and onAvailable whether the link is open, each time that changes. Either may
// be nil. Called on the ephemeral read goroutine and the loop's goroutine:
// they must not block.
func (e *Engine) SetRobotPositionsSubscriber(onPositions func(protocol.RobotPositions), onAvailable func(bool)) {
	rw := &e.robotWatch
	rw.mu.Lock()
	rw.onPositions, rw.onAvailable = onPositions, onAvailable
	rw.mu.Unlock()
}

// RobotPositionsAvailable reports whether the link is open: Core names the
// topic, this Edge's config agrees, and the link opened.
func (e *Engine) RobotPositionsAvailable() bool { return e.robotWatchActive() != "" }

// RobotPosition is the latest position held for one watched order.
func (e *Engine) RobotPosition(orderUUID string) (RobotPositionHeld, bool) {
	rw := &e.robotWatch
	rw.mu.Lock()
	defer rw.mu.Unlock()
	h, ok := rw.held[orderUUID]
	return h, ok
}

// HandleRobotPositions takes one robot.positions read off the ephemeral topic
// for this station. Memory only. The orders kept are the ones shown now; for
// each, the position replaces the one held and is handed on, unless it is
// older (a later poll already arrived): an older one is neither, so a panel
// never sees a robot step back. With nothing shown the message is dropped
// whole.
func (e *Engine) HandleRobotPositions(_ *protocol.Envelope, p *protocol.RobotPositions) {
	rw := &e.robotWatch
	rw.mu.Lock()
	union := rw.unionLocked()
	if len(union) == 0 {
		rw.mu.Unlock()
		return
	}
	out := protocol.RobotPositions{T: p.T, IntervalMS: p.IntervalMS, Orders: make([]protocol.RobotPosition, 0, len(p.Orders))}
	for _, pos := range p.Orders {
		if _, ok := slices.BinarySearch(union, pos.OrderUUID); !ok {
			continue
		}
		if h, ok := rw.held[pos.OrderUUID]; ok && p.T.Before(h.T) {
			continue
		}
		out.Orders = append(out.Orders, pos)
		if rw.held == nil {
			rw.held = map[string]RobotPositionHeld{}
		}
		rw.held[pos.OrderUUID] = RobotPositionHeld{T: p.T, IntervalMS: p.IntervalMS, Position: pos}
	}
	onPositions := rw.onPositions
	rw.mu.Unlock()
	if onPositions != nil {
		onPositions(out)
	}
}
