package countgroup

import (
	"context"
	"sync"
	"time"

	"shingo/protocol"
)

// HeartbeatWriter writes a monotonically-incrementing counter to the
// configured heartbeat tag on an interval. The PLC ladder watches this
// tag; if the value hasn't changed in >3s (ladder-side constant, not
// ours), the ladder drives all configured zone lights ON as fail-safe.
//
// Writes are gated by the Handler's `started` flag — see the comment
// on Handler.started. This is intentional.
//
// The writer also piggybacks ack-polling here: every tick, for each
// in-flight request in h.inFlight, re-read the tag. If zero, ack the
// command back to core. If non-zero past ack_dead, abandon it.
type HeartbeatWriter struct {
	h     *Handler
	logFn func(string, ...any)

	stopChan chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup

	counter int64 // single-writer (the loop goroutine) — no atomic needed
}

func NewHeartbeatWriter(h *Handler, logFn func(string, ...any)) *HeartbeatWriter {
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	return &HeartbeatWriter{
		h:        h,
		logFn:    logFn,
		stopChan: make(chan struct{}),
	}
}

func (w *HeartbeatWriter) Start() {
	w.wg.Add(1)
	go w.loop()
}

func (w *HeartbeatWriter) Stop() {
	w.stopOnce.Do(func() { close(w.stopChan) })
	w.wg.Wait()
}

func (w *HeartbeatWriter) loop() {
	defer w.wg.Done()

	interval := w.h.cfg.HeartbeatInterval
	if interval <= 0 {
		interval = 1 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-w.stopChan:
			return
		case <-ticker.C:
			w.tick()
		}
	}
}

func (w *HeartbeatWriter) tick() {
	// Gate: suppress heartbeat writes until Kafka subscription is
	// confirmed. See Handler.started comment — this is the `started`
	// guard, not a bug. Deadman trips ON during startup.
	if !w.h.IsStarted() {
		return
	}

	if w.h.cfg.HeartbeatTag == "" || w.h.cfg.HeartbeatPLC == "" {
		// Feature not fully configured. Do nothing; edge startup
		// validation will have already logged a WARN if bindings
		// reference groups without a matching heartbeat.
		return
	}

	// Monotonic counter. DINT wraps at ~2^31 (~68 years at 1Hz) —
	// not a concern, but ladder logic should detect staleness as
	// "value hasn't changed for >3s" not as "value below threshold".
	w.counter++

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := w.h.plc.WriteTagValue(ctx, w.h.cfg.HeartbeatPLC,
		w.h.cfg.HeartbeatTag, w.counter); err != nil {
		// A heartbeat write failure means the PLC deadman will trip
		// shortly. Log at INFO, not ERROR — this is expected during
		// WarLink outages and we don't want to spam the log.
		w.logFn("countgroup: heartbeat write failed (deadman will trip): %v", err)
	}

	// Piggyback ack-polling on the heartbeat tick.
	w.checkAcks(ctx)
}

// checkAcks re-reads every in-flight request tag. Tag=0 → acked; send
// CountGroupAck{Outcome:acked, AckLatencyMs}. Still non-zero + past
// ack_warn → log WARN. Still non-zero + past ack_dead → abandon.
//
// We snapshot under lock and release before making any network calls —
// sync.Map doesn't help here because the ack-confirmation step is a
// correlation-ID-gated delete, not a blind key op.
func (w *HeartbeatWriter) checkAcks(ctx context.Context) {
	h := w.h

	h.inFlightMu.Lock()
	snapshot := make([]*pendingRequest, 0, len(h.inFlight))
	for _, p := range h.inFlight {
		snapshot = append(snapshot, p)
	}
	h.inFlightMu.Unlock()

	for _, p := range snapshot {
		val, err := h.plc.ReadTagValue(ctx, p.PLC, p.Tag)
		if err != nil {
			// Transient read error; leave in-flight for next tick.
			h.logFn("countgroup: group=%s ack-check read failed: %v", p.Group, err)
			continue
		}

		if isZero(val) {
			// Ack confirmed.
			latency := time.Since(p.WrittenAt)
			h.inFlightMu.Lock()
			if cur, ok := h.inFlight[p.Group]; ok && cur.CorrelationID == p.CorrelationID {
				delete(h.inFlight, p.Group)
			}
			h.inFlightMu.Unlock()

			h.logFn("countgroup: group=%s ack OK in %s (corr=%s)",
				p.Group, latency.Round(time.Millisecond), p.CorrelationID)
			if h.ackSend != nil {
				if err := h.ackSend(&protocol.CountGroupAck{
					CorrelationID: p.CorrelationID,
					Group:         p.Group,
					Outcome:       protocol.AckOutcomeAcked,
					AckLatencyMs:  latency.Milliseconds(),
					Timestamp:     time.Now(),
				}); err != nil {
					h.logFn("countgroup: group=%s send acked ack: %v", p.Group, err)
				}
			}
			continue
		}

		elapsed := time.Since(p.WrittenAt)

		// Ack-dead: terminal. Abandon request, send ack_timeout, stop
		// polling. Light stays in whatever the PLC last latched. A
		// future command for this group will trigger bootstrap-clear
		// and self-heal. Do NOT clear the tag ourselves — PLC owns it.
		if elapsed > h.cfg.AckDead {
			h.logFn("countgroup: ERROR group=%s ack_dead after %s (tag=%s still=%v, corr=%s) — abandoning",
				p.Group, elapsed.Round(time.Second), p.Tag, val, p.CorrelationID)

			h.inFlightMu.Lock()
			if cur, ok := h.inFlight[p.Group]; ok && cur.CorrelationID == p.CorrelationID {
				delete(h.inFlight, p.Group)
			}
			h.inFlightMu.Unlock()

			if h.ackSend != nil {
				if err := h.ackSend(&protocol.CountGroupAck{
					CorrelationID: p.CorrelationID,
					Group:         p.Group,
					Outcome:       protocol.AckOutcomeTimeout,
					AckLatencyMs:  elapsed.Milliseconds(),
					Timestamp:     time.Now(),
				}); err != nil {
					h.logFn("countgroup: group=%s send timeout ack: %v", p.Group, err)
				}
			}
			continue
		}

		// Ack-warn: log once per request, keep polling.
		if elapsed > h.cfg.AckWarn && !p.WarnLogged {
			h.logFn("countgroup: WARN group=%s ack slow (%s elapsed, tag=%s still=%v)",
				p.Group, elapsed.Round(time.Millisecond), p.Tag, val)
			h.inFlightMu.Lock()
			if cur, ok := h.inFlight[p.Group]; ok && cur.CorrelationID == p.CorrelationID {
				cur.WarnLogged = true
			}
			h.inFlightMu.Unlock()
		}
	}
}
