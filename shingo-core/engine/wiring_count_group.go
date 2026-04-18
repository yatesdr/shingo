// wiring_count_group.go — Count-group transition dispatch.
//
// handleCountGroupTransition builds a CountGroupCommand for the
// transition and broadcasts it to all edges via the outbox. Each edge
// evaluates the command against its own bindings map; unbound groups
// are logged and ignored, so the broadcast is safe for edges that have
// no stake in the group.

package engine

import (
	"fmt"
	"time"

	"github.com/google/uuid"

	"shingo/protocol"
)

// handleCountGroupTransition builds a CountGroupCommand for the transition
// and broadcasts it to all edges via the outbox. Every edge evaluates the
// command against its own bindings map; if the group isn't bound on that
// edge, it logs WARN and returns (no NACK, no tag write).
func (e *Engine) handleCountGroupTransition(ev CountGroupTransitionEvent) {
	cmd := &protocol.CountGroupCommand{
		CorrelationID:     uuid.NewString(),
		Group:             ev.Group,
		Desired:           ev.Desired,
		Robots:            ev.Robots,
		RobotCount:        len(ev.Robots),
		FailSafeTriggered: ev.FailSafeTriggered,
		Timestamp:         ev.Timestamp,
	}

	if err := e.SendDataToEdge(protocol.SubjectCountGroupCommand,
		protocol.StationBroadcast, cmd); err != nil {
		e.logFn("engine: countgroup dispatch group=%s desired=%s: %v",
			ev.Group, ev.Desired, err)
		return
	}

	// Audit row — one per transition, not per poll. Store under "countgroup"
	// entity with entityID=0; action distinguishes normal transitions from
	// fail-safe force-ons; newValue carries the full detail for forensics.
	action := "transition"
	if ev.FailSafeTriggered {
		action = "fail_safe_on"
	}
	detail := fmt.Sprintf("group=%s desired=%s robots=%d corr=%s",
		ev.Group, ev.Desired, len(ev.Robots), cmd.CorrelationID)
	e.db.AppendAudit("countgroup", 0, action, "", detail, "system")
	e.dbg("countgroup transition emitted: group=%s desired=%s robots=%d fail_safe=%v corr=%s elapsed=%s",
		ev.Group, ev.Desired, len(ev.Robots), ev.FailSafeTriggered, cmd.CorrelationID,
		time.Since(ev.Timestamp))
}
