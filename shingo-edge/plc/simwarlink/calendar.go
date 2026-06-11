//go:build sim

package simwarlink

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ProductionCalendar answers "is this time on-shift?" for the sim's
// production calendar. A nil calendar (or one with no shifts) always
// returns true — backward-compatible with existing configs.
//
// The calendar operates in wall-clock local time (the plant's timezone).
// For the sim, we use UTC — the dev config specifies shifts in UTC.
type ProductionCalendar struct {
	shifts   []shiftSpan // parsed shift boundaries
	weekends map[time.Weekday]bool
}

type shiftSpan struct {
	startH, startM int
	endH, endM     int
	breaks         []breakSpan
}

type breakSpan struct {
	startH, startM int
	endH, endM     int
}

// CalendarConfig is the subset of config needed by the calendar. Matches
// config.SimCalendarConfig structurally.
type CalendarConfig struct {
	Enabled bool
	Shifts  []ShiftConfig
	Weekend []time.Weekday
}

// ShiftConfig defines one shift's time window and breaks.
type ShiftConfig struct {
	Start  string
	End    string
	Breaks []BreakConfig
}

// BreakConfig defines a break within a shift.
type BreakConfig struct {
	Start string
	End   string
}

// NewProductionCalendar parses the config. Returns nil if not enabled
// or if no shifts are configured (backward-compatible: always on-shift).
func NewProductionCalendar(cfg CalendarConfig) *ProductionCalendar {
	if !cfg.Enabled || len(cfg.Shifts) == 0 {
		return nil
	}
	cal := &ProductionCalendar{
		weekends: make(map[time.Weekday]bool),
	}
	for _, d := range cfg.Weekend {
		cal.weekends[d] = true
	}
	for _, s := range cfg.Shifts {
		sh, sm, err := parseHM(s.Start)
		if err != nil {
			continue
		}
		eh, em, err := parseHM(s.End)
		if err != nil {
			continue
		}
		ss := shiftSpan{startH: sh, startM: sm, endH: eh, endM: em}
		for _, b := range s.Breaks {
			bsh, bsm, err := parseHM(b.Start)
			if err != nil {
				continue
			}
			beh, bem, err := parseHM(b.End)
			if err != nil {
				continue
			}
			ss.breaks = append(ss.breaks, breakSpan{startH: bsh, startM: bsm, endH: beh, endM: bem})
		}
		cal.shifts = append(cal.shifts, ss)
	}
	if len(cal.shifts) == 0 {
		return nil
	}
	return cal
}

// IsOnShift returns true if the given time is during a production shift
// (not on a weekend, not during a break).
func (c *ProductionCalendar) IsOnShift(t time.Time) bool {
	if c == nil {
		return true
	}
	// Check weekend
	if c.weekends[t.Weekday()] {
		return false
	}
	h, m := t.Hour(), t.Minute()
	for _, s := range c.shifts {
		if withinHM(h, m, s.startH, s.startM, s.endH, s.endM) {
			// In shift — check breaks
			for _, b := range s.breaks {
				if withinHM(h, m, b.startH, b.startM, b.endH, b.endM) {
					return false // on break
				}
			}
			return true
		}
	}
	return false // not in any shift
}

// withinHM returns true if hh:mm is in [startH:startM, endH:endM).
func withinHM(h, m, startH, startM, endH, endM int) bool {
	t := h*60 + m
	s := startH*60 + startM
	e := endH*60 + endM
	return t >= s && t < e
}

func parseHM(s string) (int, int, error) {
	parts := strings.SplitN(s, ":", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("parse HM %q: want HH:MM", s)
	}
	h, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, fmt.Errorf("parse hour %q: %w", parts[0], err)
	}
	m, err := strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, fmt.Errorf("parse minute %q: %w", parts[1], err)
	}
	if h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("HM out of range: %02d:%02d", h, m)
	}
	return h, m, nil
}
