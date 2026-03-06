package store

import (
	"fmt"
	"strings"
	"time"
)

// parseTime converts a scanned timestamp value to time.Time.
// Handles both SQLite (returns string) and Postgres (returns time.Time).
func parseTime(v any) time.Time {
	switch t := v.(type) {
	case time.Time:
		return t
	case string:
		if t == "" {
			return time.Time{}
		}
		// Layouts with explicit timezone offsets — parse normally.
		for _, layout := range []string{
			time.RFC3339,
			time.RFC3339Nano,
			"2006-01-02 15:04:05-07:00",
			"2006-01-02 15:04:05.999999-07:00",
		} {
			if parsed, err := time.Parse(layout, t); err == nil {
				return parsed
			}
		}
		// Naive format (no timezone) — interpret as UTC.
		if parsed, err := time.ParseInLocation("2006-01-02 15:04:05", t, time.UTC); err == nil {
			return parsed
		}
	}
	return time.Time{}
}

// parseTimePtr is like parseTime but returns nil for zero/missing timestamps.
func parseTimePtr(v any) *time.Time {
	t := parseTime(v)
	if t.IsZero() {
		return nil
	}
	return &t
}

// Rebind rewrites ? placeholders to $1, $2, ... for PostgreSQL.
func Rebind(query string) string {
	n := 0
	var b strings.Builder
	for i := 0; i < len(query); i++ {
		if query[i] == '?' {
			n++
			b.WriteString(fmt.Sprintf("$%d", n))
		} else {
			b.WriteByte(query[i])
		}
	}
	return b.String()
}

// nullableInt64 converts a *int64 to a value suitable for SQL params (nil-safe).
func nullableInt64(p *int64) any {
	if p != nil {
		return *p
	}
	return nil
}

// nullableTime converts a *time.Time to a value suitable for SQL params (nil-safe).
// Normalizes to UTC for consistent storage.
func nullableTime(p *time.Time) any {
	if p != nil {
		utc := p.UTC()
		return utc
	}
	return nil
}
