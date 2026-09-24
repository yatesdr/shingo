package messaging

import (
	"fmt"
	"log"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingo/protocol/clock"
	"shingoedge/store/counters"
)

// TickStore is what the production tick shipper reads and writes. *store.DB
// satisfies it.
type TickStore interface {
	ListShippableTicks(afterID int64, limit int) ([]counters.ShippableTick, error)
	ProductionTickLag(afterID int64) (pending, oldestMS int64, err error)
	ProductionTickCursor() (id int64, ok bool, err error)
	SetProductionTickCursor(id int64) error
	InitialProductionTickCursor() (int64, error)
}

// PublishFunc sends one encoded envelope to Core. Production passes the Kafka
// client's synchronous Publish on the orders topic, which signs when a key is
// configured and blocks up to 10 s.
type PublishFunc func(data []byte) error

const (
	// tickShipPage bounds one read and one message. A caught-up pass carries a
	// handful of ticks; a backlog after an outage goes out in pages of this.
	// 500 ticks is ~55 KB of body, well under the broker's 1 MB default.
	tickShipPage = 500
	// tickCursorPersistEvery is the most often the cursor is written. After a
	// crash up to this much is re-sent, and Core's key makes it a no-op.
	tickCursorPersistEvery = time.Minute
	// tickRetryMin / tickRetryMax bound the wait after a failed publish. Rings
	// that arrive while it runs are held, not acted on, so a dead broker costs
	// one attempt per interval rather than one per poll pass.
	tickRetryMin = time.Second
	tickRetryMax = 30 * time.Second
)

// TickShipper publishes the production tick feed from counter_snapshots.
//
// The poll pass writes each tick's row and rings Notify. The shipper reads the
// shippable rows past its cursor (a primary-key range read), publishes them as
// ONE production.ticks message, and moves the in-memory cursor only when the
// publish succeeded; after a failure it retries and then pages until caught up.
// The cursor goes to its one-row table at most once a minute, only when it
// moved, and at a clean Stop. There is no outbox row: the snapshot row is the
// durable record, so an outage, a reboot or a restore loses nothing that is
// still inside counter_snapshots' 14-day retention.
//
// Off the poll goroutine on purpose: Publish blocks for up to 10 s, and the
// poll must not.
type TickShipper struct {
	store   TickStore
	publish PublishFunc
	station string

	// cursor is the last id published; persisted is the last id written to the
	// cursor table. cursor is read by Lag from other goroutines.
	cursor    atomic.Int64
	persisted int64
	// shipMu serialises ShipPending (the loop, and tests that drive it).
	shipMu sync.Mutex

	wake     chan struct{}
	stop     chan struct{}
	done     chan struct{}
	stopOnce sync.Once
	started  atomic.Bool

	DebugLog DebugLogFunc
}

// NewTickShipper loads the persisted cursor, or on the first boot after the
// upgrade starts after the last row written by the outbox-era binary (see
// counters.InitialShipCursor) and persists that at once, so a crash before the
// first minute cannot move the start.
func NewTickShipper(st TickStore, publish PublishFunc, station string) (*TickShipper, error) {
	s := &TickShipper{
		store: st, publish: publish, station: station,
		wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{}),
	}
	id, ok, err := st.ProductionTickCursor()
	if err != nil {
		return nil, fmt.Errorf("read production tick cursor: %w", err)
	}
	if !ok {
		if id, err = st.InitialProductionTickCursor(); err != nil {
			return nil, fmt.Errorf("initial production tick cursor: %w", err)
		}
		if err := st.SetProductionTickCursor(id); err != nil {
			return nil, fmt.Errorf("persist initial production tick cursor: %w", err)
		}
		log.Printf("tick shipper: first start, shipping counter_snapshots after id %d", id)
	}
	s.cursor.Store(id)
	s.persisted = id
	return s, nil
}

// Cursor is the id of the last row published.
func (s *TickShipper) Cursor() int64 { return s.cursor.Load() }

// Notify rings the shipper. Never blocks: a ring already pending covers this
// one, because a pass reads everything past the cursor.
func (s *TickShipper) Notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ShipPending publishes every shippable row past the cursor, one message per
// page, until a page comes back short. It stops at the first failure and
// returns it, with the cursor at the last page that was published.
func (s *TickShipper) ShipPending() error {
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	for {
		rows, err := s.store.ListShippableTicks(s.cursor.Load(), tickShipPage)
		if err != nil {
			return fmt.Errorf("read shippable ticks: %w", err)
		}
		if len(rows) == 0 {
			return nil
		}
		data, err := s.encode(rows)
		if err != nil {
			return err
		}
		if err := s.publish(data); err != nil {
			return fmt.Errorf("publish %d production tick(s): %w", len(rows), err)
		}
		s.cursor.Store(rows[len(rows)-1].ID)
		s.DebugLog.Log("production.ticks sent n=%d cursor=%d bytes=%d", len(rows), rows[len(rows)-1].ID, len(data))
		if len(rows) < tickShipPage {
			return nil
		}
	}
}

func (s *TickShipper) encode(rows []counters.ShippableTick) ([]byte, error) {
	body := protocol.ProductionTicks{Ticks: make([]protocol.ProductionTickEvent, len(rows))}
	for i, r := range rows {
		body.Ticks[i] = protocol.ProductionTickEvent{
			EdgeSnapshotID: r.ID,
			ProcessID:      r.ProcessID,
			StyleID:        r.StyleID,
			CountValue:     r.CountValue,
			Delta:          r.Delta,
			Anomaly:        r.Anomaly,
			RecordedAt:     time.UnixMilli(r.RecordedMS).UTC(),
		}
	}
	env, err := protocol.NewDataEnvelope(protocol.SubjectProductionTicks,
		protocol.Address{Role: protocol.RoleEdge, Station: s.station},
		protocol.Address{Role: protocol.RoleCore}, &body)
	if err != nil {
		return nil, fmt.Errorf("build production.ticks envelope: %w", err)
	}
	data, err := env.Encode()
	if err != nil {
		return nil, fmt.Errorf("encode production.ticks envelope: %w", err)
	}
	return data, nil
}

// persistIfMoved writes the cursor when it moved since the last write. Called
// by the loop once a minute and at Stop.
func (s *TickShipper) persistIfMoved() {
	s.shipMu.Lock()
	defer s.shipMu.Unlock()
	id := s.cursor.Load()
	if id == s.persisted {
		return
	}
	if err := s.store.SetProductionTickCursor(id); err != nil {
		log.Printf("tick shipper: persist cursor %d: %v", id, err)
		return
	}
	s.persisted = id
}

// Lag reports the shippable rows past the cursor and the age in ms of the
// oldest of them (0 when none): the /status and heartbeat view of whether the
// feed is keeping up. One statement.
func (s *TickShipper) Lag() (pending, oldestAgeMS int64, err error) {
	pending, oldestMS, err := s.store.ProductionTickLag(s.cursor.Load())
	if err != nil || pending == 0 {
		return pending, 0, err
	}
	age := clock.Now().UTC().UnixMilli() - oldestMS
	if age < 0 {
		age = 0
	}
	return pending, age, nil
}

// Start runs the loop. It ships whatever is already pending first, so rows
// written while the process was down go out without waiting for a tick.
func (s *TickShipper) Start() {
	s.started.Store(true)
	s.Notify()
	go s.loop()
}

// Stop ends the loop and persists a moved cursor. Safe to call more than once,
// and on a shipper that was never started.
func (s *TickShipper) Stop() {
	s.stopOnce.Do(func() { close(s.stop) })
	if s.started.Load() {
		<-s.done
	}
	s.persistIfMoved()
}

// loop is the shipper goroutine. Real-time cadence (stdlib timers), because
// its work is a real broker round trip; see clock.Default for the rule.
func (s *TickShipper) loop() {
	defer close(s.done)
	persist := time.NewTicker(tickCursorPersistEvery)
	defer persist.Stop()
	var retry *time.Timer
	var retryC <-chan time.Time
	backoff := tickRetryMin
	for {
		select {
		case <-s.stop:
			if retry != nil {
				retry.Stop()
			}
			return
		case <-persist.C:
			s.persistIfMoved()
			continue
		case <-s.wake:
			if retryC != nil {
				continue // a retry is scheduled; it reads everything this ring would
			}
		case <-retryC:
			retryC = nil
		}
		if err := s.shipSafe(); err != nil {
			log.Printf("tick shipper: %v (retrying in %s)", err, backoff)
			retry = time.NewTimer(backoff)
			retryC = retry.C
			backoff = min(backoff*2, tickRetryMax)
			continue
		}
		backoff = tickRetryMin
	}
}

// shipSafe is ShipPending with a recover, so one bad row cannot end the feed.
func (s *TickShipper) shipSafe() (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v\n%s", r, debug.Stack())
		}
	}()
	return s.ShipPending()
}
