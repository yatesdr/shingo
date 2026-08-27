//go:build sim

package service

import (
	"fmt"
	"time"
)

// SeedDemoLinesideBuckets captures a handful of demo lineside buckets on
// the first process's first few nodes so the Production page's Lineside
// Buckets table has rows to modify/clear. Sim builds only.
//
// Idempotent-ish: Capture merges into an existing active bucket for the
// same (node, style, part), so re-running the seeder tops up qtys rather
// than creating duplicates. Uses a fixed part-number set so re-runs hit
// the same rows.
func (s *ProcessService) SeedDemoLinesideBuckets() (int, error) {
	processes, err := s.List()
	if err != nil {
		return 0, fmt.Errorf("list processes: %w", err)
	}
	if len(processes) == 0 {
		return 0, fmt.Errorf("no processes configured — create a process first")
	}
	p := processes[0]

	nodes, err := s.ListNodesByProcess(p.ID)
	if err != nil {
		return 0, fmt.Errorf("list nodes: %w", err)
	}
	if len(nodes) == 0 {
		return 0, fmt.Errorf("no process_nodes on process %q — add nodes first", p.Name)
	}

	styles, err := s.db.ListStyles()
	if err != nil {
		return 0, fmt.Errorf("list styles: %w", err)
	}
	var styleID int64
	if len(styles) > 0 {
		styleID = styles[0].ID
	}

	demoBuckets := []struct {
		nodeIdx    int
		partNumber string
		qty        int
		pairKey    string
	}{
		{0, "DEMO-PART-A", 42, ""},
		{0, "DEMO-PART-B", 17, ""},
		{1, "DEMO-PART-A", 88, ""},
		{2, "DEMO-PART-C", 5, ""},
	}

	inserted := 0
	for _, d := range demoBuckets {
		if d.nodeIdx >= len(nodes) {
			break
		}
		nodeID := nodes[d.nodeIdx].ID
		if _, err := s.db.CaptureLinesideBucket(nodeID, d.pairKey, styleID, d.partNumber, d.qty); err != nil {
			return inserted, fmt.Errorf("capture bucket on node %d: %w", nodeID, err)
		}
		// Mark as active explicitly (Capture already sets active state).
		inserted++
	}
	return inserted, nil
}

// SeedDemoHourlyCountsForProcess seeds 500 parts per shift (1500 total)
// for a single process, spread unevenly across 24 hours. Re-running is
// safe: existing rows for the process+date are wiped first. Sim builds only.
func (s *CounterService) SeedDemoHourlyCountsForProcess(processID, styleID int64, countDate string) error {
	if processID == 0 {
		return fmt.Errorf("no process selected")
	}
	if countDate == "" {
		countDate = time.Now().Format("2006-01-02")
	}

	if _, err := s.db.Exec(
		`DELETE FROM hourly_counts WHERE process_id = ? AND count_date = ?`,
		processID, countDate,
	); err != nil {
		return fmt.Errorf("clear existing hourly_counts: %w", err)
	}

	// 500 parts per shift, spread unevenly across the shift's 8 hours.
	// 1st shift: 06:00-14:00 (hours 6-13)
	// 2nd shift: 14:00-22:00 (hours 14-21)
	// 3rd shift: 22:00-06:00 (hours 22,23,0,1,2,3,4,5)
	demo := map[int]int64{
		// 3rd shift (500 total) — tapers overnight
		0: 82, 1: 71, 2: 63, 3: 54, 4: 45, 5: 38, 22: 73, 23: 74,
		// 1st shift (500 total) — ramps up mid-morning
		6: 48, 7: 62, 8: 71, 9: 78, 10: 82, 11: 76, 12: 53, 13: 30,
		// 2nd shift (500 total) — peaks late afternoon
		14: 55, 15: 68, 16: 74, 17: 81, 18: 79, 19: 65, 20: 48, 21: 30,
	}
	for hour, delta := range demo {
		if err := s.db.UpsertHourlyCount(processID, styleID, countDate, hour, delta); err != nil {
			return fmt.Errorf("upsert hour %d: %w", hour, err)
		}
	}
	return nil
}

// SeedDemoHourlyCountsClear wipes all hourly_counts rows for a process+date.
// Used by the loader-graphs seeder to clear old data from every process
// before injecting fresh data on the three loaders. Sim builds only.
func (s *CounterService) SeedDemoHourlyCountsClear(processID int64, countDate string) error {
	if countDate == "" {
		countDate = time.Now().Format("2006-01-02")
	}
	_, err := s.db.Exec(
		`DELETE FROM hourly_counts WHERE process_id = ? AND count_date = ?`,
		processID, countDate,
	)
	return err
}

// SeedDemoHourlyCountsAllProcesses seeds demo hourly counts for every
// process, each with a different volume profile so the "All Processes"
// graph shows meaningful combined totals. Sim builds only.
//
// Each process gets a scaled variant of the base 24-hour profile so the
// per-process totals differ but the combined view still looks realistic.
// Re-running is safe: each process's rows for the date are wiped first.
func (s *CounterService) SeedDemoHourlyCountsAllProcesses(countDate string) (int, error) {
	if countDate == "" {
		countDate = time.Now().Format("2006-01-02")
	}

	processes, err := s.db.ListProcesses()
	if err != nil {
		return 0, fmt.Errorf("list processes: %w", err)
	}
	if len(processes) == 0 {
		return 0, fmt.Errorf("no processes configured")
	}

	styles, err := s.db.ListStyles()
	if err != nil {
		return 0, fmt.Errorf("list styles: %w", err)
	}

	// Base 24-hour profile (totals 3547). Each process gets a multiplier
	// so the combined total across N processes is meaningful and varies
	// per-process.
	base := map[int]int64{
		0: 41, 1: 35, 2: 33, 3: 28, 4: 24, 5: 18,
		6: 62, 7: 78, 8: 55, 9: 71, 10: 44, 11: 83, 12: 59, 13: 68,
		14: 49, 15: 66, 16: 57, 17: 82, 18: 45, 19: 63, 20: 54, 21: 77,
		22: 38, 23: 29,
	}

	// Per-process multipliers — different production volumes per line.
	multipliers := []float64{1.0, 0.65, 1.3, 0.45, 0.85, 1.15, 0.55, 0.95}

	seeded := 0
	for i, p := range processes {
		// Find the first style for this process (hourly_counts requires a style_id FK).
		var styleID int64
		for _, st := range styles {
			if st.ProcessID == p.ID {
				styleID = st.ID
				break
			}
		}
		if styleID == 0 {
			continue // no style for this process — skip
		}

		mult := multipliers[i%len(multipliers)]

		// Wipe existing rows for this process+date.
		if _, err := s.db.Exec(
			`DELETE FROM hourly_counts WHERE process_id = ? AND count_date = ?`,
			p.ID, countDate,
		); err != nil {
			return seeded, fmt.Errorf("clear hourly_counts for process %d: %w", p.ID, err)
		}

		for hour, delta := range base {
			scaled := int64(float64(delta) * mult)
			if scaled < 0 {
				scaled = 0
			}
			if err := s.db.UpsertHourlyCount(p.ID, styleID, countDate, hour, scaled); err != nil {
				return seeded, fmt.Errorf("upsert hour %d for process %d: %w", hour, p.ID, err)
			}
		}
		seeded++
	}
	return seeded, nil
}
