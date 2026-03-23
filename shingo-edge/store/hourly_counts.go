package store

// HourlyCount represents accumulated production count for one hour.
type HourlyCount struct {
	ID        int64  `json:"id"`
	ProcessID int64  `json:"process_id"`
	StyleID   int64  `json:"style_id"`
	CountDate string `json:"count_date"`
	Hour      int    `json:"hour"`
	Delta     int64  `json:"delta"`
}

// UpsertHourlyCount adds delta to the existing count for the given process/style/date/hour,
// or inserts a new row if none exists.
func (db *DB) UpsertHourlyCount(processID, styleID int64, countDate string, hour int, delta int64) error {
	_, err := db.Exec(
		`INSERT INTO hourly_counts (process_id, style_id, count_date, hour, delta)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(process_id, style_id, count_date, hour)
		 DO UPDATE SET delta = delta + excluded.delta, updated_at = datetime('now')`,
		processID, styleID, countDate, hour, delta,
	)
	return err
}

// ListHourlyCounts returns all hourly count rows for a given process/style/date.
func (db *DB) ListHourlyCounts(processID, styleID int64, countDate string) ([]HourlyCount, error) {
	rows, err := db.Query(
		`SELECT id, process_id, style_id, count_date, hour, delta
		 FROM hourly_counts
		 WHERE process_id = ? AND style_id = ? AND count_date = ?
		 ORDER BY hour`,
		processID, styleID, countDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []HourlyCount
	for rows.Next() {
		var c HourlyCount
		if err := rows.Scan(&c.ID, &c.ProcessID, &c.StyleID, &c.CountDate, &c.Hour, &c.Delta); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// HourlyCountTotals returns per-hour totals for a process/date, summed across all styles.
func (db *DB) HourlyCountTotals(processID int64, countDate string) (map[int]int64, error) {
	rows, err := db.Query(
		`SELECT hour, SUM(delta) FROM hourly_counts
		 WHERE process_id = ? AND count_date = ?
		 GROUP BY hour ORDER BY hour`,
		processID, countDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	totals := make(map[int]int64)
	for rows.Next() {
		var hour int
		var sum int64
		if err := rows.Scan(&hour, &sum); err != nil {
			return nil, err
		}
		totals[hour] = sum
	}
	return totals, rows.Err()
}
