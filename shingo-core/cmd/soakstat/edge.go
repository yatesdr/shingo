package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite" // pure-Go SQLite driver (CGO-free), the Edge's own

	"shingo/protocol"
)

// [S.5] EDGE STARVATION: the Edge half of the starvation watch.
//
// [S.1] counts what each station drew; it cannot say how long a station sat
// with nothing to draw, because Core never sees a seat wait. The Edge does,
// and keeps almost none of it: its tables are end state (the seat's carrier,
// count and pending ticks NOW). The one history it keeps is its OUTBOX: every
// 60 s (wall) the lineside reporter enqueues a LinesideLevelReport carrying,
// per consume seat, whether a carrier is bound, its count, and the active pile
// of the carrier's part; sent rows are purged 24 h (wall) after sending. So
// starvation is read off those reports:
//
//	A seat is STARVED from one report to the next when the report shows it
//	with no carrier, or a carrier at 0 or below, AND its pile at 0.
//
// TIME IS THE ENVELOPE'S ts, the Edge's clock.Now(): sim time when the Edge
// runs the sim clock. Each Edge prints its span on that clock beside the span
// on reported_at (wall, time.Now()), so the ratio says which clock it was. An
// interval longer than edgeGapFactor times the median is a gap (the Edge or
// its link was down; an unsent report is superseded by the next) and is
// counted as neither starved nor fed.
//
// WHAT IT CANNOT SEE, each a reason the number is a floor or a ceiling:
//   - Resolution is one report (60 wall-s, ~5 sim-min on a 5x sim): a seat
//     starved for less than an interval can read 0 or a whole interval.
//   - The pile in the report is the pile of the BOUND carrier's part. An empty
//     seat reports no part, so a pile of the claimed part next to an empty seat
//     reads 0 and the seat reads starved when the cell could still draw.
//   - A seat whose carrier nobody identified ships no row; it is "unknown" for
//     that interval, not starved.
//   - Ticks held and replayed (pending_uop_delta) leave no history at all:
//     applyHoldAndReplay logs only its errors. [S.4] g prints what is held at
//     the collect.
//
// -edge-db opens each Edge SQLite read-only (mode=ro, query_only): an
// instrument does not write to what it measures, the same rule as Core's open.
const (
	measureEdgeStarve = "[S.5] EDGE STARVATION"
	edgeGapFactor     = 3.0
)

var (
	edgeNotFoundRe = regexp.MustCompile(`uop_adjustment: process node "[^"]*" not found`)
	edgeBoundRe    = regexp.MustCompile(`uop_adjustment: bound bin \d+ to node `)
)

// edgeSource is one Edge's database, and its log when one was found.
type edgeSource struct {
	label, dbPath, logPath string
}

// edgeDBFlag is -edge-db: repeatable, and each value may be a comma list.
type edgeDBFlag []string

func (f *edgeDBFlag) String() string { return strings.Join(*f, ",") }

func (f *edgeDBFlag) Set(v string) error {
	for _, p := range strings.Split(v, ",") {
		if p = strings.TrimSpace(p); p != "" {
			*f = append(*f, p)
		}
	}
	return nil
}

// edgeSources is every -edge-db path, then every <dir>/<svc>/shingoedge.db
// under -edge-dir with its <svc>/edge.log when present.
func edgeSources(dbs []string, dir string) ([]edgeSource, error) {
	var out []edgeSource
	for _, p := range dbs {
		out = append(out, edgeSource{label: filepath.Base(filepath.Dir(p)), dbPath: p})
	}
	if dir == "" {
		return out, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("edge-dir: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		db := filepath.Join(dir, e.Name(), "shingoedge.db")
		if _, err := os.Stat(db); err != nil {
			continue
		}
		s := edgeSource{label: e.Name(), dbPath: db}
		if lp := filepath.Join(dir, e.Name(), "edge.log"); fileExists(lp) {
			s.logPath = lp
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("edge-dir %s: no <svc>/shingoedge.db under it", dir)
	}
	return out, nil
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// openEdgeReadOnly opens an Edge SQLite read-only. A URI so the mode reaches
// SQLite itself, and query_only so even a driver that ignored the mode would
// refuse a write.
func openEdgeReadOnly(path string) (*sql.DB, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	if !fileExists(abs) {
		return nil, fmt.Errorf("%s: no such file", abs)
	}
	p := filepath.ToSlash(abs)
	if !strings.HasPrefix(p, "/") {
		p = "/" + p // a drive letter: file:///C:/...
	}
	u := url.URL{Scheme: "file", Path: p,
		RawQuery: "mode=ro&_pragma=query_only(1)&_pragma=busy_timeout(5000)"}
	return sql.Open("sqlite", u.String())
}

// seatStarve is one consume seat's run, on the Edge's report clock.
type seatStarve struct {
	node                       string
	starved, observed, unknown time.Duration
}

// edgeReading is what one Edge's database (and log) says.
type edgeReading struct {
	src      edgeSource
	station  string
	owned    map[string]bool // core node names it has a process node for
	pending  []nodeCount     // pending_uop_delta held at the collect
	seats    []seatStarve
	reports  int
	simSpan  time.Duration // first to last report on the envelope ts
	wallSpan time.Duration // the same on reported_at
	gaps     int

	logRead               bool
	logNotFound, logBound int

	work *edgeWork // [S.6], edge_work.go
}

// linesideSample is one decoded lineside report.
type linesideSample struct {
	at, wall time.Time
	station  string
	entries  []protocol.LinesideLevelEntry
}

// readEdge reads one Edge.
func readEdge(src edgeSource) (*edgeReading, error) {
	db, err := openEdgeReadOnly(src.dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	e := &edgeReading{src: src, station: src.label, owned: map[string]bool{}}
	if err := e.readOwned(db); err != nil {
		return nil, fmt.Errorf("%s process nodes: %w", src.dbPath, err)
	}
	if err := e.readPending(db); err != nil {
		return nil, fmt.Errorf("%s pending ticks: %w", src.dbPath, err)
	}
	samples, err := linesideSamples(db)
	if err != nil {
		return nil, fmt.Errorf("%s lineside reports: %w", src.dbPath, err)
	}
	if n := len(samples); n > 0 {
		if samples[n-1].station != "" {
			e.station = samples[n-1].station
		}
		e.reports = n
		e.simSpan = samples[n-1].at.Sub(samples[0].at)
		e.wallSpan = samples[n-1].wall.Sub(samples[0].wall)
	}
	e.seats, e.gaps = starvation(samples)
	var logBytes []byte
	if src.logPath != "" {
		b, err := os.ReadFile(src.logPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", src.logPath, err)
		}
		e.logRead = true
		e.logNotFound = len(edgeNotFoundRe.FindAll(b, -1))
		e.logBound = len(edgeBoundRe.FindAll(b, -1))
		logBytes = b
	}
	if e.work, err = readEdgeWork(src.dbPath, logBytes); err != nil {
		return nil, fmt.Errorf("%s edge work: %w", src.dbPath, err)
	}
	// A database without the table prints "not read" in [S.6] rather than
	// failing the Edge's other sections.
	if n, err := readPLCTicks(db); err == nil {
		e.work.ticks, e.work.ticksRead = n, true
	}
	return e, nil
}

// readOwned is every core node the Edge has a process node for, retired ones
// included: the adjustment handler's lookup (GetNodeByCoreNodeName) finds a
// retired node too, so it is not "not found".
func (e *edgeReading) readOwned(db *sql.DB) error {
	rows, err := db.Query(`SELECT DISTINCT core_node_name FROM process_nodes WHERE core_node_name <> ''`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return err
		}
		e.owned[n] = true
	}
	return rows.Err()
}

func (e *edgeReading) readPending(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT pn.core_node_name, r.pending_uop_delta
		FROM process_node_runtime_states r JOIN process_nodes pn ON pn.id = r.process_node_id
		WHERE r.pending_uop_delta <> 0 ORDER BY 2 DESC, 1`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var c nodeCount
		if err := rows.Scan(&c.node, &c.n); err != nil {
			return err
		}
		e.pending = append(e.pending, c)
	}
	return rows.Err()
}

// linesideSamples decodes the Edge outbox's lineside reports, oldest first. A
// row that does not decode is skipped: the outbox is the Edge's, not a format
// this tool owns.
func linesideSamples(db *sql.DB) ([]linesideSample, error) {
	rows, err := db.Query(`SELECT payload FROM outbox WHERE msg_type = ? ORDER BY id`,
		protocol.SubjectLinesideLevelReport)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []linesideSample
	for rows.Next() {
		var payload []byte
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		if s, ok := decodeLinesideReport(payload); ok {
			out = append(out, s)
		}
	}
	return out, rows.Err()
}

func decodeLinesideReport(payload []byte) (linesideSample, bool) {
	var env protocol.Envelope
	var data protocol.Data
	var rep protocol.LinesideLevelReport
	if json.Unmarshal(payload, &env) != nil || json.Unmarshal(env.Payload, &data) != nil ||
		json.Unmarshal(data.Body, &rep) != nil || env.Timestamp.IsZero() {
		return linesideSample{}, false
	}
	return linesideSample{at: env.Timestamp, wall: rep.ReportedAt, station: rep.Station, entries: rep.Entries}, true
}

// starved is the seat's state in one report.
func seatStarved(e protocol.LinesideLevelEntry) bool {
	return (e.BinCount == 0 || e.BinUOP <= 0) && e.BucketQty <= 0
}

// starvation folds the reports into each seat's starved, observed and unknown
// time. Each report's state holds until the next; an interval past
// edgeGapFactor times the median is a gap, counted for no seat. Returns the
// seats by name and the gaps skipped.
func starvation(samples []linesideSample) ([]seatStarve, int) {
	if len(samples) < 2 {
		return nil, 0
	}
	steps := make([]time.Duration, 0, len(samples)-1)
	for i := 1; i < len(samples); i++ {
		steps = append(steps, samples[i].at.Sub(samples[i-1].at))
	}
	sorted := append([]time.Duration{}, steps...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	limit := time.Duration(edgeGapFactor * float64(sorted[len(sorted)/2]))

	seen := map[string]bool{}
	for _, s := range samples {
		for _, e := range s.entries {
			seen[e.CoreNodeName] = true
		}
	}
	acc := map[string]*seatStarve{}
	for n := range seen {
		acc[n] = &seatStarve{node: n}
	}
	gaps := 0
	for i, dt := range steps {
		if dt <= 0 {
			continue
		}
		if dt > limit {
			gaps++
			continue
		}
		in := map[string]bool{}
		for _, e := range samples[i].entries {
			in[e.CoreNodeName] = true
			a := acc[e.CoreNodeName]
			a.observed += dt
			if seatStarved(e) {
				a.starved += dt
			}
		}
		for n, a := range acc {
			if !in[n] {
				a.unknown += dt
			}
		}
	}
	out := make([]seatStarve, 0, len(acc))
	for _, a := range acc {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].starved != out[j].starved {
			return out[i].starved > out[j].starved
		}
		return out[i].node < out[j].node
	})
	return out, gaps
}

// readEdges reads every source; one that fails is a failed-to-run line and a
// nil reading.
func readEdges(sources []edgeSource, u *unrun) []*edgeReading {
	out := make([]*edgeReading, 0, len(sources))
	for _, s := range sources {
		e, err := readEdge(s)
		if err != nil {
			u.fail(measureEdgeStarve, err)
		}
		out = append(out, e)
	}
	return out
}

// printEdgeStarvation writes the section, each seat beside the units [S.1]
// counted it consuming.
func printEdgeStarvation(edges []*edgeReading, pulls []stationPull, unrun bool) {
	if len(edges) == 0 {
		return
	}
	fmt.Printf("\n%s", measureEdgeStarve)
	if unrun {
		fmt.Print("  FAILED-TO-RUN (for the Edge(s) not listed)")
	}
	fmt.Println("  per consume seat, from each Edge's lineside reports (time: the envelope's clock)")
	drew := map[string]int{}
	for _, p := range pulls {
		if p.kind == "consume" {
			drew[p.station] = p.units
		}
	}
	for _, e := range edges {
		if e == nil {
			continue
		}
		fmt.Printf("        %s (%s): %d report(s) over %.1f min on ts, %.1f min on reported_at (wall); %d gap(s) skipped\n",
			e.station, e.src.label, e.reports, e.simSpan.Minutes(), e.wallSpan.Minutes(), e.gaps)
		if len(e.seats) == 0 {
			fmt.Println("          no lineside report history (fewer than two reports in the outbox)")
			continue
		}
		fmt.Printf("          %-12s %12s %12s %6s %12s %12s\n", "seat", "starved(min)", "observed", "%", "unknown(min)", "drew [S.1]")
		for _, s := range e.seats {
			pct := 0.0
			if s.observed > 0 {
				pct = 100 * s.starved.Minutes() / s.observed.Minutes()
			}
			units := "-"
			if n, ok := drew[s.node]; ok {
				units = fmt.Sprint(n)
			}
			fmt.Printf("          %-12s %12.1f %12.1f %5.0f%% %12.1f %12s\n",
				s.node, s.starved.Minutes(), s.observed.Minutes(), pct, s.unknown.Minutes(), units)
		}
	}
}
