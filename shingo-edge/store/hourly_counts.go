package store

// HourlyCount represents accumulated production count for one hour.
type HourlyCount struct {
	ID         int64  `json:"id"`
	LineID     int64  `json:"line_id"`
	JobStyleID int64  `json:"job_style_id"`
	CountDate  string `json:"count_date"`
	Hour       int    `json:"hour"`
	Delta      int64  `json:"delta"`
}

// UpsertHourlyCount adds delta to the existing count for the given line/style/date/hour,
// or inserts a new row if none exists.
func (db *DB) UpsertHourlyCount(lineID, jobStyleID int64, countDate string, hour int, delta int64) error {
	_, err := db.Exec(
		`INSERT INTO hourly_counts (line_id, job_style_id, count_date, hour, delta)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(line_id, job_style_id, count_date, hour)
		 DO UPDATE SET delta = delta + excluded.delta, updated_at = datetime('now')`,
		lineID, jobStyleID, countDate, hour, delta,
	)
	return err
}

// ListHourlyCounts returns all hourly count rows for a given line/style/date.
func (db *DB) ListHourlyCounts(lineID, jobStyleID int64, countDate string) ([]HourlyCount, error) {
	rows, err := db.Query(
		`SELECT id, line_id, job_style_id, count_date, hour, delta
		 FROM hourly_counts
		 WHERE line_id = ? AND job_style_id = ? AND count_date = ?
		 ORDER BY hour`,
		lineID, jobStyleID, countDate,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var counts []HourlyCount
	for rows.Next() {
		var c HourlyCount
		if err := rows.Scan(&c.ID, &c.LineID, &c.JobStyleID, &c.CountDate, &c.Hour, &c.Delta); err != nil {
			return nil, err
		}
		counts = append(counts, c)
	}
	return counts, rows.Err()
}

// HourlyCountTotals returns per-hour totals for a line/date, summed across all job styles.
func (db *DB) HourlyCountTotals(lineID int64, countDate string) (map[int]int64, error) {
	rows, err := db.Query(
		`SELECT hour, SUM(delta) FROM hourly_counts
		 WHERE line_id = ? AND count_date = ?
		 GROUP BY hour ORDER BY hour`,
		lineID, countDate,
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
