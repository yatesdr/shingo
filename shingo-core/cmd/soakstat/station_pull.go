package main

import (
	"bufio"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// STATION PULL: what each station drew and made, against what it should have.
//
// A sim's work is pulled by its stations: a cell consumes its supply a unit per
// cycle, a press produces into its bins a unit per stroke. A station that was
// starved or blocked shows as one that drew or made fewer units than it should.
// That is how packed's loss on b6880b5d was found (one weld cell stopped, every
// other station unchanged), and it took a hand-written query over the dump.
//
// UNITS, NOT ORDERS. A station's orders are its swaps, and a swap lasts as long
// as the bin it delivered: a cell fed fuller bins asks less often for the same
// draw. Counted in orders, two healthy packed runs (2026-10-08) read ALN_006 at
// 77% of its pull while it consumed 105% of the baseline's units, so an order
// count says nothing about whether a station was ever short. The unit ledger
// (bin_uop_ledger's consume_tick and produce_tick rows, stamped with the node)
// is the station's own count of what it did.
//
// THE "SHOULD" IS OBSERVED, NOT MODELLED. simcalc counts swaps per station, and
// the Edge's choreographies turn a swap into one, two or a combined pair of
// orders, so its per-station figure misses the sim by 0.3x to 2.5x. Until it
// models the choreographies, the baseline is a certified run's own rates
// (-pull-out writes them), and a run is read against it (-pull). Without a
// baseline the section still prints each station's units and rate.
//
// A station is a node with ledger ticks, with its kind: a node that both draws
// and makes is two rows. Bins ticked while riding a robot (the _TRANSIT node)
// belong to no station and are left out.
const measurePull = "[S.1] STATION PULL"

// pullBelow is the fraction of its baseline a station may fall to before it is
// called below its pull: wider than one run's spread at a station (two healthy
// packed runs read every station within 10% of a third), narrower than a
// station that stopped (the wedged weld cell read 49%).
//
// The shortfall must also exceed pullNoise standard deviations of a Poisson
// count at the expected number of units, so a station that makes a handful of
// units is not called short on one bin's difference.
const (
	pullBelow = 0.85
	pullNoise = 2.0
)

type stationPull struct {
	station, kind string // kind: "consume" or "produce"
	units         int
	perPlantHour  float64
	hours         float64 // the run's span in plant-hours
	baseline      float64 // 0 when no baseline names it
}

func (s stationPull) key() string { return s.station + "|" + s.kind }

// below reports whether the station fell short of its baseline by more than
// the fraction and more than the noise.
func (s stationPull) below() bool {
	if s.baseline <= 0 || s.perPlantHour >= pullBelow*s.baseline {
		return false
	}
	expected := s.baseline * s.hours
	return expected-float64(s.units) > pullNoise*math.Sqrt(expected)
}

// stationPulls reads each station's units from the ledger, and the run's span
// in plant time: from the first top-level order's creation to the last history
// row. The ledger's own applied_at is the server's clock, which a sped-up sim
// does not move, so it cannot give plant-hours.
func stationPulls(db *sql.DB) ([]stationPull, float64, error) {
	var first, last sql.NullTime
	if err := db.QueryRow(`SELECT (SELECT MIN(created_at) FROM orders WHERE parent_order_id IS NULL),
	                              (SELECT MAX(created_at) FROM order_history)`).Scan(&first, &last); err != nil {
		return nil, 0, err
	}
	if !first.Valid || !last.Valid || !last.Time.After(first.Time) {
		return nil, 0, nil
	}
	hours := last.Time.Sub(first.Time).Hours()
	rows, err := db.Query(`
		SELECT n.name, CASE l.reason WHEN 'consume_tick' THEN 'consume' ELSE 'produce' END,
		       SUM(ABS(l.after_uop - l.before_uop))
		FROM bin_uop_ledger l JOIN nodes n ON n.id = l.node_id
		WHERE l.reason IN ('consume_tick', 'produce_tick') AND n.name <> '_TRANSIT'
		GROUP BY 1, 2 ORDER BY 1, 2`)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []stationPull
	for rows.Next() {
		var p stationPull
		if err := rows.Scan(&p.station, &p.kind, &p.units); err != nil {
			return nil, 0, err
		}
		p.perPlantHour = float64(p.units) / hours
		p.hours = hours
		out = append(out, p)
	}
	return out, hours, rows.Err()
}

// readPullBaseline reads a baseline written by -pull-out: one station per line,
// "station|kind<TAB>units-per-plant-hour". Blank lines and # comments skip.
func readPullBaseline(path string) (map[string]float64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string]float64{}
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "\t")
		rate, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
		if !ok || perr != nil || !strings.Contains(k, "|") {
			return nil, fmt.Errorf("%s:%d: want \"station|kind<TAB>rate\", got %q", path, n, line)
		}
		out[k] = rate
	}
	return out, sc.Err()
}

// writePullBaseline writes the run's rates as a baseline for -pull.
func writePullBaseline(path string, pulls []stationPull, hours float64) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# soakstat station pull baseline: units per plant-hour over %.1f plant-hours, written %s\n",
		hours, time.Now().UTC().Format(time.RFC3339))
	for _, p := range pulls {
		fmt.Fprintf(&b, "%s\t%.2f\n", p.key(), p.perPlantHour)
	}
	return os.WriteFile(path, []byte(b.String()), 0o644)
}

// applyPullBaseline sets each station's baseline and returns the stations the
// baseline names that this run never ticked at (a station that never drew or
// made a unit is the most below its pull a station can be).
func applyPullBaseline(pulls []stationPull, base map[string]float64) ([]stationPull, []string) {
	seen := map[string]bool{}
	for i := range pulls {
		pulls[i].baseline = base[pulls[i].key()]
		seen[pulls[i].key()] = true
	}
	var silent []string
	for k := range base {
		if !seen[k] && base[k] > 0 {
			silent = append(silent, k)
		}
	}
	sort.Strings(silent)
	return pulls, silent
}

// pullFindings is the stations below their pull, one line each.
func pullFindings(pulls []stationPull, silent []string) []string {
	var out []string
	for _, p := range pulls {
		if p.below() {
			out = append(out, fmt.Sprintf("BELOW PULL: %s %s %.1f units/plant-h, baseline %.1f (%.0f%%)",
				p.station, p.kind, p.perPlantHour, p.baseline, 100*p.perPlantHour/p.baseline))
		}
	}
	for _, k := range silent {
		out = append(out, fmt.Sprintf("BELOW PULL: %s ticked no unit at all (baseline names it)", strings.ReplaceAll(k, "|", " ")))
	}
	return out
}

// printPull prints the section.
func printPull(pulls []stationPull, hours float64, findings []string, baselined bool) {
	fmt.Printf("\n%s  %d station(s) over %.1f plant-hours", measurePull, len(pulls), hours)
	if !baselined {
		fmt.Printf("  (no -pull baseline: counts only)")
	}
	fmt.Println()
	fmt.Printf("        %-12s %-8s %8s %12s %10s\n", "station", "kind", "units", "per plant-h", "baseline")
	for _, p := range pulls {
		base := "-"
		if p.baseline > 0 {
			base = fmt.Sprintf("%.1f", p.baseline)
		}
		mark := ""
		if p.below() {
			mark = "   <- below its pull"
		}
		fmt.Printf("        %-12s %-8s %8d %12.1f %10s%s\n", p.station, p.kind, p.units, p.perPlantHour, base, mark)
	}
	for _, f := range findings {
		fmt.Printf("        %s\n", f)
	}
}

// printPullSection prints the section, or that it failed to run. The findings
// print with the run's violations, not twice.
func (r *report) printPullSection() {
	if r.unrunMeasures[measurePull] {
		fmt.Printf("\n%s  FAILED-TO-RUN\n", measurePull)
		return
	}
	if len(r.pulls) > 0 {
		printPull(r.pulls, r.pullHours, nil, r.pullBaselined)
	}
}
