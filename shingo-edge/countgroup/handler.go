// Package countgroup implements the edge side of the advanced-zone
// light alert feature. It receives CountGroupCommand messages from
// core, runs Derek's request/ack handshake against PLC tags via
// WarLink, and ships CountGroupAck replies back.
//
// The package also owns the shared heartbeat writer (deadman) that
// lets the PLC ladder detect a dead edge and drive lights ON as
// fail-safe. The heartbeat is gated by a `started` flag that must be
// flipped true by the Kafka subscription callback — before that, the
// deadman is intentionally allowed to trip so a brief startup window
// errs on the safe side.
package countgroup

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"shingo/protocol"
	"shingoedge/config"
)

// AckSender sends a CountGroupAck back to core. In production this is
// *edge.Engine.SendCountGroupAck; in tests it's a function literal that
// captures the ack. Using a function type (not an interface) matches
// shingo's outbound messaging pattern (edge.Engine.sendFn).
type AckSender func(ack *protocol.CountGroupAck) error

// TagReadWriter is the minimal subset of plc.Manager the handler needs.
// Taking an interface here rather than a concrete Manager keeps the
// package independent of plc's public surface and makes testing trivial.
type TagReadWriter interface {
	ReadTagValue(ctx context.Context, plcName, tagName string) (interface{}, error)
	WriteTagValue(ctx context.Context, plcName, tagName string, value interface{}) error
}

// Handler processes CountGroupCommand messages from core. Safe for
// concurrent calls from the HandleData dispatcher.
type Handler struct {
	cfg     config.CountGroupsConfig
	plc     TagReadWriter
	ackSend AckSender
	logFn   func(string, ...any)

	// started is flipped to true by MarkStarted() after Kafka
	// subscription is confirmed. The heartbeat writer suppresses all
	// tag writes until started is true — see writeHeartbeat.
	//
	// RATIONALE (do not "fix" by starting heartbeat unconditionally):
	// between edge process boot and Kafka subscription being ready,
	// edge cannot receive CountGroupCommand. If the heartbeat writes
	// during that window, the PLC deadman is satisfied and lights hold
	// whatever (potentially stale) state they had before the restart.
	// Keeping the heartbeat suppressed lets the deadman trip ON during
	// the startup window — correct fail-safe behavior. Once Kafka is
	// ready, heartbeat resumes and the next core poll provides truth.
	//
	// atomic.Bool matches shingo's convention for write-once / read-many
	// lifecycle flags (see engine.Engine.fleetConnected, sceneSyncing).
	started atomic.Bool

	// inFlight tracks outstanding write requests per group so the
	// ack-poll loop (piggybacked on heartbeat) can confirm each.
	inFlightMu sync.Mutex
	inFlight   map[string]*pendingRequest
}

// pendingRequest is one command whose action code has been written to
// the request tag and is awaiting PLC ack (tag cleared back to 0).
type pendingRequest struct {
	CorrelationID string
	Group         string
	PLC           string
	Tag           string
	ActionCode    int
	WrittenAt     time.Time
	WarnLogged    bool
}

// New constructs a Handler. Call MarkStarted after the Kafka
// subscription is confirmed so the heartbeat begins.
func New(cfg config.CountGroupsConfig, tagRW TagReadWriter, ackSend AckSender, logFn func(string, ...any)) *Handler {
	if logFn == nil {
		logFn = func(string, ...any) {}
	}
	return &Handler{
		cfg:      cfg,
		plc:      tagRW,
		ackSend:  ackSend,
		logFn:    logFn,
		inFlight: make(map[string]*pendingRequest),
	}
}

// MarkStarted flips the `started` flag true, enabling heartbeat writes
// and ack polling. Call from the Kafka subscription callback after the
// dispatch-topic subscription is confirmed.
func (h *Handler) MarkStarted() { h.started.Store(true) }

// IsStarted reports whether MarkStarted has been called. Exported for
// tests and diagnostics.
func (h *Handler) IsStarted() bool { return h.started.Load() }

// OnCommand handles an incoming CountGroupCommand. Decode is done by
// the caller (edge_handler.go) so this entry point takes a typed
// struct — matches the existing SubjectDemandSignal pattern.
func (h *Handler) OnCommand(cmd protocol.CountGroupCommand) {
	binding, ok := h.cfg.Bindings[cmd.Group]
	if !ok {
		h.logFn("countgroup: received command for unbound group %q — check shingoedge.yaml bindings",
			cmd.Group)
		return
	}

	code, ok := h.cfg.Codes[cmd.Desired]
	if !ok {
		h.logFn("countgroup: unknown desired state %q for group %q (expected 'on' or 'off')",
			cmd.Desired, cmd.Group)
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Bootstrap clear — first request per group, OR after an ack-timeout
	// abandoned a prior request leaving the tag non-zero. Safe to do every
	// command: it's idempotent (zero stays zero) and costs one read.
	cur, err := h.plc.ReadTagValue(ctx, binding.PLC, binding.RequestTag)
	if err != nil {
		h.logFn("countgroup: group=%s read request tag failed: %v", cmd.Group, err)
		h.sendAck(&cmd, protocol.AckOutcomeWarlinkErr, 0)
		return
	}
	if !isZero(cur) {
		h.logFn("countgroup: group=%s stale request tag %s=%v cleared on bootstrap",
			cmd.Group, binding.RequestTag, cur)
		if err := h.plc.WriteTagValue(ctx, binding.PLC, binding.RequestTag, 0); err != nil {
			h.logFn("countgroup: group=%s bootstrap clear failed: %v", cmd.Group, err)
			h.sendAck(&cmd, protocol.AckOutcomeWarlinkErr, 0)
			return
		}
	}

	// Write the action code. PLC manager validates writable flag and PLC
	// connectivity via WarLink; any non-2xx bubbles up as an error.
	if err := h.plc.WriteTagValue(ctx, binding.PLC, binding.RequestTag, code); err != nil {
		h.logFn("countgroup: group=%s write %s=%d failed: %v",
			cmd.Group, binding.RequestTag, code, err)
		h.sendAck(&cmd, protocol.AckOutcomeWarlinkErr, 0)
		return
	}

	// Record in-flight. Ack-poll (piggybacked on heartbeat tick) will
	// watch for the PLC to clear the tag back to 0.
	h.inFlightMu.Lock()
	// If a previous request for this group is still pending, drop it
	// (depth-1 drop-oldest queue — safety state, not history).
	h.inFlight[cmd.Group] = &pendingRequest{
		CorrelationID: cmd.CorrelationID,
		Group:         cmd.Group,
		PLC:           binding.PLC,
		Tag:           binding.RequestTag,
		ActionCode:    code,
		WrittenAt:     time.Now(),
	}
	h.inFlightMu.Unlock()
}

// sendAck is a small helper that logs send failures without bubbling —
// an ack send failure doesn't affect the PLC state, just the audit trail.
func (h *Handler) sendAck(cmd *protocol.CountGroupCommand, outcome string, latencyMs int64) {
	if h.ackSend == nil {
		return
	}
	ack := &protocol.CountGroupAck{
		CorrelationID: cmd.CorrelationID,
		Group:         cmd.Group,
		Outcome:       outcome,
		AckLatencyMs:  latencyMs,
		Timestamp:     time.Now(),
	}
	if err := h.ackSend(ack); err != nil {
		h.logFn("countgroup: send ack group=%s outcome=%s: %v",
			cmd.Group, outcome, err)
	}
}

// isZero reports whether a WarLink-returned tag value is the integer 0.
// WarLink may return the value as int, int64, float64, or json.Number
// depending on decode path; we defensive-coerce.
func isZero(v interface{}) bool {
	switch x := v.(type) {
	case nil:
		return true
	case int:
		return x == 0
	case int32:
		return x == 0
	case int64:
		return x == 0
	case float64:
		return x == 0
	case json.Number:
		n, err := x.Int64()
		return err == nil && n == 0
	}
	return false
}
