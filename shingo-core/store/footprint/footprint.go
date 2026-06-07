// Package footprint computes the Operations Overview "Plant Footprint"
// metrics (plan §15.D): how much of the floor shingo manages and the
// load/unload velocity rhythm. Cross-table reads over edge_registry, bins,
// and cms_transactions — its own package so the cross-aggregate query doesn't
// muddy a single-domain store.
package footprint

import (
	"database/sql"
	"time"
)

// Footprint is the plant-footprint summary. Counts are plant-wide totals;
// the series cover the last 30 days (footprint is a growth/velocity story,
// not a snapshot, so it ignores the dashboard's station/robot filters).
type Footprint struct {
	CellsManaged int64    `json:"cells_managed"`
	BinsManaged  int64    `json:"bins_managed"`
	CellsSpark   []int64  `json:"cells_spark"` // cumulative-by-day, last 30 pts
	BinsSpark    []int64  `json:"bins_spark"`
	LoadSeries   []Bucket `json:"load_series"` // last 30 days, daily
}

// Bucket is one day of bin load/unload activity.
type Bucket struct {
	Day      time.Time `json:"day"`
	Loaded   int64     `json:"loaded"`
	Unloaded int64     `json:"unloaded"`
}

// Get assembles the footprint. Count queries are fatal; the trend series are
// best-effort (a failed series returns empty rather than failing the call).
func Get(db *sql.DB) (*Footprint, error) {
	fp := &Footprint{}
	if err := db.QueryRow(`SELECT COUNT(*) FROM edge_registry`).Scan(&fp.CellsManaged); err != nil {
		return nil, err
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM bins`).Scan(&fp.BinsManaged); err != nil {
		return nil, err
	}
	fp.CellsSpark = cumulativeDaily(db, `SELECT date_trunc('day', registered_at)::date, COUNT(*) FROM edge_registry GROUP BY 1 ORDER BY 1`, 30)
	fp.BinsSpark = cumulativeDaily(db, `SELECT date_trunc('day', created_at)::date, COUNT(*) FROM bins GROUP BY 1 ORDER BY 1`, 30)
	fp.LoadSeries = loadUnloadDaily(db, 30)
	return fp, nil
}

// cumulativeDaily runs a (day, count) query, accumulates a running total, and
// returns the last n cumulative values — the tail of the growth curve for a
// sparkline.
func cumulativeDaily(db *sql.DB, query string, n int) []int64 {
	rows, err := db.Query(query)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var cum int64
	var all []int64
	for rows.Next() {
		var d time.Time
		var c int64
		if err := rows.Scan(&d, &c); err != nil {
			return all
		}
		cum += c
		all = append(all, cum)
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}

// loadUnloadDaily returns zero-filled daily loaded/unloaded counts for the
// last n days, sourced from the bin_uop_audit event log (inventory refactor
// §16 PR 1). Loaded = op 'set_for_production' (operator established a bin's
// manifest); unloaded = 'clear_for_reuse' / 'released_empty' /
// 'released_partial' (bin reset/emptied). These op strings are the canonical,
// migration-stable tags in store/audit (audit.OpSetForProduction etc.).
//
// This replaces two broken sources (closes Q-011 / Q-015): the prior
// bins.loaded_at query undercounted (it's each bin's *latest* load, no
// historical log), and the cms_transactions `txn_type ILIKE '%clear%'` filter
// matched ZERO rows — txn_type's only values are increase/decrease
// (plant-data-findings.md Q-011). bin_uop_audit is the transactional bin
// lifecycle log, so these counts are now real.
func loadUnloadDaily(db *sql.DB, n int) []Bucket {
	loaded := dayCounts(db, `SELECT date_trunc('day', applied_at)::date, COUNT(*) FROM bin_uop_audit
		WHERE applied_at >= NOW() - ($1 || ' days')::interval
		  AND op = 'set_for_production' GROUP BY 1`, n)
	unloaded := dayCounts(db, `SELECT date_trunc('day', applied_at)::date, COUNT(*) FROM bin_uop_audit
		WHERE applied_at >= NOW() - ($1 || ' days')::interval
		  AND op IN ('clear_for_reuse', 'released_empty', 'released_partial') GROUP BY 1`, n)

	today := time.Now().Truncate(24 * time.Hour)
	out := make([]Bucket, 0, n)
	for i := n - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		key := day.Format("2006-01-02")
		out = append(out, Bucket{Day: day, Loaded: loaded[key], Unloaded: unloaded[key]})
	}
	return out
}

func dayCounts(db *sql.DB, query string, n int) map[string]int64 {
	m := make(map[string]int64)
	rows, err := db.Query(query, n)
	if err != nil {
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var d time.Time
		var c int64
		if err := rows.Scan(&d, &c); err != nil {
			return m
		}
		m[d.Format("2006-01-02")] = c
	}
	return m
}
