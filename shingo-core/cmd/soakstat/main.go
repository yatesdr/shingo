// Command soakstat reads the [M] measures off a running or finished soak.
//
// It is RIG CODE, not production code: it lives under cmd/ with the other
// operator tools, it only ever reads, and nothing in the engine imports it. The
// campaign's standing machinery is deliberately small — one query pass and one
// summary line — because the battery is driven by hand and only the soak needs a
// machine.
//
// WHY A TOOL RATHER THAN A SET OF QUERIES IN A DOC. The measures are the product
// of the soak; the run is worthless if nobody can read it. A doc full of SQL gets
// pasted wrong at 3am, and half of these need a join the operator has to get
// right (a lane's mark lives in node_properties, a leg's depth lives two joins
// away from the order). Getting them wrong quietly is the failure mode that makes
// a green soak meaningless.
//
// WHAT IT CANNOT SEE. Two measures live only in Core's log, because the events
// they count are logged and never written to a table: the burial shadow's soft
// and hard tallies, and the dig steal. Pass -log to fold them in; without it
// those lines read `n/a (no -log)` rather than zero, because a zero nobody
// measured is the most dangerous number in a soak report.
package main

import (
	"database/sql"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/config"
	"shingocore/dispatch"
	"shingocore/store"
)

func main() {
	configPath := flag.String("config", "/etc/shingo/shingocore.dev.yaml", "path to the core config YAML")
	logSource := flag.String("log", "", "core log: a file path, `-` for stdin (the reliable form), or docker:<container>")
	oneline := flag.Bool("oneline", false, "print the single-line summary and nothing else")
	flag.Parse()

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	db, err := store.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("open core db: %v", err)
	}
	defer db.Close()

	r := collect(db, *logSource)
	if *oneline {
		fmt.Println(r.summary())
		return
	}
	r.report()
	fmt.Println()
	fmt.Println(r.summary())
	if len(r.violations) > 0 {
		os.Exit(1)
	}
}

// report is every measure the soak collects, plus the invariant violations that
// decide its exit code.
type report struct {
	orders     counts
	digs       digStats
	lanes      []laneShape
	gated      map[bool]flowStats // true = the lane carries a mark
	dissolves  int
	depthCost  []depthBucket
	logCounts  *logStats // nil when -log was not given
	violations []string
	robotReuse reuseStats
	waitCauses []causeCount
	dwell      dwellStats
}

type counts struct{ total, completed, failed, cancelled, inFlight, queued int }

type digStats struct {
	parents      int
	legs         int
	byRobotCount map[int]int // distinct robots used -> number of digs
	maxLegs      int
}

type reuseStats struct{ consecutivePairs, sameRobot, resumeSameRobot, resumes int }

type laneShape struct {
	group, lane string
	marked      bool
	depth       int
	occupied    int
	deepestFull int
}

type flowStats struct {
	completed int
	waits     int
	avgCycleS float64
}

type depthBucket struct {
	depth     int
	n         int
	avgCycleS float64
}

type causeCount struct {
	cause string
	n     int
}

// dwellStats is the OUTBOUND DWELL's watched measure: how long dig legs stand in
// the lanes they are digging, holding a blocker, while Core chooses where it goes.
//
// ── WHY IT IS WATCHED RATHER THAN CAPPED ──────────────────────────────────
//
// Owner ruling (§R.71): no per-node dweller counting is built. Capacity can be
// inferred from LOCKING — a dwelling dig's lane is visibly locked, the oracle's
// group view is honest at the granularity that governs choreography, and Core is
// the single chooser at release time deciding against fresh state. Building a
// counter for a problem that is waiting is a mechanism that absorbs.
//
// What replaces it is this number and a trend. A dwell is SUPPOSED to be short:
// in the ordinary case the destination is open and the robot barely pauses. A
// dwell time that balloons means legs are standing loaded while the group has
// nowhere to put anything, and that is the signal for the oracle to stop starting
// digs into a lock-saturated group — a day-2 decision this measure exists to
// inform rather than to pre-empt.
//
// `open` is the half that cannot be read from a completed window: legs dwelling
// RIGHT NOW, counted with their age against the sample time. A soak that ends
// with open dwellers and a large max is a different report from one whose dwells
// all closed.
type dwellStats struct {
	n          int     // dig legs that have dwelled at all in this window
	open       int     // of those, how many are still standing
	avgS, maxS float64 // seconds, open dwells measured against now
}

type logStats struct {
	source                       string
	softN, hardN, churnN, stealN int
}

func collect(db *store.DB, logSource string) *report {
	r := &report{gated: map[bool]flowStats{}}
	r.orders = orderCounts(db)
	r.digs = digDistribution(db)
	r.robotReuse = robotReuse(db)
	r.lanes = laneShapes(db)
	r.gated = gatedVsUngated(db)
	// EVERY way a chapter ends, not just the one. This counted
	// ReshuffleDissolveDetail alone and so under-reported a chapter that ended by
	// a leg failing — which is the other half of the same measure, and the number
	// a soak reads to decide whether the dissolve arm is firing at all
	// (§R.98 stage D).
	r.dissolves = scalar(db, chapterEndCancelCountQuery(), chapterEndCancelArgs()...)
	r.depthCost = depthCost(db)
	r.waitCauses = waitCauses(db)
	r.dwell = dwellDuration(db)
	if logSource != "" {
		r.logCounts = readLog(logSource)
	}
	r.violations = checkInvariants(db)
	return r
}

// ── the measures ──────────────────────────────────────────────────────────────

// SUCCESS IS `confirmed`, NOT `completed`. There is no `completed` status in this
// system, and the first draft of this tool asked for one — every measure read
// zero against a rig that was demonstrably working, which is the exact failure
// this file's header calls the most dangerous number in a soak report. The
// status vocabulary is protocol/status.go and the SQL forms are generated from
// the enum there, so they are used rather than retyped.
func orderCounts(db *store.DB) counts {
	var c counts
	c.total = scalar(db, `SELECT COUNT(*) FROM orders`)
	c.completed = scalar(db, `SELECT COUNT(*) FROM orders WHERE status = $1`, string(protocol.StatusConfirmed))
	c.failed = scalar(db, `SELECT COUNT(*) FROM orders WHERE status = $1`, string(protocol.StatusFailed))
	c.cancelled = scalar(db, `SELECT COUNT(*) FROM orders WHERE status = $1`, string(protocol.StatusCancelled))
	// Pre-dispatch, from the predicate rather than typed out: {pending, queued,
	// sourcing} is IsPreDispatch, and a status joining that family must join this
	// count with it.
	c.queued = scalar(db, fmt.Sprintf(
		`SELECT COUNT(*) FROM orders WHERE status IN (%s)`, protocol.PreDispatchStatusSQLList()))
	c.inFlight = scalar(db, fmt.Sprintf(
		`SELECT COUNT(*) FROM orders WHERE status NOT IN (%s) AND status NOT IN (%s)`,
		protocol.TerminalStatusSQLList(), protocol.PreDispatchStatusSQLList()))
	return c
}

// stallPopulation is one PROGRESS KIND the stall checker watches: which orders
// belong to it, how long one of them may sit before it is worth a line, and the
// words for the report.
//
// The predicate is the definition and the SQL is rendered FROM it, so the set
// the query returns and the set the drift test reasons about cannot come apart.
// Writing the clause out by hand is what let `pending` and `sourcing` fall
// through every kind — the bug the derivation exists to make impossible.
type stallPopulation struct {
	label string
	after string // a Postgres interval literal
	match func(protocol.Status) bool
}

// clause renders the population as a SQL predicate over `status`.
//
// A kind whose predicate matches no status renders `FALSE` rather than an empty
// `IN ()`, which is a syntax error. That case is a definition mistake, not a
// runtime one — the partition test catches it — so this only has to fail
// harmlessly rather than diagnose.
func (p stallPopulation) clause() string {
	list := protocol.StatusSQLList(p.match)
	if list == "" {
		return "FALSE"
	}
	return fmt.Sprintf("status IN (%s)", list)
}

// stallPopulations PARTITIONS the non-terminal statuses. Every one of them is in
// exactly one kind, which is what makes "nothing is stalled" a statement about
// the whole plant rather than about the statuses somebody remembered.
//
// The three kinds are progress shapes, not lifecycle stages:
//
//   - DWELLING AT A MARK is a robot committed and standing still. Core owes it a
//     decision, so the threshold is the length of somebody else's lane transit.
//   - PARKED is pre-dispatch: no robot is committed, the order is waiting for
//     material, a slot, a lane or the fleet. Long waits here are legitimate.
//     IsPreDispatch is exactly this set — {pending, sourcing, queued} — and using
//     the predicate rather than naming `queued` is the fix.
//   - IN FLIGHT is everything else non-terminal: handed over, moving, or waiting
//     on a human (delivered), plus the compound parent's own `reshuffling`.
var stallPopulations = []stallPopulation{
	{
		label: "dwelling at a mark",
		after: "90 seconds",
		match: func(s protocol.Status) bool { return s == protocol.StatusStaged },
	},
	{
		label: "parked",
		after: "15 minutes",
		match: protocol.IsPreDispatch,
	},
	{
		label: "in flight",
		after: "20 minutes",
		match: func(s protocol.Status) bool {
			return !protocol.IsTerminal(s) && !protocol.IsPreDispatch(s) && s != protocol.StatusStaged
		},
	},
}

// digDistribution answers catalog 3.8: how many robots a dig actually costs.
// Keyed on the PARENT, counting distinct non-empty robot ids across its legs —
// a leg that never reached the fleet has no robot and must not count as one.
func digDistribution(db *store.DB) digStats {
	d := digStats{byRobotCount: map[int]int{}}
	rows, err := db.DB.Query(`
		SELECT parent_order_id,
		       COUNT(*)                                              AS legs,
		       COUNT(DISTINCT NULLIF(robot_id, ''))                  AS robots
		FROM orders
		WHERE parent_order_id IS NOT NULL
		GROUP BY parent_order_id`)
	if err != nil {
		return d
	}
	defer rows.Close()
	for rows.Next() {
		var parent int64
		var legs, robots int
		if err := rows.Scan(&parent, &legs, &robots); err != nil {
			continue
		}
		d.parents++
		d.legs += legs
		d.byRobotCount[robots]++
		if legs > d.maxLegs {
			d.maxLegs = legs
		}
	}
	return d
}

// robotReuse answers catalog 8.8: does the SAME robot take leg N+1 after
// finishing leg N, and does the resumed parent's own retrieve go to the robot
// that cleared the last blocker.
//
// The caveat from the catalog is worth repeating at the point of measurement:
// SIM ASSIGNMENT IS NOT RDS ASSIGNMENT. This does not measure whether the vendor
// prefers the nearby robot. It measures whether OUR timing leaves that robot free
// and nearest at the moment the next create fires — the half Core controls.
func robotReuse(db *store.DB) reuseStats {
	var s reuseStats
	rows, err := db.DB.Query(`
		SELECT parent_order_id, robot_id
		FROM orders
		WHERE parent_order_id IS NOT NULL AND robot_id <> ''
		ORDER BY parent_order_id, sequence`)
	if err != nil {
		return s
	}
	defer rows.Close()
	var lastParent sql.NullInt64
	var lastRobot string
	for rows.Next() {
		var parent int64
		var robot string
		if err := rows.Scan(&parent, &robot); err != nil {
			continue
		}
		if lastParent.Valid && lastParent.Int64 == parent {
			s.consecutivePairs++
			if robot == lastRobot {
				s.sameRobot++
			}
		}
		lastParent = sql.NullInt64{Int64: parent, Valid: true}
		lastRobot = robot
	}
	// The resume leg: the parent's own robot against its LAST child's robot.
	rows2, err := db.DB.Query(`
		SELECT p.robot_id, c.robot_id
		FROM orders p
		JOIN LATERAL (
			SELECT robot_id FROM orders c
			WHERE c.parent_order_id = p.id AND c.robot_id <> ''
			ORDER BY c.sequence DESC LIMIT 1
		) c ON TRUE
		WHERE p.robot_id <> ''`)
	if err != nil {
		return s
	}
	defer rows2.Close()
	for rows2.Next() {
		var parentRobot, lastChild string
		if err := rows2.Scan(&parentRobot, &lastChild); err != nil {
			continue
		}
		s.resumes++
		if parentRobot == lastChild {
			s.resumeSameRobot++
		}
	}
	return s
}

// laneShapes answers catalog 8.1: where blockers lie after hours of digging.
// deepestFull is the drift signal — if lanes are being emptied mouth-first and
// refilled deepest-first as designed, occupancy should stay contiguous from the
// back; a lane holding one bin at depth 2 with depth 1 empty is an air bubble.
func laneShapes(db *store.DB) []laneShape {
	rows, err := db.DB.Query(`
		SELECT g.name                                            AS grp,
		       l.name                                            AS lane,
		       (p.value IS NOT NULL AND p.value <> '')            AS marked,
		       COUNT(s.id)                                        AS depth,
		       COUNT(b.id)                                        AS occupied,
		       COALESCE(MAX(s.depth) FILTER (WHERE b.id IS NOT NULL), 0) AS deepest_full
		FROM nodes l
		JOIN nodes g            ON g.id = l.parent_id
		LEFT JOIN nodes s       ON s.parent_id = l.id
		LEFT JOIN bins  b       ON b.node_id = s.id
		LEFT JOIN node_properties p ON p.node_id = l.id AND p.key = $1
		WHERE l.node_type_id = (SELECT id FROM node_types WHERE code = 'LANE')
		GROUP BY g.name, l.name, marked
		ORDER BY g.name, l.name`, dispatch.PropLaneGatePoint)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []laneShape
	for rows.Next() {
		var s laneShape
		if err := rows.Scan(&s.group, &s.lane, &s.marked, &s.depth, &s.occupied, &s.deepestFull); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out
}

// gatedVsUngated answers catalog 8.2 — the pitch for marks, in numbers.
//
// An order is attributed to a lane by its DELIVERY node for a store and its
// SOURCE node for a retrieve; either way the lane it contended for is the one
// whose slot it named. Orders touching no lane are excluded rather than counted
// as ungated, because they never asked the question.
func gatedVsUngated(db *store.DB) map[bool]flowStats {
	out := map[bool]flowStats{}
	rows, err := db.DB.Query(`
		WITH touched AS (
			SELECT o.id,
			       o.status,
			       o.queue_cause,
			       EXTRACT(EPOCH FROM (o.completed_at - o.created_at)) AS cycle_s,
			       (p.value IS NOT NULL AND p.value <> '')             AS marked
			FROM orders o
			JOIN nodes s  ON s.name = COALESCE(NULLIF(o.source_node, ''), o.delivery_node)
			JOIN nodes l  ON l.id = s.parent_id
			LEFT JOIN node_properties p ON p.node_id = l.id AND p.key = $1
			WHERE l.node_type_id = (SELECT id FROM node_types WHERE code = 'LANE')
		)
		SELECT marked,
		       COUNT(*) FILTER (WHERE status = $2)                   AS completed,
		       COUNT(*) FILTER (WHERE queue_cause IS NOT NULL
		                          AND queue_cause <> '')             AS waits,
		       COALESCE(AVG(cycle_s) FILTER (WHERE status = $2), 0)  AS avg_cycle
		FROM touched GROUP BY marked`, dispatch.PropLaneGatePoint, string(protocol.StatusConfirmed))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var marked bool
		var f flowStats
		if err := rows.Scan(&marked, &f.completed, &f.waits, &f.avgCycleS); err != nil {
			continue
		}
		out[marked] = f
	}
	return out
}

// dwellDuration measures how long dig legs stand loaded in the lane they are
// digging — §R.71's rider 2, and the number that decides whether the outbound
// dwell is a pause or a stall.
//
// ── THE MOMENTS IT READS, AND WHY THEY ARE THE RIGHT TWO ──────────────────
//
// A dwell starts when the robot reaches its wait — which is the order going
// `staged`, an order_history row — and ends at the next status change, which is
// the release driving it out. Both are written by the lifecycle rather than by
// anything this measure asked for, so the number costs no new writer and cannot
// drift from the thing it describes (law 4: the fact is stamped where it is made).
//
// THE LAST `staged` ROW, not the first. A leg digging a MARKED lane stages twice:
// once outside at the lane's mark waiting to be let IN, and once inside at the
// dwell waiting for a destination. They are different waits with different
// releasers, and the second is the one this measure is about — so DISTINCT ON
// takes the latest, which is the outbound one by construction (the dwell cannot
// precede the entry it follows).
//
// AN OPEN DWELL IS MEASURED AGAINST NOW, deliberately. A leg still standing when
// the sample is taken is exactly the population a ballooning trend is made of,
// and excluding it would make the average look best at the worst moment — the
// same premature-read error §R.12 cost this stream once already.
func dwellDuration(db *store.DB) dwellStats {
	var s dwellStats
	// The terminal set is RENDERED from the transition table rather than passed as
	// an array parameter: that is how every other status predicate in this tree is
	// written (protocol.TerminalStatusSQLList), and a second spelling of "which
	// statuses are terminal" is exactly what a status added later would break.
	err := db.DB.QueryRow(fmt.Sprintf(`
		WITH last_stage AS (
			SELECT DISTINCT ON (h.order_id) h.order_id, h.created_at AS started
			FROM order_history h
			JOIN orders o ON o.id = h.order_id
			WHERE h.status = $1 AND o.parent_order_id IS NOT NULL
			ORDER BY h.order_id, h.created_at DESC
		),
		dwell AS (
			SELECT ls.order_id,
			       ls.started,
			       (SELECT MIN(h2.created_at) FROM order_history h2
			         WHERE h2.order_id = ls.order_id
			           AND h2.created_at > ls.started
			           AND h2.status <> $1)                       AS ended,
			       (o.delivery_node = '' AND o.status NOT IN (%s)) AS still_open
			FROM last_stage ls
			JOIN orders o ON o.id = ls.order_id
		)
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE still_open),
		       COALESCE(AVG(EXTRACT(EPOCH FROM (COALESCE(ended, NOW()) - started))), 0),
		       COALESCE(MAX(EXTRACT(EPOCH FROM (COALESCE(ended, NOW()) - started))), 0)
		FROM dwell`, protocol.TerminalStatusSQLList()),
		string(protocol.StatusStaged)).
		Scan(&s.n, &s.open, &s.avgS, &s.maxS)
	if err != nil {
		return dwellStats{}
	}
	return s
}

// depthCost answers catalog 8.6: what depth costs, in seconds, before and after
// marks. Bucketed by the SOURCE slot's depth, so a retrieve out of slot 5 lands
// in bucket 5 whether or not it needed a dig.
func depthCost(db *store.DB) []depthBucket {
	rows, err := db.DB.Query(`
		SELECT s.depth,
		       COUNT(*),
		       AVG(EXTRACT(EPOCH FROM (o.completed_at - o.created_at)))
		FROM orders o
		JOIN nodes s ON s.name = o.source_node
		WHERE o.status = $1 AND o.completed_at IS NOT NULL AND s.depth IS NOT NULL
		GROUP BY s.depth ORDER BY s.depth`, string(protocol.StatusConfirmed))
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []depthBucket
	for rows.Next() {
		var b depthBucket
		if err := rows.Scan(&b.depth, &b.n, &b.avgCycleS); err != nil {
			continue
		}
		out = append(out, b)
	}
	return out
}

func waitCauses(db *store.DB) []causeCount {
	rows, err := db.DB.Query(`
		SELECT queue_cause, COUNT(*) FROM orders
		WHERE queue_cause IS NOT NULL AND queue_cause <> ''
		GROUP BY queue_cause ORDER BY 2 DESC`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []causeCount
	for rows.Next() {
		var c causeCount
		if err := rows.Scan(&c.cause, &c.n); err != nil {
			continue
		}
		out = append(out, c)
	}
	return out
}

// ── the invariants that decide the exit code ──────────────────────────────────
//
// These are the campaign doc's §4 list, expressed as queries. Each one is a
// statement the soak claims; a non-empty result means the claim is false, and a
// non-zero exit is how an unattended run says so without anyone reading it.
// invariantChecks is the flat registry checkInvariants runs, in report order.
// It matches the shape `collect` already uses for the measures half of this
// file: one named function per question, listed once.
//
// Named rather than inlined so each can be seeded and asserted on its own. The
// docker test currently drives the whole function and greps twelve checks'
// worth of output for the two it cares about.
var invariantChecks = []struct {
	name string
	run  func(*store.DB) []string
}{
	{"a terminal order carrying a congestion-shaped queue cause", checkTerminalWithCongestionCause},
	{"a lane with two occupancy owners", checkDoubleOccupancy},
	{"an entrant the lane never saw arrive", checkPhantomEntrants},
	{"a leg that reached the destination end unjudged", checkNotJudgedAtDestEnd},
	{"a leg judged with no bin to judge", checkNotJudgedNoBin},
	{"two legs traversing one lane at once", checkTraversalOverlap},
	{"a reservation still held by a terminal order", checkTerminalOrderReservations},
	{"an order queued five minutes with no cause on the row", checkQueuedWithoutCause},
	{"an order wearing armor the fleet never took", checkArmoredWithNoVendorOrder},
	{"how orders were freed by the periodic floor release", checkFloorReleaseHistogram},
	{"orders waiting under a cause nothing declares", checkUndeclaredWaits},
	{"orders that have not moved for their population's budget", checkStalledOrders},
	{"a negative total UOP across bins", checkNegativeTotalUOP},
}

func checkInvariants(db *store.DB) []string {
	var v []string
	for _, c := range invariantChecks {
		v = append(v, c.run(db)...)
	}
	return v
}

// ── the log-only measures ─────────────────────────────────────────────────────

func readLog(source string) *logStats {
	var body string
	switch {
	case source == "-":
		// STDIN, and this is the form that actually works against a container.
		// `-log docker:NAME` shells out to the docker CLI, which is not in the
		// image soakstat ships in — so from inside core it fails, which is
		// where you most want to run it. Piping in is the way:
		//
		//	docker logs shingo-dev-core-1 | \
		//	  docker exec -i shingo-dev-core-1 soakstat -log -
		b, err := io.ReadAll(os.Stdin)
		if err != nil {
			return &logStats{source: fmt.Sprintf("stdin read failed: %v", err)}
		}
		body = string(b)
	case strings.HasPrefix(source, "docker:"):
		out, err := exec.Command("docker", "logs", strings.TrimPrefix(source, "docker:")).CombinedOutput()
		if err != nil {
			return &logStats{source: fmt.Sprintf("docker logs failed: %v — try `docker logs X | soakstat -log -`", err)}
		}
		body = string(out)
	default:
		b, err := os.ReadFile(source)
		if err != nil {
			return &logStats{source: fmt.Sprintf("read failed: %v", err)}
		}
		body = string(b)
	}
	s := &logStats{source: source}
	for _, line := range strings.Split(body, "\n") {
		switch {
		case strings.Contains(line, "burial-shadow"):
			// THESE LINES ARE A RUNNING TALLY, NOT AN EVENT — and counting them as
			// events is a mistake this tool made and reported for an hour.
			//
			// logBurialShadow re-emits the same sentence on EVERY reconciliation
			// sweep for as long as the tally is non-zero, which is every two
			// seconds. Counting occurrences therefore measures UPTIME SINCE THE
			// FIRST BURIAL, not burials: one bypass read as 22, then 322, then
			// however long the run lasts. It made a single incident look like the
			// plant hemorrhaging, on the one number the summary line calls a
			// tripwire.
			//
			// So take the LAST value each line reports, not a count of them. The
			// two halves still mean opposite things: a soft hold buried is DATA
			// (re-planning was always going to be paid for), a hard claim buried is
			// a should-be-zero.
			// TWO HARD-BURIAL COUNTERS SINCE THE PLAN §R.4 SPLIT, and reading only
			// the first would silently under-report. BYPASS= is now the narrow
			// should-be-zero (the claim already existed when the placer was
			// committed, so the selector was never asked); CHURN= is the accepted
			// population the old BYPASS= number was mostly made of. Both lines are
			// re-emitted every sweep, so both take the LAST value rather than a
			// count of occurrences — see the note above.
			if n, ok := tallyValue(line, "BYPASS="); ok {
				s.hardN = n
			}
			if n, ok := tallyValue(line, "CHURN="); ok {
				s.churnN = n
			}
			if n, ok := tallyValue(line, "soft-hold burials "); ok {
				s.softN = n
			}
		case strings.Contains(line, "the dig always wins on a positional blocker"):
			// This one IS per-event — one line per steal, at the steal.
			s.stealN++
		}
	}
	return s
}

// ── output ────────────────────────────────────────────────────────────────────

func (r *report) report() {
	fmt.Println("SOAK MEASURES")
	fmt.Println(strings.Repeat("=", 78))

	c := r.orders
	fmt.Printf("\nORDERS  total %d · completed %d · failed %d · cancelled %d · in-flight %d · queued %d\n",
		c.total, c.completed, c.failed, c.cancelled, c.inFlight, c.queued)

	fmt.Printf("\n[3.8] ROBOTS PER DIG  %d digs, %d legs, deepest %d legs\n", r.digs.parents, r.digs.legs, r.digs.maxLegs)
	for n := 0; n <= 6; n++ {
		if k, ok := r.digs.byRobotCount[n]; ok {
			fmt.Printf("        %d robot(s): %d dig(s)%s\n", n, k, tailNote(n))
		}
	}

	fmt.Printf("\n[8.8] ROBOT REUSE ACROSS LEGS  ")
	if r.robotReuse.consecutivePairs == 0 {
		fmt.Println("no consecutive leg pairs yet")
	} else {
		fmt.Printf("%d/%d leg handoffs kept the same robot (%.0f%%)\n",
			r.robotReuse.sameRobot, r.robotReuse.consecutivePairs,
			100*float64(r.robotReuse.sameRobot)/float64(r.robotReuse.consecutivePairs))
	}
	if r.robotReuse.resumes > 0 {
		fmt.Printf("        %d/%d resumed parents reused the last blocker's robot\n",
			r.robotReuse.resumeSameRobot, r.robotReuse.resumes)
	}
	fmt.Println("        caveat: sim assignment is not RDS assignment — this measures whether")
	fmt.Println("        our timing INVITES chaining, not whether the vendor prefers it.")

	fmt.Printf("\n[8.2] GATED vs UNGATED\n")
	fmt.Printf("        %-9s %9s %9s %12s\n", "lane", "completed", "waits", "avg cycle s")
	for _, marked := range []bool{true, false} {
		f := r.gated[marked]
		fmt.Printf("        %-9s %9d %9d %12.1f\n", markedLabel(marked), f.completed, f.waits, f.avgCycleS)
	}

	fmt.Printf("\n[8.6] TIME TO COMPLETE BY SOURCE DEPTH\n")
	for _, b := range r.depthCost {
		fmt.Printf("        depth %d: n=%-5d avg %.1fs\n", b.depth, b.n, b.avgCycleS)
	}

	fmt.Printf("\n[8.1] LANE SHAPE  (air bubble = occupied>0 with the mouth empty)\n")
	fmt.Printf("        %-10s %-8s %-7s %6s %9s %10s\n", "group", "lane", "marked", "depth", "occupied", "deepest")
	for _, l := range r.lanes {
		fmt.Printf("        %-10s %-8s %-7s %6d %9d %10d\n",
			l.group, l.lane, yesNo(l.marked), l.depth, l.occupied, l.deepestFull)
	}

	fmt.Printf("\n[R.71] OUTBOUND DWELL  %d leg(s) dwelled · avg %.1fs · max %.1fs · %d still standing\n",
		r.dwell.n, r.dwell.avgS, r.dwell.maxS, r.dwell.open)
	fmt.Println("        A dwell is a dig leg standing in the lane it is digging, holding a blocker,")
	fmt.Println("        while Core chooses where it goes. EXPECTED FLAT and short: the ordinary case")
	fmt.Println("        is an open destination and a robot that barely pauses. A rising average or a")
	fmt.Println("        max that keeps growing means legs are standing loaded in a group with nowhere")
	fmt.Println("        to put anything — the signal for the oracle to stop starting digs into a")
	fmt.Println("        lock-saturated group (§R.71 rider 2). It is a WATCHED measure, not a gate:")
	fmt.Println("        no per-node dweller counting is built, by owner ruling.")

	fmt.Printf("\n[8.4] DISSOLVES  %d\n", r.dissolves)
	fmt.Println("        NOTE: the catalog expected ~0 'with settle-then-plan'. That mechanism")
	fmt.Println("        does not exist (see FINDINGS F-02) — the dissolve is the built answer,")
	fmt.Println("        so this number is the RATE OF THE RACE, not a defect count.")

	fmt.Printf("\n[7.5] SHADOW COUNTERS + [8.5] STEALS\n")
	if r.logCounts == nil {
		fmt.Println("        n/a (no -log). A zero nobody measured is worse than no number.")
	} else if r.logCounts.softN+r.logCounts.hardN+r.logCounts.churnN+r.logCounts.stealN == 0 && strings.Contains(r.logCounts.source, "failed") {
		fmt.Printf("        n/a — %s\n", r.logCounts.source)
	} else {
		fmt.Printf("        soft burials (data):     %d\n", r.logCounts.softN)
		// The hard count is TWO numbers since the PLAN R.4 split, and printing only
		// the tripwire would hide the population it was mostly made of. Bypass is
		// the should-be-zero (never-asked); churn is approved-then-invalidated,
		// which the design accepts and heals — non-zero there is the measured price
		// of law 6, not a finding.
		fmt.Printf("        hard burials (TRIPWIRE): %d   <- expected value is ZERO (never-asked)\n", r.logCounts.hardN)
		fmt.Printf("        approved-then-invalid.:  %d   <- accepted churn, healed\n", r.logCounts.churnN)
		fmt.Printf("        dig steals:              %d\n", r.logCounts.stealN)
	}

	if len(r.waitCauses) > 0 {
		fmt.Printf("\nWAIT CAUSES SEEN\n")
		for _, c := range r.waitCauses {
			fmt.Printf("        %-28s %d\n", c.cause, c.n)
		}
	}

	fmt.Printf("\nINVARIANTS\n")
	if len(r.violations) == 0 {
		fmt.Println("        all clear")
	}
	for _, v := range r.violations {
		fmt.Printf("        VIOLATION: %s\n", v)
	}
}

// summary is the one line the campaign asked for: a soak run readable at a
// glance, with the tripwire and the violation count on it because those are the
// two numbers that decide whether anyone needs to look further.
func (r *report) summary() string {
	hard := "?"
	if r.logCounts != nil {
		hard = fmt.Sprintf("%d", r.logCounts.hardN)
	}
	reuse := "n/a"
	if r.robotReuse.consecutivePairs > 0 {
		reuse = fmt.Sprintf("%.0f%%", 100*float64(r.robotReuse.sameRobot)/float64(r.robotReuse.consecutivePairs))
	}
	return fmt.Sprintf(
		"SOAK: orders %d done/%d fail · digs %d (max %d legs) · reuse %s · gated %.0fs vs ungated %.0fs · "+
			"dwell %d avg %.0fs max %.0fs (%d open) · dissolves %d · hard-burials %s · violations %d",
		r.orders.completed, r.orders.failed, r.digs.parents, r.digs.maxLegs, reuse,
		r.gated[true].avgCycleS, r.gated[false].avgCycleS,
		r.dwell.n, r.dwell.avgS, r.dwell.maxS, r.dwell.open,
		r.dissolves, hard, len(r.violations))
}

func tailNote(n int) string {
	if n >= 3 {
		return "   <- the tail that prices the chainer"
	}
	return ""
}

func markedLabel(m bool) string {
	if m {
		return "marked"
	}
	return "unmarked"
}

func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "-"
}

// chapterEndCancelCountQuery / chapterEndCancelArgs build the count over EVERY
// way a chapter ends, from dispatch's own list, with a placeholder per entry so
// no driver-array dependency is needed for a two-element IN.
func chapterEndCancelCountQuery() string {
	details := dispatch.ChapterEndCancelDetails()
	ph := make([]string, len(details))
	for i := range details {
		ph[i] = fmt.Sprintf("$%d", i+1)
	}
	return `SELECT COUNT(*) FROM orders WHERE error_detail IN (` + strings.Join(ph, ",") + `)`
}

func chapterEndCancelArgs() []any {
	details := dispatch.ChapterEndCancelDetails()
	args := make([]any, len(details))
	for i, d := range details {
		args[i] = d
	}
	return args
}

func scalar(db *store.DB, q string, args ...any) int {
	var n int
	if err := db.DB.QueryRow(q, args...).Scan(&n); err != nil {
		return 0
	}
	return n
}

// tallyValue pulls the integer immediately following `prefix` out of a running
// tally line, e.g. "BYPASS=3" or "soft-hold burials 12 (longest ...)".
//
// It exists because the burial-shadow lines are re-emitted every sweep, so the
// only honest reading is the VALUE they carry rather than how many times they
// were printed. Returns false when the prefix is absent or not followed by
// digits, so a format change degrades to "no reading" instead of to a wrong one.
func tallyValue(line, prefix string) (int, bool) {
	i := strings.Index(line, prefix)
	if i < 0 {
		return 0, false
	}
	rest := line[i+len(prefix):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0, false
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0, false
	}
	return n, true
}

// checkTerminalWithCongestionCause reports a terminal order carrying a congestion-shaped queue cause.
func checkTerminalWithCongestionCause(db *store.DB) []string {
	var out []string
	// "Terminating demand is a no-no", executable. A congestion-shaped cause on
	// a terminal order means something waited and then died anyway.
	if n := scalar(db, `
		SELECT COUNT(*) FROM orders
		WHERE status = $1
		  AND queue_cause IS NOT NULL AND queue_cause <> ''
		  AND queue_cause NOT IN ('config-failure', 'fleet-error')`,
		string(protocol.StatusFailed)); n > 0 {
		out = append(out, fmt.Sprintf("%d order(s) FAILED carrying a congestion-shaped queue cause", n))
	}
	return out
}

// checkDoubleOccupancy reports a lane with two occupancy owners.
func checkDoubleOccupancy(db *store.DB) []string {
	var out []string

	// Two occupants in one lane. Hold B admits the asker and refuses everyone
	// else, so two distinct owners on one lane is the hold not holding.
	//
	// STILL A HARD VIOLATION, AND NO LONGER THE WHOLE COLLISION CHECK. Since the
	// exit release (9f74ad6a) an order drops its row the moment it LIFTS its bin,
	// while the robot is still driving out of the corridor. So the two robots this
	// check was built to catch can now be in one lane with only ONE row between
	// them, and this count stays at zero through exactly the scenario the release
	// was warned about. The traversal-window check below is the other half; read
	// them together or neither means what it says.
	if n := scalar(db, `
		SELECT COUNT(*) FROM (
			SELECT node_id FROM reservations
			WHERE resource_kind = 'occupancy' AND state <> 'released'
			GROUP BY node_id HAVING COUNT(DISTINCT order_id) > 1
		) x`); n > 0 {
		out = append(out, fmt.Sprintf("%d lane(s) with TWO occupancy owners", n))
	}
	return out
}

// checkPhantomEntrants reports an entrant the lane never saw arrive.
func checkPhantomEntrants(db *store.DB) []string {
	var out []string

	// THE PHANTOM ENTRANT — the dual of the check above, and the one whose
	// absence made this tool's "0 violations" meaningless for F-12.
	//
	// Every occupancy assertion in this repo hunts rows that should not exist.
	// Not one asked whether a row that SHOULD exist does. So an order that
	// dispatched into a lane and wrote nothing was arithmetically incapable of
	// raising the two-occupants count above 1, and the soak reported clean over a
	// population that had largely been deleted from the ledger. The zero was not
	// evidence; it was a corollary of the defect.
	//
	// `staged` is excluded and that is not a fudge: a gate-staged order dwells
	// OUTSIDE the corridor and correctly holds no row, which is pinned by
	// TestCharSeam_GatedCreate_TakesNoOccupancyUntilTheTail. So are the
	// pre-dispatch statuses, which have no robot yet.
	//
	// ── REDEFINED AFTER THE EXIT RELEASE, BECAUSE IT STOPPED MEANING THIS ──
	//
	// "Executing in a lane and holding no occupancy row" was a defect when the
	// only release was the DROPOFF. Since 9f74ad6a it is also the INTENDED state
	// of every order that has picked its bin and is driving out — so the raw
	// count began mixing a real defect with the design, and grew with throughput
	// because the design does. It was reported as five phantom entrants and read
	// as evidence for building the exit marker; most of it was the marker's own
	// premise being measured back.
	//
	// The discriminator is WHERE THE BIN IS, which is the one fact that says
	// whether the robot has crossed the lane boundary yet — and it is asked of
	// the SOURCE end only:
	//
	//	SOURCE lane — the order was sent to take a bin OUT. While that bin is
	//	              still in the lane it has not lifted, so the robot is inbound
	//	              or inside and MUST be declared. Once the bin has left, the
	//	              release was correct and the order is in the traversal window.
	//
	// THE DEST END IS NOT JUDGEABLE FROM THESE COLUMNS, and trying cost this
	// checker its meaning once already. A multi-segment order takes occupancy for
	// its PRE-WAIT nodes only — commitToFleet over planNodes(preWait), and
	// complex_dispatch.go says why in as many words: a row for a lane the robot
	// may reach in ten minutes would wall that lane for the whole dwell. So an
	// order in_transit toward a delivery node in a lane it has not been dispatched
	// into yet CORRECTLY holds no row, and no column here can separate that from a
	// real omission — it needs steps_json and wait_index. Measured, not assumed:
	// on the dev stack seven of the eight rows a dest-end test flagged were this
	// exact by-design case (order 47's UTN_014 sits after its wait).
	//
	// Reported with lane and status rather than as a bare count, because the
	// judgement is the reader's — unchanged from the original, and the reason the
	// original was salvageable at all.
	phantomQ := fmt.Sprintf(`
		SELECT o.id, o.status, l.name
		FROM orders o
		JOIN nodes s ON s.name = o.source_node
		JOIN nodes l ON l.id = s.parent_id
		JOIN node_types lt ON lt.id = l.node_type_id AND lt.code = 'LANE'
		LEFT JOIN reservations r
		       ON r.order_id = o.id AND r.resource_kind = 'occupancy'
		      AND r.node_id = l.id AND r.state <> 'released'
		JOIN bins b   ON b.id = o.bin_id
		LEFT JOIN nodes bn ON bn.id = b.node_id
		WHERE o.vendor_order_id <> ''
		  AND o.status NOT IN (%s)
		  AND o.status NOT IN (%s)
		  AND o.status <> '%s'
		  AND r.id IS NULL
		  AND bn.parent_id IS NOT DISTINCT FROM l.id
		ORDER BY o.id LIMIT 12`,
		protocol.TerminalStatusSQLList(), protocol.PreDispatchStatusSQLList(),
		protocol.StatusStaged)
	if rows, err := db.DB.Query(phantomQ); err == nil {
		for rows.Next() {
			var id int
			var status, lane string
			if err := rows.Scan(&id, &status, &lane); err != nil {
				continue
			}
			out = append(out, fmt.Sprintf(
				"PHANTOM ENTRANT: order %d (%s) was sent to pick in lane %s, its bin is still in "+
					"that lane so it has not lifted, and it holds no occupancy row — it is invisible "+
					"to everyone else's admission", id, status, lane))
		}
		rows.Close()
	}
	return out
}

// checkNotJudgedAtDestEnd reports a leg that reached the destination end unjudged.
func checkNotJudgedAtDestEnd(db *store.DB) []string {
	var out []string

	// WHAT THE DISCRIMINATOR REFUSES TO JUDGE, COUNTED RATHER THAN DROPPED — a
	// filter that silently removes its hard cases reads as "covered everything".
	//
	// Two populations: the dest-end rows above (correct-by-design or not, this
	// tool cannot tell without parsing the plan), and executing orders carrying no
	// bin_id at all, which have no bin to locate. The second is its own finding —
	// it is the nil-BinID family the round left open.
	if n := scalar(db, fmt.Sprintf(`
		SELECT COUNT(DISTINCT o.id)
		FROM orders o
		JOIN nodes s ON s.name = o.delivery_node
		JOIN nodes l ON l.id = s.parent_id
		JOIN node_types lt ON lt.id = l.node_type_id AND lt.code = 'LANE'
		LEFT JOIN reservations r
		       ON r.order_id = o.id AND r.resource_kind = 'occupancy'
		      AND r.node_id = l.id AND r.state <> 'released'
		WHERE o.vendor_order_id <> ''
		  AND o.status NOT IN (%s)
		  AND o.status NOT IN (%s)
		  AND o.status <> '%s'
		  AND r.id IS NULL`,
		protocol.TerminalStatusSQLList(), protocol.PreDispatchStatusSQLList(),
		protocol.StatusStaged)); n > 0 {
		out = append(out, fmt.Sprintf(
			"NOT JUDGED (dest end): %d executing order(s) name a delivery node in a lane and hold no "+
				"occupancy row. Expected whenever that dropoff is in a post-wait segment the robot "+
				"has not been sent on — deciding needs steps_json, which this tool does not parse. "+
				"Neither a violation nor a clean bill", n))
	}
	return out
}

// checkNotJudgedNoBin reports a leg judged with no bin to judge.
func checkNotJudgedNoBin(db *store.DB) []string {
	var out []string
	if n := scalar(db, fmt.Sprintf(`
		SELECT COUNT(DISTINCT o.id)
		FROM orders o
		JOIN nodes s ON s.name IN (o.source_node, o.delivery_node)
		JOIN nodes l ON l.id = s.parent_id
		JOIN node_types lt ON lt.id = l.node_type_id AND lt.code = 'LANE'
		LEFT JOIN reservations r
		       ON r.order_id = o.id AND r.resource_kind = 'occupancy'
		      AND r.node_id = l.id AND r.state <> 'released'
		WHERE o.vendor_order_id <> ''
		  AND o.status NOT IN (%s)
		  AND o.status NOT IN (%s)
		  AND o.status <> '%s'
		  AND r.id IS NULL
		  AND o.bin_id IS NULL`,
		protocol.TerminalStatusSQLList(), protocol.PreDispatchStatusSQLList(),
		protocol.StatusStaged)); n > 0 {
		out = append(out, fmt.Sprintf(
			"NOT JUDGED (no bin): %d executing order(s) in a lane hold no occupancy row AND carry no "+
				"bin_id, so there is no bin to locate against the mouth. An executing order with no "+
				"bin_id is its own finding", n))
	}
	return out
}

// checkTraversalOverlap reports two legs traversing one lane at once.
func checkTraversalOverlap(db *store.DB) []string {
	var out []string

	// ── THE COLLISION CHECK THE EARLY RELEASE ACTUALLY NEEDS ──────────────
	//
	// The two-owners count above cannot see the risk 9f74ad6a took, because that
	// risk is precisely ONE row and one un-rowed robot: the first order lifted,
	// dropped its row, and is driving out; the second was admitted into the lane
	// it is still inside. Two robots, one corridor, one occupancy row, and every
	// existing assertion silent.
	//
	// So this counts the overlap directly: a lane where somebody HOLDS occupancy
	// while somebody else is in the traversal window on the same lane. That is
	// the measurement the exit-marker decision turns on — not the phantom count,
	// which is now (correctly) dominated by orders behaving as designed.
	//
	// It is a WATCH, not a violation: the window is an owner ruling taken
	// deliberately, and a non-zero count here is the evidence that it bites, not
	// proof that something is broken. Named so the reader can tell those apart.
	if rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT l.name, COUNT(DISTINCT o.id)
		FROM orders o
		JOIN nodes s  ON s.name = o.source_node
		JOIN nodes l  ON l.id = s.parent_id
		JOIN node_types lt ON lt.id = l.node_type_id AND lt.code = 'LANE'
		LEFT JOIN reservations r
		       ON r.order_id = o.id AND r.resource_kind = 'occupancy'
		      AND r.node_id = l.id AND r.state <> 'released'
		JOIN bins b   ON b.id = o.bin_id
		LEFT JOIN nodes bn ON bn.id = b.node_id
		WHERE o.vendor_order_id <> ''
		  AND o.status NOT IN (%s)
		  AND o.status NOT IN (%s)
		  AND o.status <> '%s'
		  AND r.id IS NULL
		  AND bn.parent_id IS DISTINCT FROM l.id
		  AND EXISTS (
		    SELECT 1 FROM reservations h
		    WHERE h.resource_kind = 'occupancy' AND h.state <> 'released'
		      AND h.node_id = l.id AND h.order_id <> o.id
		  )
		GROUP BY l.name ORDER BY l.name LIMIT 12`,
		protocol.TerminalStatusSQLList(), protocol.PreDispatchStatusSQLList(),
		protocol.StatusStaged)); err == nil {
		for rows.Next() {
			var lane string
			var n int
			if err := rows.Scan(&lane, &n); err != nil {
				continue
			}
			out = append(out, fmt.Sprintf(
				"TRAVERSAL OVERLAP (watch, not a violation): lane %s has an occupancy holder AND %d "+
					"order(s) still driving out of it having already released. This is the window the "+
					"exit release opened by owner ruling — it is what the EXIT MARKER decision waits "+
					"on, and the phantom count is not", lane, n))
		}
		rows.Close()
	}
	return out
}

// checkTerminalOrderReservations reports a reservation still held by a terminal order.
func checkTerminalOrderReservations(db *store.DB) []string {
	var out []string

	// A hold outliving its order. Terminalization releases by order, so a live
	// reservation owned by a terminal order is an orphan nothing will clear.
	//
	// BROKEN OUT BY KIND, because the kinds fail differently and one of them is
	// what this soak was specifically asked to watch. A leaked `occupancy` row is
	// a lane that reads OCCUPIED FOREVER to every future entrant — the F-12 fix's
	// exact failure mode, and the one that silently removes a corridor from the
	// plant. A leaked `slot` or `mouth` row is narrower. A bare total cannot tell
	// them apart, and "N reservations leaked" is not an answer to "did complex
	// occupancy release on every path".
	if rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT r.resource_kind, COUNT(*) FROM reservations r
		JOIN orders o ON o.id = r.order_id
		WHERE r.state <> 'released'
		  AND o.status IN (%s)
		GROUP BY 1 ORDER BY 1`, protocol.TerminalStatusSQLList())); err == nil {
		for rows.Next() {
			var kind string
			var n int
			if err := rows.Scan(&kind, &n); err != nil {
				continue
			}
			detail := ""
			if kind == "occupancy" {
				detail = " — that lane reads OCCUPIED to every future entrant, permanently"
			}
			out = append(out, fmt.Sprintf("%d %s reservation(s) held by a TERMINAL order%s", n, kind, detail))
		}
		rows.Close()
	}
	return out
}

// checkQueuedWithoutCause reports an order queued five minutes with no cause on the row.
func checkQueuedWithoutCause(db *store.DB) []string {
	var out []string

	// A wait with no cause on the row. The operator sentence is the whole point
	// of parking rather than failing; a parked order nobody can explain is the
	// shape this stream exists to refuse.
	if n := scalar(db, `
		SELECT COUNT(*) FROM orders
		WHERE status = 'queued'
		  AND (queue_cause IS NULL OR queue_cause = '')
		  AND created_at < now() - interval '5 minutes'`); n > 0 {
		out = append(out, fmt.Sprintf("%d order(s) queued 5min+ with NO cause on the row", n))
	}
	return out
}

// checkArmoredWithNoVendorOrder reports an order wearing armor the fleet never
// took: the crash sliver.
//
// ── THE CASE NOTHING IN THIS FILE ASKED ABOUT ─────────────────────────────
//
// Every dispatched-family check above filters for a NON-EMPTY vendor_order_id, and the
// filter is right for what those checks measure — they judge a leg the fleet is
// carrying, and a leg with no vendor id is not one. But it means the empty case
// was never asked anywhere, and nothing else asks it either: the poller's own
// re-registration query selects non-empty ids only (store/orders/orders.go
// ListTrackedVendorOrderIDs), so an order in this state is watched by nothing at
// all.
//
// It is reachable. Core writes `dispatched` and then creates the vendor order —
// the CAS-before-create ordering, kept deliberately because it is the
// racing-dispatchers lock — so there is a window, and a crash inside it leaves an
// order armored, claimed, holding its lane, and invisible to the one loop that
// would have noticed it stopped moving.
//
// THE AGE BOUND IS WHAT MAKES IT A FINDING rather than a race report. Inside the
// window the state is correct and expected; minutes later it is a wedge. Five
// minutes is the same bound checkQueuedWithoutCause uses for the same reason —
// long enough that a transient cannot reach it, short enough to catch the wedge
// inside a shift.
//
// Reported with the order id and status, because what to do about one of these
// is the reader's judgement: the bin and lane it holds have to go somewhere, and
// no sweep in this house cancels an order on a timer.
func checkArmoredWithNoVendorOrder(db *store.DB) []string {
	var out []string
	rows, err := db.DB.Query(fmt.Sprintf(`
		SELECT id, status, EXTRACT(EPOCH FROM (now() - updated_at))::bigint
		FROM orders
		WHERE status IN (%s)
		  AND (vendor_order_id IS NULL OR vendor_order_id = '')
		  AND updated_at < now() - interval '5 minutes'
		ORDER BY id LIMIT 12`, protocol.VendorActiveStatusSQLList()))
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, ageSec int64
		var status string
		if err := rows.Scan(&id, &status, &ageSec); err != nil {
			continue
		}
		out = append(out, fmt.Sprintf(
			"ARMORED, NO FLEET ORDER: order %d has been %s for %s with an empty vendor_order_id — "+
				"Core wrote the status and the create never landed, so it holds its claims and its "+
				"lane while nothing polls it (the poller's re-registration selects non-empty ids only)",
			id, status, protocol.FormatDuration(time.Duration(ageSec)*time.Second)))
	}
	return out
}

// checkFloorReleaseHistogram reports how orders were freed by the periodic floor release.
func checkFloorReleaseHistogram(db *store.DB) []string {
	var out []string

	// ── THE FLOOR TRIPWIRE ─────────────────────────────────────────────────
	//
	// Every release the lane liveness floor made is a recovery_actions row, and
	// this is the histogram BY CAUSE. It is the batch's deliverable, not a
	// health check: each cause with a non-zero count is a wait whose event
	// releaser did not fire, ranked by how often, which is the worklist an
	// emitter hunt runs on instead of a hunch.
	//
	// EXPECTED TO TREND TO ZERO as the emitters are found — except for
	// fleet-error, which is absence-class. Nothing emits "the fleet became
	// willing", so floor releases under it are the DESIGN and a fleet-health
	// signal rather than a missing subscription. It is separated here rather
	// than suppressed, for the same reason the burial shadow counts its known
	// gap apart instead of hiding it.
	if rows, err := db.DB.Query(`
		SELECT COALESCE(NULLIF(split_part(split_part(detail, 'cause "', 2), '"', 1), ''), '(none)'),
		       COUNT(*)
		FROM recovery_actions
		WHERE action = $1
		GROUP BY 1 ORDER BY 2 DESC`, dispatch.FloorReleaseAction); err == nil {
		for rows.Next() {
			var cause string
			var n int
			if err := rows.Scan(&cause, &n); err != nil {
				continue
			}
			switch cause {
			case string(dispatch.CauseFleetRefusedCreate):
				out = append(out, fmt.Sprintf("FLOOR-RELEASE (expected, absence-class): %d under %s — "+
					"no event exists for this; read it as fleet health, not a missing emitter", n, cause))
			case "(none)", "(no cause on the row)":
				// A BLANK IS NOT A MISSING EMITTER — causeReleasers is not the file to
				// open — and it is not ONE defect either. It is two, and they want
				// opposite investigations: an arm that refused without recording, or
				// nothing having evaluated the order at all. §12.49 traced a specimen
				// of the second kind (order 56: freed 17s after staging, with zero
				// refusals logged against it while the same line fired 77 times for
				// others), and the old wording sent the reader hunting an arm that did
				// not exist. Same split the floor's own record now makes.
				out = append(out, fmt.Sprintf("FLOOR-RELEASE (blank cause): %d order(s) freed by the "+
					"floor with NO cause on the row. TWO possible defects: an arm refused them "+
					"without calling setQueueReason, OR nothing ever evaluated them and the missing "+
					"EVENT is the defect. Check the log for a refusal naming the order — no refusal "+
					"means the second. Not an inventory gap either way", n))
			default:
				out = append(out, fmt.Sprintf("FLOOR-RELEASE: %d order(s) freed by the periodic floor "+
					"under cause %s — an event should have done this; see causeReleasers for which",
					n, cause))
			}
		}
		rows.Close()
	}
	return out
}

// checkUndeclaredWaits reports orders waiting under a cause nothing declares.
func checkUndeclaredWaits(db *store.DB) []string {
	var out []string

	// ── OBSERVED VS DECLARED ───────────────────────────────────────────────
	//
	// The plant's own rows, grouped by what they are waiting under, minus the
	// declared inventory. A (status, cause) pair the plant produces and the table
	// does not describe is a hold class nobody designed a way out of — the exact
	// shape of F-22, found from the other end.
	//
	// It reads the DECLARED set from dispatch rather than a copy, so this cannot
	// drift into checking a stale list: totality guarantees those keys are the
	// whole cause vocabulary.
	declared := map[string]bool{}
	for _, c := range dispatch.DeclaredQueueCauses() {
		declared[c] = true
	}
	if rows, err := db.DB.Query(`
		SELECT DISTINCT status, COALESCE(queue_cause, '')
		FROM orders
		WHERE COALESCE(queue_cause, '') <> ''`); err == nil {
		for rows.Next() {
			var status, cause string
			if err := rows.Scan(&status, &cause); err != nil {
				continue
			}
			if !declared[cause] {
				out = append(out, fmt.Sprintf("UNDECLARED WAIT: orders sit at status=%s under cause %q, "+
					"which has no causeReleasers row — nothing on record says what ends it, and no "+
					"floor claims it", status, cause))
			}
		}
		rows.Close()
	}
	return out
}

// checkStalledOrders reports orders that have not moved for their population's budget.
func checkStalledOrders(db *store.DB) []string {
	var out []string

	// ── THE STALL CHECKER ──────────────────────────────────────────────────
	//
	// "0 failures, 0 violations" was TRUE and INSUFFICIENT. Every checker above
	// this line watches an INVARIANT — is the ledger consistent, did anything die
	// wrongly — and an order that simply stops making progress violates none of
	// them. The owner found four such rows on the live board by eye, after a run
	// this tool had called clean. Nothing was watching PROGRESS.
	//
	// A stalled order is not a broken invariant, so it is reported with its CAUSE
	// rather than as a bare count: the cause is the whole difference between a
	// legitimate long wait (the loop consumed faster than it produced) and a wedge
	// (a lane wait nothing can release). It flags both, because at soak scale the
	// distinction is a judgement and the tool's job is to put the row in front of
	// someone rather than to make it.
	//
	// T IS PER KIND, and the numbers come from what the rig actually does rather
	// than from taste:
	//
	//   staged  90s — a dwell at a mark is meant to be the length of somebody
	//                 else's lane transit. The rig's mean cycle is ~120s, so a
	//                 robot parked longer than that is not waiting for a lane, it
	//                 is waiting for something that is not coming.
	//   queued  15m — a park is expected to be long. Material waits legitimately
	//                 run to the next production tick, and the loop's slowest
	//                 replenishment cycle is minutes. Below this it would cry
	//                 wolf every tick.
	//   in-flight 20m — a robot executing one leg. The sim's longest transit is
	//                 well under this; a leg older than it is not moving.
	//
	// Deliberately NOT one number: 90s would flag every honest material wait and
	// 15m would have missed all three staged rows, which are the ones that
	// mattered. If a kind ever needs a fourth threshold, add it here rather than
	// widening one of these.
	//
	// THE POPULATIONS ARE DERIVED FROM THE STATUS ENUM, NOT HAND-LISTED, and that
	// is a fix rather than tidying. The "parked" kind used to be `status='queued'`
	// alone while "in flight" excluded queued, staged, pending AND sourcing — so
	// `pending` and `sourcing` matched NONE of the three and were watched by
	// nothing. Those are the pre-dispatch statuses where a held compound leg and a
	// rolled-back fleet refusal both wait, which is to say the checker was blind
	// in exactly the place a stall is cheapest to miss.
	//
	// stallPopulations carries a Go predicate per kind and renders its own SQL, so
	// the set the checker queries and the set the drift test reasons about are one
	// thing. TestStallPopulationsPartitionTheNonTerminalStatuses fails if a new
	// status lands in none of them or in two.
	for _, s := range stallPopulations {
		rows, err := db.DB.Query(fmt.Sprintf(`
			SELECT id, status, COALESCE(NULLIF(queue_cause,''), '(no cause on the row)'),
			       EXTRACT(EPOCH FROM (now() - updated_at))::int
			FROM orders
			WHERE %s AND updated_at < now() - interval '%s'
			ORDER BY updated_at LIMIT 12`, s.clause(), s.after))
		if err != nil {
			continue
		}
		for rows.Next() {
			var id, age int
			var status, cause string
			if err := rows.Scan(&id, &status, &cause, &age); err != nil {
				continue
			}
			out = append(out, fmt.Sprintf("STALLED: order %d %s (%s) for %dm — cause: %s",
				id, s.label, status, age/60, cause))
		}
		rows.Close()
	}
	return out
}

// checkNegativeTotalUOP reports a negative total UOP across bins.
func checkNegativeTotalUOP(db *store.DB) []string {
	var out []string

	// The inventory invariant, the same one /api/inventory/invariant serves.
	total := scalar(db, `SELECT COALESCE(SUM(uop_count), 0) FROM bins`)
	if total < 0 {
		out = append(out, "negative total UOP across bins")
	}
	return out
}
