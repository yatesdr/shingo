package plc

import (
	"bufio"
	"io"
	"strings"
)

// SSERawEvent represents a single parsed SSE event from the wire.
type SSERawEvent struct {
	Event string
	Data  string
	ID    string
}

// SSEReader reads SSE events from an io.Reader using a bufio.Scanner.
type SSEReader struct {
	scanner *bufio.Scanner

	// Partial event accumulated across Next calls. SSE comments (keepalives)
	// are legal anywhere — including between the data lines of a
	// still-arriving event — so reporting one mid-event must not discard what
	// already arrived.
	pending SSERawEvent
	// dataParts backs pending.Data across calls (Data is joined on dispatch).
	dataParts []string
	hasFields bool
}

// NewSSEReader creates a new SSE stream reader.
//
// The scanner's default 64KB buffer is too small for WarLink's large
// SSE events (array tags, BOOL[256], etc.). A single oversize event
// returns bufio.ErrTooLong and wedges the SSE channel into a reconnect
// loop. Cap at 8MB which is well above any plausible single event.
func NewSSEReader(r io.Reader) *SSEReader {
	s := bufio.NewScanner(r)
	s.Buffer(make([]byte, 64*1024), 8*1024*1024)
	return &SSEReader{scanner: s}
}

// Next returns the next complete SSE event. The bool is true when an event
// was dispatched; false with nil error means a comment line (keepalive) was
// consumed — activity on the stream, but not an event. Callers that run
// liveness timers (the stall detector does — a keepalive proves the stream
// is alive) need that distinction; callers that only want events ignore it.
// Returns io.EOF at end of stream.
func (s *SSEReader) Next() (SSERawEvent, bool, error) {
	for s.scanner.Scan() {
		line := s.scanner.Text()

		// Blank line dispatches the event
		if line == "" {
			if s.hasFields {
				ev := s.pending
				ev.Data = strings.Join(s.dataParts, "\n")
				s.pending = SSERawEvent{}
				s.dataParts = nil
				s.hasFields = false
				return ev, true, nil
			}
			continue
		}

		// Comment lines (starting with ':') are not events, but they are
		// stream activity: report them so callers can reset liveness timers.
		// Pending fields survive in s.pending/s.dataParts.
		if strings.HasPrefix(line, ":") {
			return SSERawEvent{}, false, nil
		}

		// Split on first ':'
		field := line
		value := ""
		if idx := strings.Index(line, ":"); idx >= 0 {
			field = line[:idx]
			value = strings.TrimPrefix(line[idx+1:], " ")
		}

		switch field {
		case "event":
			s.pending.Event = value
			s.hasFields = true
		case "data":
			s.dataParts = append(s.dataParts, value)
			s.hasFields = true
		case "id":
			s.pending.ID = value
			s.hasFields = true
		}
	}

	if err := s.scanner.Err(); err != nil {
		return SSERawEvent{}, false, err
	}

	// EOF with accumulated fields: dispatch final event
	if s.hasFields {
		ev := s.pending
		ev.Data = strings.Join(s.dataParts, "\n")
		s.pending = SSERawEvent{}
		s.dataParts = nil
		s.hasFields = false
		return ev, true, nil
	}

	return SSERawEvent{}, false, io.EOF
}
