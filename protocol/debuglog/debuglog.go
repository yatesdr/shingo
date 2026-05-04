package debuglog

import (
	"fmt"
	"io"
	"log"
	"os"
	"sync"
	"time"
)

// Entry represents a single debug log entry.
type Entry struct {
	Time      time.Time `json:"time"`
	Subsystem string    `json:"subsystem"`
	Message   string    `json:"message"`
}

// Logger provides an always-active ring buffer and optional file output for debug logging.
type Logger struct {
	mu      sync.Mutex
	entries []Entry
	pos     int
	full    bool
	size    int

	file    *os.File
	fileLog *log.Logger
	filter  map[string]bool // nil = all subsystems to file; non-nil = only these

	// stderr mirror: when non-nil, every Log() also writes a single
	// formatted line here. Default is os.Stderr, set in New(). Lets
	// engine/dispatch error diagnostics that used to go through
	// log.Printf reach the ring buffer + browser UI without losing
	// stderr/journal coverage that ops may grep during incidents.
	stderr io.Writer

	onEntry func(Entry)
}

// New creates a Logger with a ring buffer of the given size.
// fileFilter controls file output:
//   - nil: no file output (ring buffer only)
//   - empty slice: file output for all subsystems
//   - non-empty: file output only for listed subsystems
//
// The file is always "shingo-debug.log", truncated on open.
func New(ringSize int, fileFilter []string) (*Logger, error) {
	l := &Logger{
		entries: make([]Entry, ringSize),
		size:    ringSize,
		stderr:  os.Stderr,
	}

	if fileFilter != nil {
		f, err := os.Create("shingo-debug.log")
		if err != nil {
			return nil, fmt.Errorf("open debug log: %w", err)
		}
		l.file = f
		l.fileLog = log.New(f, "", 0)

		if len(fileFilter) > 0 {
			l.filter = make(map[string]bool, len(fileFilter))
			for _, s := range fileFilter {
				l.filter[s] = true
			}
		}
		// filter == nil means all subsystems pass
	}

	return l, nil
}

// Close closes the log file if open.
func (l *Logger) Close() error {
	if l.file != nil {
		return l.file.Close()
	}
	return nil
}

// SetOnEntry sets a callback invoked for each new entry (e.g. SSE broadcast).
// The callback is called outside the lock, so it is safe to do blocking work.
func (l *Logger) SetOnEntry(fn func(Entry)) {
	l.mu.Lock()
	l.onEntry = fn
	l.mu.Unlock()
}

// SetStderr sets (or clears, with nil) the stderr mirror writer. Each Log
// call writes one formatted line here outside the ring buffer mutex.
// Defaults to os.Stderr; set to nil in tests that want quiet output.
func (l *Logger) SetStderr(w io.Writer) {
	l.mu.Lock()
	l.stderr = w
	l.mu.Unlock()
}

// Log writes an entry to the ring buffer (always), mirrors to stderr (if
// configured), and writes to the file (if enabled and subsystem passes filter).
func (l *Logger) Log(subsystem, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	e := Entry{
		Time:      time.Now().UTC(),
		Subsystem: subsystem,
		Message:   msg,
	}

	l.mu.Lock()
	l.entries[l.pos] = e
	l.pos = (l.pos + 1) % l.size
	if l.pos == 0 || l.full {
		l.full = true
	}
	cb := l.onEntry
	stderr := l.stderr
	l.mu.Unlock()

	if cb != nil {
		cb(e)
	}

	if stderr != nil {
		fmt.Fprintf(stderr, "%s [%s] %s\n", e.Time.Format("2006-01-02T15:04:05.000Z"), subsystem, msg)
	}

	if l.file != nil {
		if l.filter == nil || l.filter[subsystem] {
			l.fileLog.Printf("%s [%s] %s", e.Time.Format("2006-01-02T15:04:05.000Z"), subsystem, msg)
		}
	}
}

// Func returns a log function scoped to a subsystem. Always returns a non-nil function.
func (l *Logger) Func(subsystem string) func(string, ...any) {
	return func(format string, args ...any) {
		l.Log(subsystem, format, args...)
	}
}

// Entries returns ring buffer entries filtered by subsystem ("" = all), oldest first.
func (l *Logger) Entries(subsystem string) []Entry {
	l.mu.Lock()
	defer l.mu.Unlock()

	var count int
	if l.full {
		count = l.size
	} else {
		count = l.pos
	}

	raw := make([]Entry, count)
	if l.full {
		n := copy(raw, l.entries[l.pos:])
		copy(raw[n:], l.entries[:l.pos])
	} else {
		copy(raw, l.entries[:l.pos])
	}

	if subsystem == "" {
		return raw
	}

	out := make([]Entry, 0, len(raw)/2)
	for _, e := range raw {
		if e.Subsystem == subsystem {
			out = append(out, e)
		}
	}
	return out
}

// Subsystems returns distinct subsystem tags currently in the ring buffer.
func (l *Logger) Subsystems() []string {
	l.mu.Lock()
	defer l.mu.Unlock()

	var count int
	if l.full {
		count = l.size
	} else {
		count = l.pos
	}

	seen := make(map[string]bool)
	for i := 0; i < count; i++ {
		s := l.entries[i].Subsystem
		if s != "" {
			seen[s] = true
		}
	}

	out := make([]string, 0, len(seen))
	for s := range seen {
		out = append(out, s)
	}
	return out
}

// FileEnabled returns true if file output is active.
func (l *Logger) FileEnabled() bool {
	return l.file != nil
}
