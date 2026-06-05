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

// Next returns the next complete SSE event, or an error (io.EOF at end of stream).
// It blocks until a full event is available.
func (s *SSEReader) Next() (SSERawEvent, error) {
	var ev SSERawEvent
	var dataParts []string
	hasFields := false

	for s.scanner.Scan() {
		line := s.scanner.Text()

		// Blank line dispatches the event
		if line == "" {
			if hasFields {
				ev.Data = strings.Join(dataParts, "\n")
				return ev, nil
			}
			continue
		}

		// Comment lines (starting with ':') are ignored
		if strings.HasPrefix(line, ":") {
			continue
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
			ev.Event = value
			hasFields = true
		case "data":
			dataParts = append(dataParts, value)
			hasFields = true
		case "id":
			ev.ID = value
			hasFields = true
		}
	}

	if err := s.scanner.Err(); err != nil {
		return SSERawEvent{}, err
	}

	// EOF with accumulated fields: dispatch final event
	if hasFields {
		ev.Data = strings.Join(dataParts, "\n")
		return ev, nil
	}

	return SSERawEvent{}, io.EOF
}
