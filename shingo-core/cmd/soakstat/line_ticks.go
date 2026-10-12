package main

import (
	"database/sql"
	"fmt"
	"sort"
	"time"
)

// line_ticks.go — each line's last PLC tick (the orc's ruling on D2's after,
// §2.3). A line is a process with a counter on an Edge. One that stopped before
// the end is a finding at the top of the report: D2's after printed line 2's
// tick count as a line that kept running, when it had stopped and nothing was
// coming for it (TRACE-line2-empties).
//
// THE CLOCK IS THE COUNTER'S OWN: counter_snapshots.recorded_ms, which a sim
// build stamps on the sim clock, so every line on every Edge reads on one
// clock. "The end" is the last tick on any line read. A line stopped when its
// last tick is more than strandedQuiet sim-minutes before that: the quiet the
// stranded table already calls stranded.

type lineTick struct {
	line  string // the process's name
	ticks int64
	last  time.Time
}

// stoppedLine is a line whose last tick came long before the end.
type stoppedLine struct {
	station, line string
	last          time.Time
	before        time.Duration
}

// readLineTicks reads each process's PLC ticks and its last one. Rows written
// before process_id was stamped fold under their reporting point's PLC name.
func readLineTicks(db *sql.DB) ([]lineTick, error) {
	rows, err := db.Query(`SELECT COALESCE(p.name, rp.plc_name, ''), COUNT(*),
		       MAX(COALESCE(cs.recorded_ms, CAST(strftime('%s', cs.recorded_at) AS INTEGER) * 1000))
		FROM counter_snapshots cs
		JOIN reporting_points rp ON rp.id = cs.reporting_point_id
		LEFT JOIN processes p ON p.id = cs.process_id
		WHERE cs.delta > 0
		GROUP BY 1 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []lineTick
	for rows.Next() {
		var l lineTick
		var ms int64
		if err := rows.Scan(&l.line, &l.ticks, &ms); err != nil {
			return nil, err
		}
		l.last = time.UnixMilli(ms).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}

// lineEnd is the last tick on any line read.
func lineEnd(edges []*edgeReading) time.Time {
	var end time.Time
	for _, e := range edges {
		if e == nil {
			continue
		}
		for _, l := range e.lines {
			if l.last.After(end) {
				end = l.last
			}
		}
	}
	return end
}

// stoppedLines is every line that stopped, the longest stopped first.
func stoppedLines(edges []*edgeReading) []stoppedLine {
	end := lineEnd(edges)
	var out []stoppedLine
	for _, e := range edges {
		if e == nil {
			continue
		}
		for _, l := range e.lines {
			if before := end.Sub(l.last); before > strandedQuiet*time.Minute {
				out = append(out, stoppedLine{station: e.station, line: l.line, last: l.last, before: before})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].before > out[j].before })
	return out
}

// stoppedText is a stopped line's one sentence, the finding and the violation.
func stoppedText(s stoppedLine) string {
	return fmt.Sprintf("LINE STOPPED: %s %s last ticked at %s, %.1f sim-min before the last tick on any line",
		s.station, s.line, s.last.Format("15:04:05"), s.before.Minutes())
}

// printLineTicks is the table under the stranded orders: every line, its ticks,
// its last tick and how long before the end it came.
func printLineTicks(edges []*edgeReading) {
	end := lineEnd(edges)
	if end.IsZero() {
		fmt.Println("LINES  n/a (no Edge database read, or no PLC ticks in it)")
		fmt.Println()
		return
	}
	fmt.Printf("LINES  last PLC tick per line (the counter's clock; the end is the last tick on any line, %s)\n", end.Format("15:04:05"))
	for _, e := range edges {
		if e == nil {
			continue
		}
		for _, l := range e.lines {
			before := end.Sub(l.last)
			mark := ""
			if before > strandedQuiet*time.Minute {
				mark = "  STOPPED"
			}
			fmt.Printf("        %-14s %-12s ticks %-6d last %s  %.1f sim-min before the end%s\n",
				e.station, l.line, l.ticks, l.last.Format("15:04:05"), before.Minutes(), mark)
		}
	}
	fmt.Println()
}
