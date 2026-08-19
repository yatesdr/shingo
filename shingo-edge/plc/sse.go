package plc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"time"
)

// sseReconcileInterval is how often SSE mode re-runs the level-triggered
// warlinkPollTick while a stream is up, so a stale per-PLC status cannot
// persist indefinitely. See the rationale at its use site in sseConnect.
//
// 60s is chosen against the failure it bounds, not against load: the cost is
// one REST call a minute, and the alternative observed in the field was three
// and a half days of a cell not counting.
//
// A var rather than a const so tests can shorten it; nothing in production
// writes it.
var sseReconcileInterval = 60 * time.Second

// sseStallTimeout is how long an SSE stream may deliver nothing (no events,
// no keepalives) before it is declared dead and reconnected. WarLink sends a
// keepalive comment every 30s, so a healthy stream resets this constantly; a
// genuine 120s of silence means the TCP path is dead — typically a silent
// WiFi drop on the edge Pi — and blocking on it forever is the alternative.
//
// A var for the same reason as sseReconcileInterval.
var sseStallTimeout = 120 * time.Second

// --- SSE event payload types (from WarLink) ---

type sseValueChange struct {
	PLC   string `json:"plc"`
	Tag   string `json:"tag"`
	Value any    `json:"value"`
	Type  string `json:"type"`
}

type sseStatusChange struct {
	PLC            string `json:"plc"`
	Status         string `json:"status"`
	TagCount       int    `json:"tagCount"`
	Error          string `json:"error"`
	ProductName    string `json:"productName"`
	SerialNumber   string `json:"serialNumber"`
	Vendor         string `json:"vendor"`
	ConnectionMode string `json:"connectionMode"`
}

type sseHealthUpdate struct {
	PLC       string `json:"plc"`
	Driver    string `json:"driver"`
	Online    bool   `json:"online"`
	Status    string `json:"status"`
	Error     string `json:"error"`
	Timestamp string `json:"timestamp"`
}

// sseLoop is the top-level goroutine for SSE mode. It reconnects on disconnect
// with capped exponential backoff.
func (m *Manager) sseLoop() {
	defer m.warlinkWg.Done()

	attempt := 0
	for {
		// Check for stop before connecting
		select {
		case <-m.stopChan:
			return
		case <-m.warlinkStopChan:
			return
		default:
		}

		err := m.sseConnect()
		if err == nil {
			// Clean shutdown via context cancellation
			return
		}

		// Connection lost: mark disconnected
		m.mu.Lock()
		wasConnected := m.warlinkConnected
		if wasConnected {
			m.warlinkConnected = false
			m.warlinkError = err
		}
		var disconnected []string
		if wasConnected {
			for _, mp := range m.plcs {
				mp.mu.Lock()
				if mp.Status == "Connected" {
					mp.Status = "Disconnected"
					disconnected = append(disconnected, mp.Name)
				}
				mp.mu.Unlock()
			}
		}
		m.mu.Unlock()
		if wasConnected {
			log.Printf("WarLink SSE disconnected: %v", err)
			m.emitter.EmitWarLinkDisconnected(err)
			for _, name := range disconnected {
				m.emitter.EmitPLCDisconnected(name, err)
			}
		}

		attempt++
		if !m.sseBackoff(attempt) {
			return // stop requested
		}
	}
}

// sseConnect opens a single SSE connection and processes events until
// the stream ends or the context is cancelled. Returns nil on clean shutdown.
func (m *Manager) sseConnect() error {
	ctx, cancel := context.WithCancel(context.Background())

	m.mu.Lock()
	m.sseCancel = cancel
	m.mu.Unlock()

	defer cancel()

	stream, err := m.wl.OpenEventStream(ctx)
	if err != nil {
		if ctx.Err() != nil {
			return nil // clean shutdown
		}
		return err
	}
	defer stream.Close()

	// Connected — REST bootstrap to populate the cache before processing SSE
	// events. ownsConnState=false: the block just below sets warlinkConnected
	// from the stream, which is the authority here; letting the bootstrap
	// declare the connection DOWN moments before the stream declares it UP was
	// always pointless and became misleading once this ran on a timer too.
	m.warlinkSync(false)

	m.mu.Lock()
	wasDisconnected := !m.warlinkConnected
	m.warlinkConnected = true
	m.warlinkError = nil
	m.mu.Unlock()
	if wasDisconnected {
		log.Printf("WarLink SSE connected: %s", m.baseURL())
		m.emitter.EmitWarLinkConnected()
	}

	// Stall detection: cancel the SSE context if no data arrives within 120s.
	// This catches silent TCP drops that leave the reader blocked forever.
	//
	// Note this does NOT catch the failure sseReconcileInterval addresses: a
	// stream that is healthy and delivering value-change events, while the
	// per-PLC connection statuses it carries are stale. Springfield's stream
	// was never stalled — it was talking the whole weekend.
	//
	// stalled is set BEFORE cancel() so the read loop below can tell this
	// cancellation apart from a shutdown. That distinction is load-bearing:
	// a context error normally means "we are stopping" (return nil, exit
	// cleanly), but a stall means "reconnect". Returning nil here is what
	// wedged Springfield on 2026-08-19 — the stall fired, the loop treated
	// its own recovery as a shutdown, and the edge spent five hours
	// "connected" to a stream that no longer existed.
	stallTimeout := sseStallTimeout
	stalled := make(chan struct{})
	stallTimer := time.NewTimer(stallTimeout)
	defer stallTimer.Stop()
	go func() {
		select {
		case <-stallTimer.C:
			log.Printf("WarLink SSE stall detected (no data for %s), reconnecting", stallTimeout)
			close(stalled)
			cancel()
		case <-ctx.Done():
		}
	}()

	// Periodic reconciliation — the self-healing half of SSE mode.
	//
	// status-change is EDGE-triggered: WarLink emits it on a transition, not
	// as a standing truth. The bootstrap warlinkPollTick above is level-
	// triggered, but it runs exactly ONCE per connection. When WarLink and
	// this edge come back at the same time — a core-box reboot, a WarLink
	// relocation — that single poll lands while WarLink is still opening its
	// own PLC connections, so the cache captures "Connecting" for most of
	// them. The transitions that would promote them then either predate our
	// subscription or are missed on the stream, and nothing ever re-asks.
	// IsConnected keeps returning false, pollReportingPoint early-returns,
	// and the cell silently stops counting until a human restarts the service.
	//
	// Observed at Springfield 2026-07-24: WarLink held 49/50 PLCs Connected
	// while the edge showed 10/50, for three and a half days, across a
	// weekend. Counting only resumed on a manual restart.
	//
	// Poll mode never had this failure, because warlinkPollTick runs every
	// PollRate. Re-running the same proven reconcile on a slow timer bounds
	// the wedge to one interval instead of "until somebody notices", and
	// costs one REST call a minute.
	go m.sseReconcileLoop(ctx)

	reader := NewSSEReader(stream)
	for {
		ev, isEvent, err := reader.Next()
		if err != nil {
			// A stall cancels the context from our own timer; every other
			// cancellation (StopWarLinkPoller, engine shutdown) is a stop.
			// See the stall-detection block above for why conflating them
			// kills the loop.
			select {
			case <-stalled:
				return fmt.Errorf("SSE stall: no data for %s", stallTimeout)
			default:
			}
			if err == io.EOF || ctx.Err() != nil {
				if ctx.Err() != nil {
					return nil // clean shutdown
				}
				return fmt.Errorf("SSE stream EOF")
			}
			return fmt.Errorf("SSE read: %w", err)
		}

		// Both events and keepalive comments are stream activity — reset the
		// stall timer on either. (Before the reader reported comments, a
		// keepalive-only stream would still trip the stall timer and churn
		// reconnects every interval; the health ticker's real events masked
		// this in production.)
		if !stallTimer.Stop() {
			select {
			case <-stallTimer.C:
			default:
			}
		}
		stallTimer.Reset(stallTimeout)
		if !isEvent {
			continue
		}

		switch ev.Event {
		case "value-change":
			m.handleSSEValueChange(ev.Data)
		case "status-change":
			m.handleSSEStatusChange(ev.Data)
		case "health":
			m.handleSSEHealth(ev.Data)
		default:
			// Ignore unknown event types
		}
	}
}

// sseReconcileLoop re-runs the level-triggered warlinkPollTick every
// sseReconcileInterval until ctx is cancelled. Returns when the SSE
// connection it belongs to goes away, so each connection owns exactly one.
func (m *Manager) sseReconcileLoop(ctx context.Context) {
	t := time.NewTicker(sseReconcileInterval)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			// ownsConnState=false: the STREAM owns whether WarLink is up. A
			// transient REST failure here must not flap the indicator or fire
			// a disconnect alert on a healthy stream.
			m.warlinkSync(false)
		case <-ctx.Done():
			return
		}
	}
}

func (m *Manager) handleSSEValueChange(data string) {
	var change sseValueChange
	if err := json.Unmarshal([]byte(data), &change); err != nil {
		log.Printf("SSE value-change decode: %v", err)
		return
	}
	if change.PLC == "" {
		log.Printf("SSE value-change: empty PLC name; dropping (would create permanent-flap ManagedPLC entry)")
		return
	}

	// Single WLock check-and-insert. Pre-fix had RLock→Unlock→WLock
	// which raced on concurrent first-event-for-new-PLC: two
	// goroutines could both observe 'not in map' under RLock, both
	// grab WLock, both insert — the loser's ManagedPLC was orphaned
	// and subsequent writes to mp.Values landed on the orphan.
	// Pattern mirrors handleSSEStatusChange:226-234 which already
	// uses single-WLock.
	m.mu.Lock()
	mp, ok := m.plcs[change.PLC]
	if !ok {
		// SSE may report PLCs discovered after bootstrap
		mp = newManagedPLC(change.PLC)
		m.plcs[change.PLC] = mp
	}
	m.mu.Unlock()

	mp.mu.Lock()
	mp.Values[change.Tag] = TagValue{
		Name:    change.Tag,
		TypeStr: change.Type,
		Value:   change.Value,
	}
	mp.mu.Unlock()
}

func (m *Manager) handleSSEStatusChange(data string) {
	var status sseStatusChange
	if err := json.Unmarshal([]byte(data), &status); err != nil {
		log.Printf("SSE status-change decode: %v", err)
		return
	}
	if status.PLC == "" {
		log.Printf("SSE status-change: empty PLC name; dropping (would create permanent-flap ManagedPLC entry)")
		return
	}

	// Normalize status to title case to match codebase convention
	if status.Status == "" {
		log.Printf("SSE status-change: empty status for PLC %s", status.PLC)
		return
	}
	normalized := strings.ToUpper(status.Status[:1]) + strings.ToLower(status.Status[1:])

	// Cross-lock-domain pattern: m.mu only guards the m.plcs map
	// (create-if-missing); per-PLC field writes (mp.Status, mp.Error,
	// etc.) go under mp.mu.Lock() because readers reach them through
	// mp.mu.RLock() (IsConnected, ReadTag, GetPLCHealth). Writing
	// these under m.mu instead would race the readers.
	m.mu.Lock()
	mp, ok := m.plcs[status.PLC]
	if !ok {
		mp = newManagedPLC(status.PLC)
		m.plcs[status.PLC] = mp
	}
	m.mu.Unlock()

	mp.mu.Lock()
	oldStatus := mp.Status
	mp.Status = normalized
	mp.Error = status.Error
	mp.ProductName = status.ProductName
	mp.Vendor = status.Vendor
	mp.mu.Unlock()

	if normalized == "Connected" && oldStatus != "Connected" {
		m.emitter.EmitPLCConnected(status.PLC)
	} else if normalized != "Connected" && oldStatus == "Connected" {
		var emitErr error
		if status.Error != "" {
			emitErr = errors.New(status.Error)
		}
		m.emitter.EmitPLCDisconnected(status.PLC, emitErr)
		m.emitter.EmitPLCHealthAlert(status.PLC, status.Error)
	}
}

func (m *Manager) handleSSEHealth(data string) {
	var health sseHealthUpdate
	if err := json.Unmarshal([]byte(data), &health); err != nil {
		log.Printf("SSE health decode: %v", err)
		return
	}
	if health.PLC == "" {
		log.Printf("SSE health: empty PLC name; dropping (would create permanent-flap ManagedPLC entry)")
		return
	}

	// Same cross-lock-domain pattern as handleSSEStatusChange above:
	// m.mu only guards the map; mp.Health write goes under mp.mu
	// because GetPLCHealth reads it under mp.mu.RLock().
	m.mu.Lock()
	mp, ok := m.plcs[health.PLC]
	if !ok {
		mp = newManagedPLC(health.PLC)
		m.plcs[health.PLC] = mp
	}
	m.mu.Unlock()

	mp.mu.Lock()
	hadPriorHealth := mp.Health != nil
	wasOnline := hadPriorHealth && mp.Health.Online
	mp.Health = &PLCHealth{
		Online:    health.Online,
		Driver:    health.Driver,
		Status:    health.Status,
		Error:     health.Error,
		Timestamp: health.Timestamp,
	}
	mp.mu.Unlock()

	// Detect transitions
	if wasOnline && !health.Online {
		m.emitter.EmitPLCHealthAlert(health.PLC, health.Error)
	} else if !wasOnline && health.Online && hadPriorHealth {
		// Only emit recover if we previously had health state (not first report)
		m.emitter.EmitPLCHealthRecover(health.PLC)
	}
}

// sseBackoff waits with capped exponential backoff + jitter.
// Returns false if a stop signal was received during the wait.
func (m *Manager) sseBackoff(attempt int) bool {
	// Base delay: 1s * 2^(attempt-1), capped at 30s
	// Cap the exponent to avoid int64 overflow (1<<63 wraps to 0).
	exp := attempt - 1
	if exp > 5 { // 2^5 = 32s, already above the 30s cap
		exp = 5
	}
	base := time.Duration(1<<uint(exp)) * time.Second
	if base > 30*time.Second {
		base = 30 * time.Second
	}

	// Add ±20% jitter
	jitter := time.Duration(float64(base) * (0.8 + 0.4*rand.Float64()))

	log.Printf("WarLink SSE reconnecting in %v (attempt %d)", jitter.Round(time.Millisecond), attempt)

	timer := time.NewTimer(jitter)
	defer timer.Stop()

	select {
	case <-m.stopChan:
		return false
	case <-m.warlinkStopChan:
		return false
	case <-timer.C:
		return true
	}
}
