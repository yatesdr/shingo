package main

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"shingo/protocol"
	"shingocore/dispatch"
)

// STRANDED ORDERS: the jam check, and the first thing a run report says.
//
// A run's completed count says nothing about the orders it did not complete.
// The owner's dev stack on main ran a demo plant into a permanent jam on
// 2026-10-05 (23 orders frozen behind one dig leg with nowhere to park), and a
// throughput table read off that plant would have looked like a slow run, not a
// dead one. So the run end is read for orders that stopped:
//
//   - every non-terminal order older than strandedAge sim-minutes, and
//   - every non-terminal order whose status or wait has not changed for
//     strandedQuiet sim-minutes (its newest history row: a park writes one per
//     cause change, so a changed wait is a row).
//
// Both ages are on the sim clock (the newest history row in the table), as
// hist-wedges reads it: history is stamped on the sim clock, which a sped-up sim
// runs ahead of the server's.
//
// Each stranded order is shown with what it waits on and who holds that, and
// the chain followed back until it reaches an order that is still moving, a
// waiter nobody holds, or a cycle.
//
// THE VERDICT. A stranded order has a releaser pending when its chain reaches
// a LIVE order (not stranded itself, so an event of its is still to come), or
// ends in a wait nobody holds whose cause has an EVENT releaser in the
// inventory (dispatch.ReleaserOf: material appears, a slot frees) while work
// is progressing where that event would come from: an order dispatched or
// later in the last strandedQuiet sim-minutes at the waiter's own nodes, their
// lane or their group. A cycle, a chain that ends in a floored cause, or an end
// with no work progressing near it has none, and the run FAILS on it, whatever
// its completed count. A material wait beside a working supply is not a jam;
// a market cluster beside a shuttle loop elsewhere is one. The looser first cut
// (any order moving anywhere in the plant) passed main's 2026-10-05 jam, whose
// market cluster stood a day beside a shuttle completing every two minutes; that
// is the reason the refinement to the waiter's own nodes, lane and group exists.
//
// THE CLOCK. Run against a live Core with its config, "now" is the sim clock
// itself, so a frozen plant ages. Read from a dump, it is the newest history
// row, and a frozen plant's last movers read as live: the table says which.
//
// Only columns main also has are read (orders, order_history, reservations,
// nodes, bins), so -stranded reads main's database too.
const (
	strandedAge   = 15 // sim-minutes since the order's first history row
	strandedQuiet = 10 // sim-minutes since its newest history row
	chainMax      = 8
)

// strandedVerdictPrefix starts the violation a failing table adds.
const strandedVerdictPrefix = "STRANDED:"

type activeOrder struct {
	id, parent                               int64
	typ, status, src, dst, station, payload  string
	robot, vendorState, cause, reason, steps string
	waitIndex                                int
	ageMin, quietMin                         float64
	stranded                                 bool
	holds                                    []string // what it holds, for the table
	children                                 []int64  // non-terminal legs
	waitLane                                 int64    // the lane its current wait is on, 0 if none
	waitLaneName                             string
}

type strandedReport struct {
	active   []*activeOrder // every non-terminal order, by id
	stranded []strandedRow
	noRel    int
	clock    string // where "now" came from
	moved    int    // orders that progressed (dispatched or later) in the last strandedQuiet sim-minutes
}

type strandedRow struct {
	o        *activeOrder
	waitsOn  string
	holders  string
	chain    string
	releaser bool
}

// readStranded reads the run end. An error is a failed instrument, never an
// empty table.
//
// simNow is the sim clock when the caller has one; nil reads the newest
// history row.
func readStranded(db *sql.DB, simNow *time.Time) (*strandedReport, error) {
	rep := &strandedReport{clock: "newest history row (no live sim clock)"}
	var now any
	if simNow != nil {
		now = simNow.UTC()
		rep.clock = "the live sim clock, " + simNow.UTC().Format(time.RFC3339)
	}
	re, err := readActive(db, now)
	if err != nil {
		return nil, err
	}
	if err := re.readHolds(db); err != nil {
		return nil, err
	}
	if err := re.nameWaitLanes(db); err != nil {
		return nil, err
	}
	near, err := progressNear(db, now)
	if err != nil {
		return nil, err
	}
	rep.moved = near.orders
	for _, id := range re.ids {
		rep.active = append(rep.active, re.byID[id])
	}
	for _, o := range rep.active {
		if !o.stranded {
			continue
		}
		by, what := re.waitsOn(o)
		row := strandedRow{o: o, waitsOn: what, holders: holderText(by, re.byID)}
		row.chain, row.releaser = followChain(o, re.byID, re.waitsOn, near.moved)
		if !row.releaser {
			rep.noRel++
		}
		rep.stranded = append(rep.stranded, row)
	}
	return rep, nil
}

// runEnd is what the table reads at the run end: every non-terminal order by
// id (ids in order), and every active hold with the lane it sits in.
type runEnd struct {
	byID  map[int64]*activeOrder
	ids   []int64
	holds []hold
}

// hold is one active reservation or hard claim, with the lane it sits in (a
// slot's parent, a mouth or occupancy row's own node, a bin's slot's parent).
type hold struct {
	order  int64
	laneID int64
	text   string
}

// readActive reads every non-terminal order, aged on now, and links each
// order to its non-terminal legs.
func readActive(db *sql.DB, now any) (*runEnd, error) {
	re := &runEnd{byID: map[int64]*activeOrder{}}
	rows, err := db.Query(`
		WITH now_sim AS (SELECT COALESCE($1::timestamptz, (SELECT MAX(created_at) FROM order_history)) AS t),
		     h AS (SELECT order_id, MIN(created_at) AS first_at, MAX(created_at) AS last_at
		           FROM order_history GROUP BY order_id)
		SELECT o.id, COALESCE(o.parent_order_id, 0), o.order_type, o.status, o.source_node, o.delivery_node,
		       o.station_id, o.payload_code, o.robot_id, o.vendor_state,
		       COALESCE(o.queue_cause, ''), o.queue_reason, o.steps_json, o.wait_index,
		       COALESCE(EXTRACT(EPOCH FROM ((SELECT t FROM now_sim) - COALESCE(h.first_at, o.created_at))) / 60.0, 0),
		       COALESCE(EXTRACT(EPOCH FROM ((SELECT t FROM now_sim) - COALESCE(h.last_at, o.updated_at))) / 60.0, 0)
		FROM orders o LEFT JOIN h ON h.order_id = o.id
		WHERE o.status NOT IN (`+protocol.TerminalStatusSQLList()+`)
		ORDER BY o.id`, now)
	if err != nil {
		return nil, fmt.Errorf("active orders: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		o := &activeOrder{}
		if err := rows.Scan(&o.id, &o.parent, &o.typ, &o.status, &o.src, &o.dst, &o.station, &o.payload,
			&o.robot, &o.vendorState, &o.cause, &o.reason, &o.steps, &o.waitIndex, &o.ageMin, &o.quietMin); err != nil {
			return nil, fmt.Errorf("active orders: %w", err)
		}
		o.stranded = o.ageMin > strandedAge || o.quietMin > strandedQuiet
		o.waitLane = laneWaitOf(o.steps, o.waitIndex)
		re.byID[o.id] = o
		re.ids = append(re.ids, o.id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("active orders: %w", err)
	}
	for _, o := range re.byID {
		if p, ok := re.byID[o.parent]; ok {
			p.children = append(p.children, o.id)
		}
	}
	return re, nil
}

// readHolds reads every active hold, and lists each active order's own.
func (re *runEnd) readHolds(db *sql.DB) error {
	rows, err := db.Query(`
		SELECT r.order_id, r.resource_kind, r.state,
		       COALESCE(b.label, ''), COALESCE(n.name, bn.name, ''),
		       COALESCE(CASE WHEN r.resource_kind IN ('mouth', 'occupancy') THEN n.id ELSE COALESCE(n.parent_id, bn.parent_id) END, 0)
		FROM reservations r
		LEFT JOIN bins b ON b.id = r.bin_id LEFT JOIN nodes bn ON bn.id = b.node_id
		LEFT JOIN nodes n ON n.id = r.node_id
		UNION ALL
		SELECT b.claimed_by, 'claim', 'hard', b.label, COALESCE(n.name, ''), COALESCE(n.parent_id, 0)
		FROM bins b LEFT JOIN nodes n ON n.id = b.node_id WHERE b.claimed_by IS NOT NULL`)
	if err != nil {
		return fmt.Errorf("active holds: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var h hold
		var kind, state, bin, node string
		if err := rows.Scan(&h.order, &kind, &state, &bin, &node, &h.laneID); err != nil {
			return fmt.Errorf("active holds: %w", err)
		}
		h.text = kind + " " + state + " " + strings.TrimSpace(bin+" "+node)
		re.holds = append(re.holds, h)
		if o, ok := re.byID[h.order]; ok {
			o.holds = append(o.holds, h.text)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("active holds: %w", err)
	}
	return nil
}

// nameWaitLanes reads the name of each lane an active order waits on.
func (re *runEnd) nameWaitLanes(db *sql.DB) error {
	names := map[int64]string{}
	for _, o := range re.byID {
		if o.waitLane != 0 {
			names[o.waitLane] = ""
		}
	}
	for id := range names {
		var name string
		if err := db.QueryRow(`SELECT name FROM nodes WHERE id = $1`, id).Scan(&name); err != nil && err != sql.ErrNoRows {
			return fmt.Errorf("lane name %d: %w", id, err)
		}
		names[id] = name
	}
	for _, o := range re.byID {
		o.waitLaneName = names[o.waitLane]
	}
	return nil
}

// waitsOn is an order's direct blockers: its open legs, or the other orders
// holding anything in the lane its wait is on. Its own parent and children
// are not its blockers there: a dig's legs work under their parent's lock.
func (re *runEnd) waitsOn(o *activeOrder) ([]int64, string) {
	if len(o.children) > 0 {
		sort.Slice(o.children, func(i, j int) bool { return o.children[i] < o.children[j] })
		return o.children, "its legs " + joinIDs(o.children)
	}
	what := o.reason
	if what == "" {
		what = o.cause
	}
	if o.waitLane == 0 {
		return nil, what
	}
	seen := map[int64]bool{}
	var by []int64
	for _, h := range re.holds {
		if h.laneID != o.waitLane || h.order == o.id || h.order == o.parent || seen[h.order] {
			continue
		}
		if p, ok := re.byID[h.order]; ok && p.parent == o.id {
			continue
		}
		seen[h.order] = true
		by = append(by, h.order)
	}
	sort.Slice(by, func(i, j int) bool { return by[i] < by[j] })
	return by, fmt.Sprintf("lane %s (%s)", o.waitLaneName, what)
}

// followChain walks blockers from o until it reaches a live order (a releaser
// is pending), a waiter nobody holds (pending only if its cause has an event
// releaser and work progressed near it), a cycle or chainMax steps (none is).
// Every branch is followed; the order has a releaser if any branch has one.
func followChain(o *activeOrder, byID map[int64]*activeOrder,
	waitsOn func(*activeOrder) ([]int64, string), movedNear func(*activeOrder) bool) (string, bool) {
	end := func(cur *activeOrder) (string, bool) {
		what, evented := dispatch.ReleaserOf(cur.cause)
		switch {
		case cur.cause == "":
			return fmt.Sprintf("%d nobody holds it, and it carries no cause: nothing is coming", cur.id), false
		case !evented:
			return fmt.Sprintf("%d nobody holds it; %s is floored, not evented: nothing is coming", cur.id, cur.cause), false
		case !movedNear(cur):
			return fmt.Sprintf("%d nobody holds it; releaser %q, but no work progressed at its nodes or their groups in %d sim-min",
				cur.id, what, strandedQuiet), false
		}
		return fmt.Sprintf("%d nobody holds it; releaser %q, work progressing at its nodes or groups", cur.id, what), true
	}
	if by, _ := waitsOn(o); len(by) == 0 {
		return end(o)
	}
	var parts []string
	seen := map[int64]bool{o.id: true}
	frontier := []int64{o.id}
	for depth := 0; depth < chainMax && len(frontier) > 0; depth++ {
		var next []int64
		for _, id := range frontier {
			cur := byID[id]
			by, _ := waitsOn(cur)
			for _, b := range by {
				h, ok := byID[b]
				switch {
				case !ok:
					parts = append(parts, fmt.Sprintf("%d (ended: its event already fired)", b))
					return strings.Join(parts, " → "), true
				case !h.stranded:
					parts = append(parts, fmt.Sprintf("%d (live, %s)", b, h.status))
					return strings.Join(parts, " → "), true
				case seen[b]:
					parts = append(parts, fmt.Sprintf("%d (cycle)", b))
				default:
					seen[b] = true
					parts = append(parts, fmt.Sprintf("%d %s", b, h.status))
					next = append(next, b)
				}
			}
			if len(by) == 0 && id != o.id {
				text, ok := end(cur)
				parts = append(parts, text)
				if ok {
					return strings.Join(parts, " → "), true
				}
			}
		}
		frontier = next
	}
	return strings.Join(parts, " → "), false
}

// nearProgress is the work that moved in the last strandedQuiet sim-minutes:
// orders with a history row past acquiring (dispatched or later, terminal
// included), and the places they touched.
type nearProgress struct {
	orders int
	places map[string]bool
	parent map[string]string
}

// placesOf is a node and the nodes above it up to two levels: a lane slot, its
// lane, its group.
func (n nearProgress) placesOf(name string) []string {
	var out []string
	for i := 0; i < 3 && name != ""; i++ {
		out = append(out, name)
		name = n.parent[name]
	}
	return out
}

// moved reports whether work progressed at the waiter's own source or
// destination, their lane or their group. A shuttle running elsewhere is not
// its releaser: main's 2026-10-05 market jam stood for a day beside a loop that
// completed every two minutes.
func (n nearProgress) moved(o *activeOrder) bool {
	for _, end := range []string{o.src, o.dst} {
		for _, p := range n.placesOf(end) {
			if n.places[p] {
				return true
			}
		}
	}
	return false
}

func progressNear(db *sql.DB, now any) (nearProgress, error) {
	n := nearProgress{places: map[string]bool{}, parent: map[string]string{}}
	rows, err := db.Query(`SELECT n.name, COALESCE(p.name, '') FROM nodes n LEFT JOIN nodes p ON p.id = n.parent_id`)
	if err != nil {
		return n, fmt.Errorf("node tree: %w", err)
	}
	for rows.Next() {
		var name, parent string
		if err := rows.Scan(&name, &parent); err != nil {
			rows.Close()
			return n, fmt.Errorf("node tree: %w", err)
		}
		n.parent[name] = parent
	}
	rows.Close()
	rows, err = db.Query(fmt.Sprintf(`
		SELECT DISTINCT o.id, o.source_node, o.delivery_node
		FROM order_history h JOIN orders o ON o.id = h.order_id
		WHERE h.created_at > COALESCE($1::timestamptz, (SELECT MAX(created_at) FROM order_history)) - interval '%d minutes'
		  AND h.status NOT IN (`+protocol.PreDispatchStatusSQLList()+`, 'sourcing')`, strandedQuiet), now)
	if err != nil {
		return n, fmt.Errorf("work progressed: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var src, dst string
		if err := rows.Scan(&id, &src, &dst); err != nil {
			return n, fmt.Errorf("work progressed: %w", err)
		}
		n.orders++
		for _, end := range []string{src, dst} {
			for _, p := range n.placesOf(end) {
				n.places[p] = true
			}
		}
	}
	return n, rows.Err()
}

func holderText(by []int64, byID map[int64]*activeOrder) string {
	if len(by) == 0 {
		return "-"
	}
	var out []string
	for _, b := range by {
		if h, ok := byID[b]; ok && len(h.holds) > 0 {
			out = append(out, fmt.Sprintf("%d [%s]", b, strings.Join(h.holds, "; ")))
		} else {
			out = append(out, fmt.Sprint(b))
		}
	}
	return strings.Join(out, ", ")
}

// laneWaitOf is the lane of the wait the order is parked at, 0 if it is not a
// lane wait. Unparseable steps are no wait: the table still names the cause.
func laneWaitOf(stepsJSON string, waitIndex int) int64 {
	if stepsJSON == "" {
		return 0
	}
	var steps []struct {
		Action   string `json:"action"`
		WaitKind string `json:"wait_kind"`
		WaitLane int64  `json:"wait_lane"`
	}
	if json.Unmarshal([]byte(stepsJSON), &steps) != nil {
		return 0
	}
	seen := 0
	for _, s := range steps {
		if s.Action != protocol.ActionWait {
			continue
		}
		if seen == waitIndex {
			return s.WaitLane
		}
		seen++
	}
	return 0
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return strings.Join(s, ",")
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

func route(o *activeOrder) string {
	return firstNonEmpty(o.src, "·") + " → " + firstNonEmpty(o.dst, "·")
}

// verdict is the table's one line, and the violation when it fails.
func (r *strandedReport) verdict() string {
	if r.noRel == 0 {
		return fmt.Sprintf("PASS: %d stranded order(s), every one with a releaser pending", len(r.stranded))
	}
	return fmt.Sprintf("FAIL: %d stranded order(s), %d with no releaser event pending", len(r.stranded), r.noRel)
}

// print writes the STRANDED ORDERS table and the Active Orders snapshot, in the
// owner's Active Orders columns (ID, Route, Station, Type, Payload, Status,
// Robot, Created) with the wait's columns after them.
func (r *strandedReport) print() {
	fmt.Println("STRANDED ORDERS AT RUN END")
	fmt.Println(strings.Repeat("=", 78))
	fmt.Printf("non-terminal and older than %d sim-min, or no status/wait change in %d sim-min\n", strandedAge, strandedQuiet)
	fmt.Printf("VERDICT %s\n", r.verdict())
	fmt.Printf("clock: %s · orders that progressed in the last %d sim-min: %d\n", r.clock, strandedQuiet, r.moved)
	if len(r.stranded) == 0 {
		fmt.Println("        (none)")
	}
	for _, s := range r.stranded {
		o := s.o
		rel := "NO RELEASER"
		if s.releaser {
			rel = "releaser pending"
		}
		fmt.Printf("\n  %d  %s  %s  %s  %s  %s  robot %s  age %.1fm  quiet %.1fm  — %s\n",
			o.id, route(o), firstNonEmpty(o.station, "·"), o.typ, firstNonEmpty(o.payload, "·"), o.status,
			firstNonEmpty(o.robot, "·"), o.ageMin, o.quietMin, rel)
		fmt.Printf("      cause:    %s\n", firstNonEmpty(o.cause, "(none on the row)"))
		fmt.Printf("      waits on: %s\n", s.waitsOn)
		fmt.Printf("      held by:  %s\n", s.holders)
		if len(o.holds) > 0 {
			fmt.Printf("      holds:    %s\n", strings.Join(o.holds, "; "))
		}
		fmt.Printf("      chain:    %s\n", s.chain)
	}

	fmt.Printf("\nACTIVE ORDERS AT RUN END  %d\n", len(r.active))
	fmt.Printf("  %-6s %-34s %-14s %-15s %-9s %-11s %-8s %8s  %s\n",
		"ID", "Route", "Station", "Type", "Payload", "Status", "Robot", "age(m)", "cause")
	for _, o := range r.active {
		fmt.Printf("  %-6d %-34s %-14s %-15s %-9s %-11s %-8s %8.1f  %s\n",
			o.id, trunc(route(o), 34), trunc(firstNonEmpty(o.station, "·"), 14), trunc(o.typ, 15),
			trunc(firstNonEmpty(o.payload, "·"), 9), o.status, firstNonEmpty(o.robot, "·"), o.ageMin, o.cause)
	}
	fmt.Println()
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
