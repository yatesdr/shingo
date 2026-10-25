package main

import (
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strings"

	"shingo/protocol"
	"shingocore/store/audit"
	"shingocore/store/bins"
)

// [S.4] HANDOFF: the carrier-handoff work's before-numbers, per Edge where the
// row knows its Edge.
//
// A carrier changes hands at a seat in three messages: Core's placement record
// (a UOPAdjustment with Bound, the PLACEMENT RECORD half of a departure), the
// release that ends the carrier's generation at the seat, and, when a count
// arrives for the ended generation, Core's BinEpochRefresh. Every one of them is
// broadcast (station "*"), so every Edge receives every placement, and an Edge
// that does not model the node logs `process node "X" not found` and drops it.
// The 2026-10-09 demo read 400 of 486 placements unowned. Each number here is
// one the handoff work is meant to move:
//
//	a. placements per Edge, owned against unowned, by node class. Counted from
//	   Core's outbox (the Bound messages Core enqueued, every one sent to every
//	   Edge). An Edge owns a node it has a process node for: read from the
//	   Edge's own process_nodes when -edge-db names it, otherwise from Core's
//	   plant-claims mirror (style_claims through process_styles, the rows
//	   plantclaims.StationsClaimingNode reads, plus demand_registry). Class is
//	   the node name up to its first '_', with the node type's code after a '/'
//	   where Core has one.
//	b. stale-epoch drops per station: bin_uop_exception kind stale_epoch, whose
//	   actor is the counting station (kept forever, unlike the ledger).
//	c. releases whose after_uop differs from before_uop, per op.
//	d. epoch refreshes per target node, and how many named _TRANSIT (a carrier
//	   riding a robot, where no station models it).
//	e. labelled carriers at 0 (or below) at rest: a payload code and no stock
//	   (a count of 0, or an overpack below it), at a node other than _TRANSIT,
//	   at the collect.
//	f. consume ticks Core applied to a carrier after its release and before
//	   its next generation (the next EpochBumpOps or manifest_confirmed row on
//	   the bin): a seat still counting into a carrier it has let go.
//	g. ticks held and replayed: Edge-only (see edge.go).
//	h. landings sent by the last arm (no claiming station, no order station):
//	   the landing's address for a seat nothing names, the plant's only Edge or
//	   every Edge (RULINGS-C2c-run-through §4). Expected about 0 a run. Counted
//	   from core.log ("bin_landing: last arm", one line per landing), so n/a
//	   without -log: no outbox row says which arm addressed it.
//
// THE OUTBOX IS PURGED 24 h (wall) after a row is sent
// (outbox.MessageRetentionPeriod), so a and d are the run's whole count only for
// a run collected within a day of its start.
const measureHandoff = "[S.4] HANDOFF"

var (
	msgTypeUOPAdjustment = "data." + protocol.SubjectUOPAdjustment
	msgTypeEpochRefresh  = "data." + protocol.SubjectBinEpochRefresh
	// handoffReleaseOps are the release click's ops: the operator let the
	// carrier go. clear_for_reuse (a clear) and released_capture_empty (a
	// pull-parts count reaching zero) are not a seat letting a carrier go.
	handoffReleaseOps = []string{audit.OpReleasedEmpty, audit.OpReleasedPartial,
		audit.OpReleasedEmptyFallback, audit.OpReleasedPartialFallback, audit.OpReleasedUnderpack}
)

// nodeCount is a count at one node, with the node's class.
type nodeCount struct {
	node, class string
	n           int
}

// nodeClass is the name's prefix up to its first '_', and the node type's code
// after a '/' when there is one.
func nodeClass(name, typeCode string) string {
	prefix, _, _ := strings.Cut(name, "_")
	if prefix == "" {
		prefix = name
	}
	if typeCode != "" {
		return prefix + "/" + typeCode
	}
	return prefix
}

type staleDrops struct {
	station  string
	n, units int
}

type releaseOp struct {
	op         string
	n, changed int
}

type ticksAfterRelease struct {
	releases, followed, bins, ticks, units, maxPer int
	byStation                                      map[string]int // ticks per counting station
}

type handoffCore struct {
	placements []nodeCount
	stations   []string
	claims     map[string]map[string]bool // station -> nodes, Core's mirror
	stale      []staleDrops
	releases   []releaseOp
	refreshes  []nodeCount
	zeroAtRest []nodeCount
	after      ticksAfterRelease
}

// readHandoff runs the section's reads, each one statement.
func readHandoff(db *sql.DB) (*handoffCore, error) {
	h := &handoffCore{}
	var err error
	if h.placements, err = outboxByNode(db, msgTypeUOPAdjustment, true); err != nil {
		return nil, fmt.Errorf("handoff placements: %w", err)
	}
	if h.stations, h.claims, err = claimsMirror(db); err != nil {
		return nil, fmt.Errorf("handoff claims: %w", err)
	}
	if h.stale, err = staleEpochDrops(db); err != nil {
		return nil, fmt.Errorf("handoff stale drops: %w", err)
	}
	if h.releases, err = releasesChanged(db); err != nil {
		return nil, fmt.Errorf("handoff releases: %w", err)
	}
	if h.refreshes, err = outboxByNode(db, msgTypeEpochRefresh, false); err != nil {
		return nil, fmt.Errorf("handoff refreshes: %w", err)
	}
	if h.zeroAtRest, err = labelledAtZero(db); err != nil {
		return nil, fmt.Errorf("handoff labelled at 0: %w", err)
	}
	if h.after, err = ticksAfterReleases(db); err != nil {
		return nil, fmt.Errorf("handoff ticks after release: %w", err)
	}
	return h, nil
}

// outboxByNode counts Core's outbox rows of msgType per target node (the body's
// core_node_name), only the Bound ones when bound. The payload is the encoded
// envelope: the body is p.data.
func outboxByNode(db *sql.DB, msgType string, bound bool) ([]nodeCount, error) {
	rows, err := db.Query(`
		WITH m AS (
			SELECT convert_from(payload, 'UTF8')::jsonb #> '{p,data}' AS b
			FROM outbox WHERE msg_type = $1)
		SELECT m.b->>'core_node_name', COALESCE(MAX(nt.code), ''), COUNT(*)
		FROM m
		LEFT JOIN nodes n ON n.name = m.b->>'core_node_name'
		LEFT JOIN node_types nt ON nt.id = n.node_type_id
		WHERE NOT $2::boolean OR (m.b->>'bound')::boolean IS TRUE
		GROUP BY 1 ORDER BY 3 DESC, 1`, msgType, bound)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nodeCount
	for rows.Next() {
		var c nodeCount
		var name sql.NullString
		var typeCode string
		if err := rows.Scan(&name, &typeCode, &c.n); err != nil {
			return nil, err
		}
		c.node = name.String
		c.class = nodeClass(c.node, typeCode)
		out = append(out, c)
	}
	return out, rows.Err()
}

// claimsMirror is every Edge Core knows (edge_registry, and every station the
// mirror names) and, per station, the nodes Core's mirror says it models.
func claimsMirror(db *sql.DB) ([]string, map[string]map[string]bool, error) {
	rows, err := db.Query(`
		SELECT ps.station_id, sc.core_node_name FROM style_claims sc
		  JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
		  WHERE ps.station_id IS NOT NULL AND ps.station_id <> ''
		UNION SELECT station_id, core_node_name FROM demand_registry
		UNION SELECT station_id, '' FROM edge_registry WHERE station_id <> ''`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	claims := map[string]map[string]bool{}
	for rows.Next() {
		var station, node string
		if err := rows.Scan(&station, &node); err != nil {
			return nil, nil, err
		}
		if claims[station] == nil {
			claims[station] = map[string]bool{}
		}
		if node != "" {
			claims[station][node] = true
		}
	}
	stations := make([]string, 0, len(claims))
	for s := range claims {
		stations = append(stations, s)
	}
	sort.Strings(stations)
	return stations, claims, rows.Err()
}

// staleEpochDrops is the stale-epoch drops per counting station, and the units
// they carried (the applier's detail.delta).
func staleEpochDrops(db *sql.DB) ([]staleDrops, error) {
	rows, err := db.Query(`
		SELECT actor, COUNT(*), COALESCE(SUM(ABS((detail->>'delta')::numeric)), 0)::bigint
		FROM bin_uop_exception WHERE kind = $1
		GROUP BY actor ORDER BY actor`, audit.ExcStaleEpoch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []staleDrops
	for rows.Next() {
		var s staleDrops
		if err := rows.Scan(&s.station, &s.n, &s.units); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// sqlStrings is a quoted IN list of known constants (never of input).
func sqlStrings(ss []string) string {
	q := make([]string, len(ss))
	for i, s := range ss {
		q[i] = "'" + strings.ReplaceAll(s, "'", "''") + "'"
	}
	return strings.Join(q, ", ")
}

// releasesChanged is each release op's rows, and how many moved the count.
func releasesChanged(db *sql.DB) ([]releaseOp, error) {
	rows, err := db.Query(`
		SELECT op, COUNT(*), COUNT(*) FILTER (WHERE before_uop IS NOT NULL AND after_uop <> before_uop)
		FROM bin_uop_ledger WHERE op IN (` + sqlStrings(handoffReleaseOps) + `)
		GROUP BY op ORDER BY op`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []releaseOp
	for rows.Next() {
		var r releaseOp
		if err := rows.Scan(&r.op, &r.n, &r.changed); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// labelledAtZero is the carriers holding a label and no stock (bins.StockSQL:
// counted to 0, or overpacked below it) at a node other than _TRANSIT, per
// node.
func labelledAtZero(db *sql.DB) ([]nodeCount, error) {
	rows, err := db.Query(`
		SELECT n.name, COALESCE(MAX(nt.code), ''), COUNT(*)
		FROM bins b JOIN nodes n ON n.id = b.node_id
		LEFT JOIN node_types nt ON nt.id = n.node_type_id
		WHERE b.payload_code <> '' AND NOT ` + bins.StockSQL("b") + ` AND b.status <> 'retired'
		  AND n.name <> '_TRANSIT'
		GROUP BY n.name ORDER BY 3 DESC, 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []nodeCount
	for rows.Next() {
		var c nodeCount
		var typeCode string
		if err := rows.Scan(&c.node, &typeCode, &c.n); err != nil {
			return nil, err
		}
		c.class = nodeClass(c.node, typeCode)
		out = append(out, c)
	}
	return out, rows.Err()
}

// ticksAfterReleases counts, per release row, the consume ticks on its bin
// after it (ledger id order) and before the bin's next generation starts. One
// statement; rows are (release, counting station, ticks, units), plus one row
// per release with no tick (station NULL) so the releases are all counted.
func ticksAfterReleases(db *sql.DB) (ticksAfterRelease, error) {
	next := sqlStrings(append(append([]string{}, audit.EpochBumpOps...), audit.OpManifestConfirmed))
	rows, err := db.Query(`
		WITH rel AS (
			SELECT l.id, l.bin_id,
			       (SELECT MIN(x.id) FROM bin_uop_ledger x
			         WHERE x.bin_id = l.bin_id AND x.id > l.id AND x.op IN (` + next + `)) AS next_id
			FROM bin_uop_ledger l WHERE l.op IN (` + sqlStrings(handoffReleaseOps) + `))
		SELECT r.id, r.bin_id, t.station, COUNT(t.id), COALESCE(SUM(ABS(t.after_uop - COALESCE(t.before_uop, t.after_uop))), 0)
		FROM rel r LEFT JOIN bin_uop_ledger t
		  ON t.bin_id = r.bin_id AND t.id > r.id AND (r.next_id IS NULL OR t.id < r.next_id)
		 AND t.reason = 'consume_tick'
		GROUP BY r.id, r.bin_id, t.station`)
	if err != nil {
		return ticksAfterRelease{}, err
	}
	defer rows.Close()
	a := ticksAfterRelease{byStation: map[string]int{}}
	perRelease := map[int64]int{}
	bins := map[int64]bool{}
	for rows.Next() {
		var relID, binID int64
		var station sql.NullString
		var ticks, units int
		if err := rows.Scan(&relID, &binID, &station, &ticks, &units); err != nil {
			return a, err
		}
		perRelease[relID] += ticks
		if ticks > 0 {
			bins[binID] = true
			a.byStation[station.String] += ticks
			a.ticks += ticks
			a.units += units
		}
	}
	a.releases = len(perRelease)
	a.bins = len(bins)
	for _, n := range perRelease {
		if n > 0 {
			a.followed++
		}
		a.maxPer = max(a.maxPer, n)
	}
	return a, rows.Err()
}

// placementSplit is one Edge's placements, owned against unowned, per class.
type placementSplit struct {
	station, ownedBy      string // ownedBy: "edge-db" or "core-mirror"
	owned, unowned        int
	unownedByClass        map[string]int
	ownedByClass          map[string]int
	logNotFound, logBound int // -1 when no Edge log was read
}

// splitPlacements divides every placement between owned and unowned for each
// Edge: by the Edge's own process nodes where an Edge DB names the station,
// else by Core's mirror. An Edge DB whose station Core does not know still
// gets a row: it received every broadcast too.
func splitPlacements(h *handoffCore, edges []*edgeReading) []placementSplit {
	byStation := map[string]*edgeReading{}
	stations := append([]string{}, h.stations...)
	for _, e := range edges {
		if e == nil {
			continue
		}
		byStation[e.station] = e
		if _, known := h.claims[e.station]; !known {
			stations = append(stations, e.station)
		}
	}
	sort.Strings(stations)
	var out []placementSplit
	for _, s := range stations {
		p := placementSplit{station: s, ownedBy: "core-mirror", unownedByClass: map[string]int{},
			ownedByClass: map[string]int{}, logNotFound: -1, logBound: -1}
		owns := h.claims[s]
		if e := byStation[s]; e != nil {
			p.ownedBy, owns = "edge-db", e.owned
			if e.logRead {
				p.logNotFound, p.logBound = e.logNotFound, e.logBound
			}
		}
		for _, c := range h.placements {
			if owns[c.node] {
				p.owned += c.n
				p.ownedByClass[c.class] += c.n
			} else {
				p.unowned += c.n
				p.unownedByClass[c.class] += c.n
			}
		}
		out = append(out, p)
	}
	return out
}

// classText is "SLN 200, SMN 100" in descending count.
func classText(m map[string]int) string {
	if len(m) == 0 {
		return "-"
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if m[keys[i]] != m[keys[j]] {
			return m[keys[i]] > m[keys[j]]
		}
		return keys[i] < keys[j]
	})
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%s %d", k, m[k])
	}
	return strings.Join(parts, ", ")
}

func sumCounts(cs []nodeCount) int {
	n := 0
	for _, c := range cs {
		n += c.n
	}
	return n
}

// nodeText is the first few nodes of a per-node count, "NODE n".
func nodeText(cs []nodeCount, limit int) string {
	var parts []string
	for i, c := range cs {
		if i == limit {
			parts = append(parts, fmt.Sprintf("+%d more", len(cs)-limit))
			break
		}
		parts = append(parts, fmt.Sprintf("%s %d", c.node, c.n))
	}
	if len(parts) == 0 {
		return "none"
	}
	return strings.Join(parts, ", ")
}

// printHandoff writes the section.
func printHandoff(h *handoffCore, edges []*edgeReading, logs *logStats, unrun bool) {
	fmt.Printf("\n%s", measureHandoff)
	if unrun || h == nil {
		fmt.Println("  FAILED-TO-RUN")
		return
	}
	fmt.Println("  the carrier-handoff before-numbers (class = name prefix, /node type where Core has one)")
	fmt.Printf("        a. placements (Bound adjustments, each broadcast to every Edge): %d\n", sumCounts(h.placements))
	fmt.Printf("           %-14s %-11s %7s %8s  %s\n", "station", "owns-by", "owned", "unowned", "unowned by class")
	for _, p := range splitPlacements(h, edges) {
		fmt.Printf("           %-14s %-11s %7d %8d  %s\n", p.station, p.ownedBy, p.owned, p.unowned, classText(p.unownedByClass))
		if p.logNotFound >= 0 {
			fmt.Printf("           %-14s edge log: %d `process node not found` (every adjustment kind), %d bound\n",
				"", p.logNotFound, p.logBound)
		}
	}
	staleN, staleU := 0, 0
	var perStation []string
	for _, s := range h.stale {
		staleN += s.n
		staleU += s.units
		perStation = append(perStation, fmt.Sprintf("%s %d", s.station, s.n))
	}
	fmt.Printf("        b. stale-epoch drops: %d (%d units)", staleN, staleU)
	if len(perStation) > 0 {
		fmt.Printf("  by station: %s", strings.Join(perStation, ", "))
	}
	fmt.Println()
	relN, relChanged := 0, 0
	var perOp []string
	for _, r := range h.releases {
		relN += r.n
		relChanged += r.changed
		perOp = append(perOp, fmt.Sprintf("%s %d/%d", r.op, r.changed, r.n))
	}
	fmt.Printf("        c. releases with after_uop <> before_uop: %d of %d", relChanged, relN)
	if len(perOp) > 0 {
		fmt.Printf("  (%s)", strings.Join(perOp, ", "))
	}
	fmt.Println()
	transit := 0
	for _, c := range h.refreshes {
		if c.node == "_TRANSIT" {
			transit += c.n
		}
	}
	fmt.Printf("        d. epoch refreshes: %d, to _TRANSIT %d  (%s)\n", sumCounts(h.refreshes), transit, nodeText(h.refreshes, 8))
	fmt.Printf("        e. labelled carriers at 0 (or below) at rest: %d  (%s)\n", sumCounts(h.zeroAtRest), nodeText(h.zeroAtRest, 8))
	a := h.after
	fmt.Printf("        f. consume ticks after a release: %d of %d releases followed, %d bin(s), %d ticks (%d units), max %d per release\n",
		a.followed, a.releases, a.bins, a.ticks, a.units, a.maxPer)
	if len(a.byStation) > 0 {
		fmt.Printf("           by counting station: %s\n", classText(a.byStation))
	}
	printLastArm(logs)
	printHeldTicks(edges)
}

// printHandoffSections prints [S.4] and, when an Edge was named, [S.5] and [S.6].
func (r *report) printHandoffSections() {
	printHandoff(r.handoff, r.edges, r.logCounts, r.unrunMeasures[measureHandoff])
	printEdgeStarvation(r.edges, r.pulls, r.unrunMeasures[measureEdgeStarve])
	printEdgeWork(os.Stdout, r.edges, r.orders.completed)
}

// printHeldTicks is item g: Edge-only, and only its end state exists.
func printHeldTicks(edges []*edgeReading) {
	if len(edges) == 0 {
		fmt.Println("        g. ticks held and replayed: n/a (no Edge DB/log given)")
		return
	}
	fmt.Println("        g. ticks held and replayed: not recorded over the run (no table, no log line on")
	fmt.Println("           either path); held at the collect (pending_uop_delta):")
	for _, e := range edges {
		if e == nil {
			continue
		}
		fmt.Printf("           %-14s %d unit(s)  (%s)\n", e.station, sumCounts(e.pending), nodeText(e.pending, 8))
	}
}

// printLastArm is item h: from the log only.
func printLastArm(logs *logStats) {
	if logs == nil {
		fmt.Println("        h. landings sent by the last arm (no claiming station, no order station): n/a (no -log)")
		return
	}
	fmt.Printf("        h. landings sent by the last arm (no claiming station, no order station): %d (%d outbox row(s))"+
		"   <- expected about 0 a run\n", logs.lastArmLandings, logs.lastArmRows)
}
