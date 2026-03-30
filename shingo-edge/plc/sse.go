package plc

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"strings"
	"time"
)

// --- SSE event payload types (from WarLink) ---

type sseValueChange struct {
	PLC   string      `json:"plc"`
	Tag   string      `json:"tag"`
	Value interface{} `json:"value"`
	Type  string      `json:"type"`
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

	// Connected — REST bootstrap to populate cache before processing SSE events
	m.warlinkPollTick()

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
	const stallTimeout = 120 * time.Second
	stallTimer := time.NewTimer(stallTimeout)
	defer stallTimer.Stop()
	go func() {
		select {
		case <-stallTimer.C:
			log.Printf("WarLink SSE stall detected (no data for %s), reconnecting", stallTimeout)
			cancel()
		case <-ctx.Done():
		}
	}()

	reader := NewSSEReader(stream)
	for {
		ev, err := reader.Next()
		if err != nil {
			if err == io.EOF || ctx.Err() != nil {
				if ctx.Err() != nil {
					return nil // clean shutdown or stall timeout
				}
				return fmt.Errorf("SSE stream EOF")
			}
			return fmt.Errorf("SSE read: %w", err)
		}

		// Reset stall timer on every event (including comments/keepalives)
		if !stallTimer.Stop() {
			select {
			case <-stallTimer.C:
			default:
			}
		}
		stallTimer.Reset(stallTimeout)

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

func (m *Manager) handleSSEValueChange(data string) {
	var change sseValueChange
	if err := json.Unmarshal([]byte(data), &change); err != nil {
		log.Printf("SSE value-change decode: %v", err)
		return
	}

	m.mu.RLock()
	mp, ok := m.plcs[change.PLC]
	m.mu.RUnlock()

	if !ok {
		// SSE may report PLCs discovered after bootstrap
		mp = &ManagedPLC{
			Name:   change.PLC,
			Values: make(map[string]TagValue),
		}
		m.mu.Lock()
		m.plcs[change.PLC] = mp
		m.mu.Unlock()
	}

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

	// Normalize status to title case to match codebase convention
	if status.Status == "" {
		log.Printf("SSE status-change: empty status for PLC %s", status.PLC)
		return
	}
	normalized := strings.ToUpper(status.Status[:1]) + strings.ToLower(status.Status[1:])

	m.mu.Lock()
	mp, ok := m.plcs[status.PLC]
	if !ok {
		mp = &ManagedPLC{
			Name:   status.PLC,
			Values: make(map[string]TagValue),
		}
		m.plcs[status.PLC] = mp
	}
	oldStatus := mp.Status
	mp.Status = normalized
	mp.Error = status.Error
	mp.ProductName = status.ProductName
	mp.Vendor = status.Vendor
	m.mu.Unlock()

	if normalized == "Connected" && oldStatus != "Connected" {
		m.emitter.EmitPLCConnected(status.PLC)
	} else if normalized != "Connected" && oldStatus == "Connected" {
		var emitErr error
		if status.Error != "" {
			emitErr = fmt.Errorf("%s", status.Error)
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

	m.mu.Lock()
	mp, ok := m.plcs[health.PLC]
	if !ok {
		mp = &ManagedPLC{
			Name:   health.PLC,
			Values: make(map[string]TagValue),
		}
		m.plcs[health.PLC] = mp
	}

	hadPriorHealth := mp.Health != nil
	wasOnline := hadPriorHealth && mp.Health.Online
	mp.Health = &PLCHealth{
		Online:    health.Online,
		Driver:    health.Driver,
		Status:    health.Status,
		Error:     health.Error,
		Timestamp: health.Timestamp,
	}
	m.mu.Unlock()

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
