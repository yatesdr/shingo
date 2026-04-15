package countgroup

import "time"

// Transition is the payload emitted by the loop whenever a group's
// debounced occupancy flips. The engine wiring subscriber translates
// this into a protocol.CountGroupCommand and ships it to edge via the
// outbox.
type Transition struct {
	Group             string
	Desired           string // "on" | "off"
	Robots            []string
	FailSafeTriggered bool
	Timestamp         time.Time
}

// Emitter is the sink for Transition events. In production this is
// an EventBus adapter; in tests it's a recording mock.
type Emitter interface {
	Emit(t Transition)
}
