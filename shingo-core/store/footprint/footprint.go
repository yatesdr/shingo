// Package footprint computes the Operations Overview "Plant Footprint"
// metrics (plan §15.D): how much of the floor shingo manages and the
// load/unload velocity rhythm. Cross-table reads over edge_registry, bins,
// and cms_transactions — its own package so the cross-aggregate query doesn't
// muddy a single-domain store.
package footprint

import (
	"database/sql"
	"fmt"
	"log"
	"strings"
	"time"
)

// Footprint is the plant-footprint summary. Counts are plant-wide totals;
// the series cover the last 30 days (footprint is a growth/velocity story,
// not a snapshot, so it ignores the dashboard's station/robot filters).
type Footprint struct {
	// CellsManaged counts edge_registry rows — i.e. edge/line registrations
	// (e.g. "devplant.line1"), NOT physical cells. Surfacing real cells
	// (weld cells / presses) needs the PLCName/cell identity the edge currently
	// drops at emit (Q-034); until then the dashboard labels this "Lines".
	CellsManaged int64 `json:"cells_managed"`
	// ProcessesManaged is the count of distinct producing processes across lines
	// (cell_part_events) — the real production grain available today.
	ProcessesManaged int64 `json:"processes_managed"`
	BinsManaged      int64 `json:"bins_managed"` // total — the headline
	// Full vs empty split (Q-032): full = uop_remaining > 0; empty = the rest.
	// BinsFull + BinsEmpty == BinsManaged. The Inventory page owns the richer
	// three-way lifecycle split (stocked / in-production-empty / idle).
	BinsFull   int64   `json:"bins_full"`
	BinsEmpty  int64   `json:"bins_empty"`
	CellsSpark []int64 `json:"cells_spark"` // cumulative-by-day, last 30 pts
	BinsSpark  []int64 `json:"bins_spark"`  // cumulative total (kept for back-compat)
	// BinsSeries is true historical full/empty per day, reconstructed by replaying
	// the bin_uop_audit uop log (Q-032). Depth is limited to the log's history.
	BinsSeries []BinDayBucket `json:"bins_series"`
	LoadSeries []Bucket       `json:"load_series"` // last 30 days, daily
}

// BinDayBucket is one day's reconstructed bin occupancy: total bins managed and
// the full (uop > 0) / empty split as of end-of-day.
type BinDayBucket struct {
	Day   time.Time `json:"day"`
	Full  int64     `json:"full"`
	Empty int64     `json:"empty"`
	Total int64     `json:"total"`
}

// Bucket is one day of bin load/unload activity.
type Bucket struct {
	Day      time.Time `json:"day"`
	Loaded   int64     `json:"loaded"`
	Unloaded int64     `json:"unloaded"`
}

// Get assembles the footprint. Count queries are fatal; the trend series are
// best-effort (a failed series returns empty rather than failing the call).
// loadedOp / unloadedOps are the bin_uop_audit op tags counted as loaded vs
// unloaded; the caller (outer store layer) supplies them from store/audit so
// this sub-package doesn't cross-import another store aggregate.
func Get(db *sql.DB, loc *time.Location, loadedOp string, unloadedOps []string) (*Footprint, error) {
	if loc == nil {
		loc = time.UTC
	}
	fp := &Footprint{}
	if err := db.QueryRow(`SELECT COUNT(*) FROM edge_registry`).Scan(&fp.CellsManaged); err != nil {
		return nil, err
	}
	// Distinct producing processes across lines — best-effort (cell_part_events
	// may be empty/absent). Physical cells aren't distinguishable yet (Q-034).
	_ = db.QueryRow(`SELECT COUNT(DISTINCT (cell_id, process_id)) FROM cell_part_events`).Scan(&fp.ProcessesManaged)
	// Full = a bin currently holding parts (uop_remaining > 0); empty = the rest.
	if err := db.QueryRow(`SELECT COUNT(*), COUNT(*) FILTER (WHERE uop_remaining > 0) FROM bins`).
		Scan(&fp.BinsManaged, &fp.BinsFull); err != nil {
		return nil, err
	}
	fp.BinsEmpty = fp.BinsManaged - fp.BinsFull
	fp.CellsSpark = cumulativeDaily(db, `SELECT date_trunc('day', registered_at)::date, COUNT(*) FROM edge_registry GROUP BY 1 ORDER BY 1`, 30)
	fp.BinsSpark = cumulativeDaily(db, `SELECT date_trunc('day', created_at)::date, COUNT(*) FROM bins GROUP BY 1 ORDER BY 1`, 30)
	fp.BinsSeries = binsOccupancyDaily(db, loc, 30)
	fp.LoadSeries = loadUnloadDaily(db, loc, loadedOp, unloadedOps, 30)
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

// binsOccupancyDaily reconstructs total/full/empty bins per (plant-local) day
// over the trailing n days by replaying the bin_uop_audit uop log: a bin's uop
// as of a day is the after_uop of its latest change up to that day (else the
// before_uop of its first later change, else its current uop if never logged).
// full = uop > 0. History depth is bounded by how far back bin_uop_audit goes;
// the (bin_id, applied_at DESC) index keeps the per-day lookups cheap.
func binsOccupancyDaily(db *sql.DB, loc *time.Location, n int) []BinDayBucket {
	tz := loc.String()
	rows, err := db.Query(`
		WITH days AS (
			SELECT generate_series(
				date_trunc('day', now() AT TIME ZONE $1) - (($2 - 1) || ' days')::interval,
				date_trunc('day', now() AT TIME ZONE $1),
				interval '1 day') AS dl)
		SELECT (dl AT TIME ZONE $1) AS day,
			count(q.id) FILTER (WHERE q.uop > 0)               AS full,
			count(q.id) FILTER (WHERE COALESCE(q.uop, 0) <= 0) AS empty,
			count(q.id)                                        AS total
		FROM days
		LEFT JOIN LATERAL (
			SELECT b.id, COALESCE(
				(SELECT a.after_uop  FROM bin_uop_audit a WHERE a.bin_id = b.id AND a.applied_at <  (dl + interval '1 day') AT TIME ZONE $1 ORDER BY a.applied_at DESC LIMIT 1),
				(SELECT a.before_uop FROM bin_uop_audit a WHERE a.bin_id = b.id AND a.applied_at >= (dl + interval '1 day') AT TIME ZONE $1 ORDER BY a.applied_at ASC  LIMIT 1),
				b.uop_remaining) AS uop
			FROM bins b WHERE b.created_at < (dl + interval '1 day') AT TIME ZONE $1) q ON TRUE
		GROUP BY dl ORDER BY dl`, tz, n)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []BinDayBucket
	for rows.Next() {
		var b BinDayBucket
		if err := rows.Scan(&b.Day, &b.Full, &b.Empty, &b.Total); err != nil {
			return out
		}
		out = append(out, b)
	}
	return out
}

// loadUnloadDaily returns zero-filled daily loaded/unloaded counts for the
// last n days, sourced from the bin_uop_audit event log (inventory refactor
// §16 PR 1). Loaded = op 'set_for_production' (operator established a bin's
// manifest); unloaded = the full bin-release family (audit.ReleaseFamilyOps).
//
// Two bugs fixed here (Q-036): (1) the unloaded set was hardcoded to three ops
// while live unloads also write released_capture_empty / released_underpack —
// now derived from audit.ReleaseFamilyOps so it can't drift again; (2) days
// were keyed in UTC on the Go side (time.Now().Truncate(24h)) but in the
// Postgres server timezone in SQL — a mismatch that dropped counts. Both sides
// now key in the plant timezone (loc).
//
// This replaced two earlier broken sources (closed Q-011 / Q-015): the prior
// bins.loaded_at query undercounted (each bin's *latest* load only), and the
// cms_transactions `txn_type ILIKE '%clear%'` filter matched zero rows.
func loadUnloadDaily(db *sql.DB, loc *time.Location, loadedOp string, unloadedOps []string, n int) []Bucket {
	tz := loc.String()
	loaded := opDayCounts(db, tz, n, loadedOp)
	unloaded := opDayCounts(db, tz, n, unloadedOps...)

	out := make([]Bucket, 0, n)
	for _, day := range plantDayKeys(time.Now(), loc, n) {
		key := day.Format("2006-01-02")
		out = append(out, Bucket{Day: day, Loaded: loaded[key], Unloaded: unloaded[key]})
	}
	return out
}

// plantDayKeys returns the trailing n plant-local calendar-day midnights,
// oldest first, anchored on now. Keying the Go day axis in the plant zone (not
// UTC) is half of the Q-036 day-key fix; opDayCounts buckets the SQL side in
// the same zone so the string keys line up.
func plantDayKeys(now time.Time, loc *time.Location, n int) []time.Time {
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	out := make([]time.Time, 0, n)
	for i := n - 1; i >= 0; i-- {
		out = append(out, today.AddDate(0, 0, -i))
	}
	return out
}

// opDayCounts returns per-(plant-local-)day counts of bin_uop_audit rows whose
// op is in the given set, over the trailing n days. tz is an IANA name; the
// AT TIME ZONE conversion buckets each row's day in the plant zone.
func opDayCounts(db *sql.DB, tz string, n int, ops ...string) map[string]int64 {
	if len(ops) == 0 {
		return map[string]int64{}
	}
	ph := make([]string, len(ops))
	args := []any{n, tz} // $1 = days, $2 = tz; ops follow at $3…
	for i, op := range ops {
		ph[i] = fmt.Sprintf("$%d", i+3)
		args = append(args, op)
	}
	// Bucket the day as a 'YYYY-MM-DD' string in SQL (not ::date) so the map key
	// matches the Go-built axis verbatim. Scanning ::date into time.Time and
	// reformatting let pgx's date→time.Time conversion shift the calendar day,
	// which silently dropped counts (loaded read 0 with real set_for_production
	// rows present).
	q := fmt.Sprintf(`SELECT to_char(date_trunc('day', applied_at AT TIME ZONE $2), 'YYYY-MM-DD') AS day, COUNT(*)
		FROM bin_uop_audit
		WHERE applied_at >= NOW() - make_interval(days => $1)
		  AND op IN (%s)
		GROUP BY 1`, strings.Join(ph, ", "))
	return dayCounts(db, q, args...)
}

func dayCounts(db *sql.DB, query string, args ...any) map[string]int64 {
	m := make(map[string]int64)
	rows, err := db.Query(query, args...)
	if err != nil {
		// Surface the error instead of silently returning an empty map — a
		// swallowed param-type error here once made the whole velocity chart
		// read zero with real data present.
		log.Printf("footprint: load/unload day-count query: %v", err)
		return m
	}
	defer rows.Close()
	for rows.Next() {
		var day string // 'YYYY-MM-DD', already plant-tz-bucketed in SQL
		var c int64
		if err := rows.Scan(&day, &c); err != nil {
			return m
		}
		m[day] = c
	}
	return m
}
