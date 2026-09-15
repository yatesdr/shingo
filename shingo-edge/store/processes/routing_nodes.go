// routing_nodes.go — the process ROUTING SET (process_routing_nodes) inside
// the processes aggregate: the nodes a process may route material through
// that are not its own positions. See domain/routing_set.go for what the set
// is and why it is a table.

package processes

import (
	"database/sql"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"

	"shingoedge/domain"
	"shingoedge/store/internal/helpers"
)

// RoutingNode, RoutingNodeInput and RoutingDeriveReport are the routing-set
// data types; the structs live in shingoedge/domain.
type (
	RoutingNode         = domain.RoutingNode
	RoutingNodeInput    = domain.RoutingNodeInput
	RoutingDeriveReport = domain.RoutingDeriveReport
)

// ErrInvalidRoutingRole refuses a role outside source / staging / destination.
// 'waypoint' in particular is not a role: key routes are validated against
// the map, not against this list.
var ErrInvalidRoutingRole = errors.New("routing role must be source, staging or destination")

// ErrRoutingNodeIsPosition refuses a routing row for a node that is one of the
// process's own positions. Positions stay in process_nodes; the routing
// picture is the union of the two, and a node in both halves would be
// offered twice.
var ErrRoutingNodeIsPosition = errors.New("is a position of this process; positions stay in process_nodes")

// ErrRoutingNodeInUse refuses a delete while a live style's claim still names
// the node in the matching field. The error names the style so the engineer
// knows which claim to look at.
var ErrRoutingNodeInUse = errors.New("a live claim still routes through this node")

// ErrRoutingNodeNotFound: no such row in this process.
var ErrRoutingNodeNotFound = errors.New("routing node not found in this process")

const routingSelect = `id, process_id, core_node_name, role, label, sequence, enabled, origin, called_by, created_at`

// routingRoleOrder groups a list source / staging / destination — the order
// material moves, and the order the desktop shows them.
const routingRoleOrder = `CASE role WHEN 'source' THEN 0 WHEN 'staging' THEN 1 ELSE 2 END`

func scanRoutingNode(scanner interface{ Scan(...any) error }) (RoutingNode, error) {
	var r RoutingNode
	var createdAt string
	if err := scanner.Scan(&r.ID, &r.ProcessID, &r.CoreNodeName, &r.Role, &r.Label, &r.Sequence,
		&r.Enabled, &r.Origin, &r.CalledBy, &createdAt); err != nil {
		return r, err
	}
	r.CreatedAt = helpers.ScanTime(createdAt)
	return r, nil
}

// ListRoutingNodes returns a process's routing set, grouped by role in
// source / staging / destination order, then by sequence and name.
//
// NO STYLE COUNTS. This read is on the hot path — the composer block, the
// preview, the save — and the count is read by exactly one screen. It used to
// carry a correlated COUNT(DISTINCT) over every claim of the process, with
// TRIM() on both sides of every comparison, which no index can serve: 1.23 ms
// at 40 styles and 2.65 ms at 90, on every poll of every station, for a number
// the station discards. ListRoutingNodesWithCounts is the panel's read.
func ListRoutingNodes(db DBTX, processID int64) ([]RoutingNode, error) {
	rows, err := db.Query(`SELECT `+routingSelect+` FROM process_routing_nodes
		WHERE process_id = ? ORDER BY `+routingRoleOrder+`, sequence, core_node_name`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoutingNode
	for rows.Next() {
		r, err := scanRoutingNode(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListRoutingNodesWithCounts is ListRoutingNodes with StyleCount filled: how
// many of the process's LIVE styles name each node in THAT row's role — the
// evidence the Routing panel puts under a backfilled name.
//
// THE COUNT IS COMPUTED IN GO, from the claims this reads once, because the
// SQL form of it is unindexable. domain.CountRoutingStyles holds the matching
// rule so the count and the delete guard cannot answer differently.
//
// PER ROLE, not per name: a node that is a live source may be nobody's
// destination, and one count over every field would put "on 10 styles" beside
// a destination row nothing sends to.
func ListRoutingNodesWithCounts(db *sql.DB, processID int64) ([]RoutingNode, error) {
	rows, err := ListRoutingNodes(db, processID)
	if err != nil {
		return nil, err
	}
	claims, err := ListLiveClaimsByProcess(db, processID)
	if err != nil {
		return nil, err
	}
	domain.CountRoutingStyles(rows, claims)
	return rows, nil
}

// RoutingSetNames is the set of nodes a process may route through: its live
// positions plus EVERY member of its routing set, adopted or not. It is the
// universe a flow preset is validated against.
//
// ADOPTED OR NOT, by owner ruling R4 (2026-09-12). The `enabled` switch is the
// engineer filtering what OPERATORS see as options on the HMI; it says nothing
// about what a flow may name. With the filter here, a press whose routing set
// had been backfilled but not adopted could RUN a flow — `flow/save` validates
// against the process's nodes and the scene, not against this — and could not
// NAME it: `Save as preset…` and `Name it…` answered
// `outbound_destination "Supermarket Area" not a position or a routing-set
// member of this process` on a flow doing its job. Two validations disagreeing
// about what a valid flow is is how a screen refuses the thing it is for.
func RoutingSetNames(db *sql.DB, processID int64) (map[string]bool, error) {
	rows, err := db.Query(`SELECT core_node_name FROM process_nodes WHERE process_id = ?1 AND deleted_at IS NULL
		UNION SELECT core_node_name FROM process_routing_nodes WHERE process_id = ?1`, processID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		if name = strings.TrimSpace(name); name != "" {
			out[name] = true
		}
	}
	return out, rows.Err()
}

// isLivePosition reports whether name is one of the process's live
// process_nodes.
func isLivePosition(db *sql.DB, processID int64, name string) (bool, error) {
	var n int
	err := db.QueryRow(`SELECT COUNT(*) FROM process_nodes
		WHERE process_id = ? AND core_node_name = ? AND deleted_at IS NULL`, processID, name).Scan(&n)
	return n > 0, err
}

// UpsertRoutingNode inserts a routing row or updates the one already there
// for the same (process, node, role). A second role for the same node is a
// second row — a buffer can be a source and a destination. Returns the row id.
//
// Origin defaults to engineer: the backfill is the only writer that says
// otherwise, and it does not come through here.
func UpsertRoutingNode(db *sql.DB, in RoutingNodeInput) (int64, error) {
	in.CoreNodeName = strings.TrimSpace(in.CoreNodeName)
	if in.CoreNodeName == "" {
		return 0, fmt.Errorf("routing node name is required")
	}
	if !domain.IsRoutingRole(in.Role) {
		return 0, fmt.Errorf("%w: %q", ErrInvalidRoutingRole, in.Role)
	}
	if in.Origin == "" {
		in.Origin = domain.RoutingOriginEngineer
	}
	if in.Origin != domain.RoutingOriginEngineer && in.Origin != domain.RoutingOriginBackfill {
		return 0, fmt.Errorf("routing origin must be engineer or backfill, got %q", in.Origin)
	}
	if pos, err := isLivePosition(db, in.ProcessID, in.CoreNodeName); err != nil {
		return 0, err
	} else if pos {
		return 0, fmt.Errorf("%s %w", in.CoreNodeName, ErrRoutingNodeIsPosition)
	}
	if _, err := db.Exec(`INSERT INTO process_routing_nodes
		(process_id, core_node_name, role, label, sequence, enabled, origin, called_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(process_id, core_node_name, role) DO UPDATE SET
			label = excluded.label, sequence = excluded.sequence, enabled = excluded.enabled,
			origin = excluded.origin, called_by = excluded.called_by`,
		in.ProcessID, in.CoreNodeName, in.Role, in.Label, in.Sequence, in.Enabled, in.Origin, in.CalledBy); err != nil {
		return 0, err
	}
	var id int64
	err := db.QueryRow(`SELECT id FROM process_routing_nodes WHERE process_id = ? AND core_node_name = ? AND role = ?`,
		in.ProcessID, in.CoreNodeName, in.Role).Scan(&id)
	return id, err
}

// SetRoutingNodeEnabled flips one row's enabled flag.
//
// THE SWITCH IS THE APPROVAL (owner ruling 2026-09-10). A backfilled row is a
// name the derivation read out of a live claim and nobody has agreed to; it
// arrives off, and switching it on is an engineer saying operators may pick
// it. That is a change of AUTHORSHIP and not only of a flag, so the origin
// becomes 'engineer' and called_by records who. It used to keep saying
// backfill, which left the panel unable to tell an approved name from one
// still waiting except by the flag it had just set, and left the summary line
// counting it forever.
//
// SWITCHING ONE OFF DOES NOT FLIP IT BACK. Where a name came from is history,
// and a row an engineer approved and then withdrew is still a row an engineer
// touched. Re-deriving does NOT put a backfill origin back: the backfill is
// INSERT OR IGNORE and skips any row that already exists, so origin is never
// rewritten by it — and that is load-bearing, not an oversight.
// DeriveRoutingNodes runs at every Edge boot (cmd/shingoedge/main.go), so if it
// reset origin, every approval on the box would be silently un-approved on
// every restart, since the switch IS the approval.
//
// Nor does deleting and re-deriving: DeleteRoutingNode refuses while a live
// claim still names the node in the row's role field, and derivation reads
// THAT SAME predicate, so every row the backfill created is a row the delete
// guard must refuse for as long as the claim lives. The two paths are exact
// complements, which is why the dead end is total and not a gap to plug.
//
// UpsertRoutingNode is the one writer that CAN set backfill — its validation
// accepts the value — but its only non-test caller stamps engineer
// unconditionally (www: apiUpsertRoutingNode), so nothing user-reachable
// reaches it. That is a policy line, not a structural block: anyone relaxing
// it should know it is the last door.
//
// The row must belong to processID.
func SetRoutingNodeEnabled(db *sql.DB, processID, id int64, enabled bool, calledBy string) error {
	res, err := db.Exec(`UPDATE process_routing_nodes
		SET enabled = ?, called_by = ?,
		    origin = CASE WHEN ? = 1 THEN ? ELSE origin END
		WHERE id = ? AND process_id = ?`,
		enabled, calledBy, enabled, domain.RoutingOriginEngineer, id, processID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrRoutingNodeNotFound
	}
	return nil
}

// routingFieldMatch is the claim-side predicate for one role: which claim
// fields name a node in that role. The delete guard matches the FIELD, not the
// name — a node that is a live source may still be deleted as a destination.
func routingFieldMatch(role string) string {
	switch role {
	case domain.RoutingRoleSource:
		return `TRIM(c.inbound_source) = ?1`
	case domain.RoutingRoleStaging:
		return `(TRIM(c.inbound_staging) = ?1 OR TRIM(c.outbound_staging) = ?1)`
	default:
		return `(TRIM(c.outbound_destination) = ?1 OR TRIM(c.changeover_evac_destination) = ?1)`
	}
}

// DeleteRoutingNode removes one row — unless a live style's claim in the
// process still names the node in the matching field, in which case it
// refuses with the style's name.
func DeleteRoutingNode(db *sql.DB, processID, id int64) error {
	var name, role string
	switch err := db.QueryRow(`SELECT core_node_name, role FROM process_routing_nodes
		WHERE id = ? AND process_id = ?`, id, processID).Scan(&name, &role); {
	case errors.Is(err, sql.ErrNoRows):
		return ErrRoutingNodeNotFound
	case err != nil:
		return err
	}
	rows, err := db.Query(`SELECT DISTINCT s.name FROM style_node_claims c
		JOIN styles s ON s.id = c.style_id
		WHERE s.process_id = ?2 AND s.deleted_at IS NULL AND c.retired_at IS NULL AND `+routingFieldMatch(role)+`
		ORDER BY s.name`, name, processID)
	if err != nil {
		return err
	}
	defer rows.Close()
	var styles []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return err
		}
		styles = append(styles, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(styles) > 0 {
		return fmt.Errorf("%w: %s is the %s of style %s", ErrRoutingNodeInUse, name, role, strings.Join(styles, ", "))
	}
	_, err = db.Exec(`DELETE FROM process_routing_nodes WHERE id = ? AND process_id = ?`, id, processID)
	return err
}

// routingDeriveStatements are the five INSERT OR IGNOREs of the backfill: one
// per claim field, over the LIVE claims of LIVE styles of the process. A style
// whose process row is gone is never visited (the caller iterates processes),
// a claim whose style row is gone joins to nothing, and a retired claim is
// skipped — both plants carry such rows and none is an error here.
var routingDeriveStatements = []struct{ field, role string }{
	{"inbound_source", domain.RoutingRoleSource},
	{"outbound_destination", domain.RoutingRoleDestination},
	{"changeover_evac_destination", domain.RoutingRoleDestination},
	{"inbound_staging", domain.RoutingRoleStaging},
	{"outbound_staging", domain.RoutingRoleStaging},
}

// DeriveRoutingNodes runs the routing-set backfill for every process and
// returns one report per process, in process-list order. It is the boot-time
// call; the desktop's re-derive is DeriveRoutingNodesForProcess.
//
// isUnknown answers whether Core does not know a name. Nil means the caller
// could not check — at boot, before Core has been heard from — and every
// name then counts as known: absence of data is never a finding.
func DeriveRoutingNodes(db *sql.DB, isUnknown func(name string) bool) ([]RoutingDeriveReport, error) {
	procs, err := List(db)
	if err != nil {
		return nil, fmt.Errorf("routing set backfill: list processes: %w", err)
	}
	out := make([]RoutingDeriveReport, 0, len(procs))
	for _, p := range procs {
		rep, err := deriveRoutingNodes(db, p, isUnknown)
		if err != nil {
			return out, err
		}
		out = append(out, rep)
	}
	return out, nil
}

// DeriveRoutingNodesForProcess is the backfill for one process — the
// desktop's "re-derive". A no-op, reported as Skipped, once the flow composer
// is enabled for the process.
func DeriveRoutingNodesForProcess(db *sql.DB, processID int64, isUnknown func(name string) bool) (RoutingDeriveReport, error) {
	p, err := Get(db, processID)
	if err != nil {
		return RoutingDeriveReport{}, err
	}
	return deriveRoutingNodes(db, *p, isUnknown)
}

func deriveRoutingNodes(db *sql.DB, p Process, isUnknown func(name string) bool) (RoutingDeriveReport, error) {
	if !p.FlowComposerEnabled {
		for _, st := range routingDeriveStatements {
			if _, err := db.Exec(`INSERT OR IGNORE INTO process_routing_nodes
				(process_id, core_node_name, role, origin, enabled)
				SELECT DISTINCT s.process_id, TRIM(c.`+st.field+`), ?, ?, 0
				FROM style_node_claims c JOIN styles s ON s.id = c.style_id
				WHERE s.process_id = ? AND s.deleted_at IS NULL AND c.retired_at IS NULL AND TRIM(c.`+st.field+`) != ''`,
				st.role, domain.RoutingOriginBackfill, p.ID); err != nil {
				return RoutingDeriveReport{}, fmt.Errorf("routing set backfill: process %d %s: %w", p.ID, st.field, err)
			}
		}
		// Positions stay in process_nodes. A staging spot that is also one of
		// the process's own positions (HK P400's PLN_02 / PLN_05) is derived by
		// the statements above and dropped here.
		if _, err := db.Exec(`DELETE FROM process_routing_nodes WHERE process_id = ?1 AND core_node_name IN (
			SELECT core_node_name FROM process_nodes WHERE process_id = ?1 AND deleted_at IS NULL)`, p.ID); err != nil {
			return RoutingDeriveReport{}, fmt.Errorf("routing set backfill: process %d drop positions: %w", p.ID, err)
		}
	}
	rep, err := RoutingSetReport(db, p, isUnknown)
	if err != nil {
		return rep, err
	}
	log.Print(rep.Line())
	return rep, nil
}

// RoutingSetReport counts a process's routing set as it stands: backfilled
// rows, the live claims behind them, and the names Core does not know. It is
// the derive report without the derivation — what the Routing panel shows on
// load.
func RoutingSetReport(db *sql.DB, p Process, isUnknown func(name string) bool) (RoutingDeriveReport, error) {
	rep, _, err := RoutingSet(db, p, isUnknown)
	return rep, err
}

// RoutingSet is the Routing tab's whole answer in one pass: the rows with
// their style counts, and the report that describes them.
//
// THREE READS, AND EVERY ONE FEEDS THE RESPONSE. The route took six. The
// report re-read process_routing_nodes twice — once as COUNT(*) WHERE origin =
// backfill and once as SELECT DISTINCT core_node_name — beside the list the
// response was already carrying, and counted the live claims a third time with
// its own COUNT(*) beside the claims ListRoutingNodesWithCounts reads to fill
// StyleCount. Four facts, three of them already in hand.
//
// The two table re-reads are the same set as ListRoutingNodes (neither
// filtered on anything the list does not), and the claim COUNT's predicate is
// ListLiveClaimsByProcess's exactly — process's live styles, unretired claims.
// So all three are answered from the two reads, in Go.
//
// The Routing tab is a tab: it is opened and re-opened while an engineer works
// through a plant's names, on the one SQLite connection the whole Edge shares.
func RoutingSet(db *sql.DB, p Process, isUnknown func(name string) bool) (RoutingDeriveReport, []RoutingNode, error) {
	rows, err := ListRoutingNodes(db, p.ID)
	if err != nil {
		return RoutingDeriveReport{ProcessID: p.ID, ProcessName: p.Name}, nil, err
	}
	claims, err := ListLiveClaimsByProcess(db, p.ID)
	if err != nil {
		return RoutingDeriveReport{ProcessID: p.ID, ProcessName: p.Name}, nil, err
	}
	// The per-row evidence the Routing panel puts under a backfilled name,
	// from the claims just read — the rule lives in domain so the count and
	// the delete guard cannot answer differently.
	domain.CountRoutingStyles(rows, claims)
	return routingSetReportFrom(p, rows, len(claims), isUnknown), rows, nil
}

// routingSetReportFrom is the report as a pure function of what was read.
func routingSetReportFrom(p Process, rows []RoutingNode, liveClaims int, isUnknown func(name string) bool) RoutingDeriveReport {
	rep := RoutingDeriveReport{
		ProcessID: p.ID, ProcessName: p.Name,
		FlowComposerEnabled: p.FlowComposerEnabled,
		Claims:              liveClaims,
	}
	for _, r := range rows {
		if r.Origin == domain.RoutingOriginBackfill {
			rep.Nodes++
		}
	}
	if isUnknown == nil {
		return rep
	}
	// DISTINCT, ORDER BY core_node_name — what the query said, kept, because
	// two rows for one name (a source and a destination) are one decision.
	seen := make(map[string]bool, len(rows))
	for _, r := range rows {
		if seen[r.CoreNodeName] || !isUnknown(r.CoreNodeName) {
			continue
		}
		seen[r.CoreNodeName] = true
		rep.Unknown = append(rep.Unknown, r.CoreNodeName)
	}
	sort.Strings(rep.Unknown)
	rep.NeedDecision = len(rep.Unknown)
	return rep
}
