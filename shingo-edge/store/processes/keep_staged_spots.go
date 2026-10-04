package processes

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"shingo/protocol"
	"shingoedge/domain"
)

// KeptSpot is a keep-staged claim's spot as it stood before a write: the
// claim by (style, line node) and the node its spare stands on. A door hands
// the spots it may have moved or cleared to CheckKeepStagedSpots.
type KeptSpot struct {
	StyleID int64
	Line    string
	Spot    string
	// Source is the claim's inbound source: where its spare came from, and so
	// where a spare left behind by a cleared or moved spot goes back to.
	Source string
}

// CheckKeepStagedSpots is the dedicated-spot rule, run ONCE at the end of a
// write transaction that touches claims, after its last write and before its
// commit.
//
// WHY AT THE END AND NOT INSIDE UpsertClaim. The rule is about pairs of rows,
// and a transaction passes through states no one asked for: the composer
// upserts every cell before it deletes the removed ones, so a spot moved from
// one cell to another in a single save is, for a moment, named by both. A
// check per upsert would refuse that save; a check over the finished table
// sees what the transaction actually leaves.
//
// THE RULE. A keep-staged claim's spot is its keep_staged_node. The spare on
// it is what the swap fetches and the refills deliver to it, so nothing else
// may put a bin on that node or take one off it:
//   - another claim in the same style, or in a style of another process, may
//     not name it in any routing column (spotColumns);
//   - another style of the SAME process may name it as staging or as its own
//     spot only (inbound_staging, outbound_staging, keep_staged_node): styles
//     of one process never run at once, so the spot can serve each in turn,
//     but a line position has a process-node row whose runtime an admin move
//     can bind, and that would put a line's bin on it;
//   - the claim's own routes may not name it, except its inbound_staging: the
//     same node as both is a robot waiting under the spare it is about to lift.
//
// MOVING OR CLEARING A SPOT is refused while orders still deliver to the old
// one: before lists the spots the transaction may have changed, each is
// compared with the finished table, and only when one moved or cleared does
// the check read orders, once, for all of them.
//
// One SELECT over every live claim. The tables are small, and one statement is
// what a store pinned to one connection can afford on a save.
//
// PER EDGE. An Edge reads only its own claims, and Core's mirror carries
// neither keep_staged_node nor every routing column, so two Edges naming one
// node are not caught here; the error text says so.
//
// It returns the spots of before that the write moved or cleared, so the door
// can hand them on once the transaction commits: a spare left standing on a
// spot nothing keeps any more goes back to where it came from
// (Engine.spotsCleared).
func CheckKeepStagedSpots(tx DBTX, before []KeptSpot) ([]KeptSpot, error) {
	rows, err := readSpotClaims(tx)
	if err != nil {
		return nil, fmt.Errorf("keep_staged_node spot check: %w", err)
	}
	if err := spotConflict(rows); err != nil {
		return nil, err
	}
	return refuseMovingABusySpot(tx, rows, before)
}

// spotColumns are the claim columns through which a claim puts a bin on a node
// or takes one off it, in the order an error reports them.
var spotColumns = []string{
	"core_node_name", "inbound_staging", "outbound_staging", "paired_core_node",
	"second_paired_core_node", "inbound_source", "outbound_destination",
	"containment_destination", "changeover_evac_destination", "changeover_evac_nodes", "keep_staged_node",
}

// spotClaim is one live claim as the check reads it: who it is and every node
// it names, column by column.
type spotClaim struct {
	processKey int64 // the style's process; a style with none is its own
	styleID    int64
	style      string
	line       string
	names      map[string][]string // spot column -> nodes it names
}

func (c spotClaim) label() string { return fmt.Sprintf("style %q line %s", c.style, c.line) }

func (c spotClaim) spot() string {
	return firstOf(c.names["keep_staged_node"])
}

func firstOf(s []string) string {
	if len(s) == 0 {
		return ""
	}
	return s[0]
}

// The one SELECT. Retired claims are left out: nothing revives one. Retired
// STYLES are kept in: RestoreStyle brings a style back without touching its
// claims, so its spots must stay counted while it is away.
const spotClaimsSQL = `SELECT s.process_id, c.style_id, s.name, c.core_node_name,
	c.inbound_staging, c.outbound_staging, c.paired_core_node, c.second_paired_core_node,
	c.inbound_source, c.outbound_destination, c.containment_destination,
	c.changeover_evac_destination, c.changeover_evac_nodes, c.keep_staged_node
	FROM style_node_claims c JOIN styles s ON s.id = c.style_id
	WHERE c.retired_at IS NULL
	ORDER BY c.id`

func readSpotClaims(db DBTX) ([]spotClaim, error) {
	rs, err := db.Query(spotClaimsSQL)
	if err != nil {
		return nil, err
	}
	defer rs.Close()
	var out []spotClaim
	for rs.Next() {
		var c spotClaim
		var process sql.NullInt64
		vals := make([]string, len(spotColumns))
		if err := rs.Scan(&process, &c.styleID, &c.style, &vals[0],
			&vals[1], &vals[2], &vals[3], &vals[4], &vals[5], &vals[6], &vals[7], &vals[8], &vals[9], &vals[10]); err != nil {
			return nil, err
		}
		c.processKey = process.Int64
		if !process.Valid {
			c.processKey = -c.styleID
		}
		c.line = vals[0]
		c.names = make(map[string][]string, len(spotColumns))
		for i, col := range spotColumns {
			if col == "changeover_evac_nodes" {
				// A JSON array, or '' for none (marshalEvacNodes).
				var nodes []string
				_ = json.Unmarshal([]byte(vals[i]), &nodes)
				c.names[col] = nodes
				continue
			}
			if vals[i] != "" {
				c.names[col] = []string{vals[i]}
			}
		}
		out = append(out, c)
	}
	return out, rs.Err()
}

// spotConflict applies the rule to the finished table and reports every
// conflict it finds, so one refused save names them all.
func spotConflict(rows []spotClaim) error {
	var errs []error
	for i, k := range rows {
		spot := k.spot()
		if spot == "" {
			continue
		}
		for _, col := range spotColumns {
			if col == "inbound_staging" || col == "keep_staged_node" {
				continue
			}
			for _, n := range k.names[col] {
				if n == spot {
					errs = append(errs, fmt.Errorf("%w: %s keeps its spare at %s and also names it as %s; "+
						"a kept spare's spot cannot be one of its own line's routes other than its inbound staging",
						domain.ErrKeepStagedSpot, k.label(), spot, col))
				}
			}
		}
		for j, c := range rows {
			if i == j {
				continue
			}
			sameProcessOtherStyle := c.processKey == k.processKey && c.styleID != k.styleID
			for _, col := range spotColumns {
				if sameProcessOtherStyle && (col == "inbound_staging" || col == "outbound_staging" || col == "keep_staged_node") {
					continue
				}
				// Two kept spots on one node are one conflict, reported once.
				if col == "keep_staged_node" && j < i && c.spot() == spot {
					continue
				}
				for _, n := range c.names[col] {
					if n != spot {
						continue
					}
					where := "in the same style"
					switch {
					case sameProcessOtherStyle:
						where = "in another style of the same process, which may reuse it as staging only"
					case c.processKey != k.processKey:
						where = "in another process"
					}
					errs = append(errs, fmt.Errorf("%w: node %s is the kept spot (keep_staged_node) of %s, "+
						"and %s names it as %s, %s. Checked across this Edge's claims only",
						domain.ErrKeepStagedSpot, spot, k.label(), c.label(), col, where))
				}
			}
		}
	}
	return errors.Join(errs...)
}

// refuseMovingABusySpot refuses a transaction that moved or cleared a spot
// (keep_staged_node changed or cleared, or the claim removed)
// while a non-terminal order still delivers to the old spot: that order is a
// refill for a spare the line no longer keeps there. The orders read runs only
// when a spot did move.
func refuseMovingABusySpot(db DBTX, rows []spotClaim, before []KeptSpot) ([]KeptSpot, error) {
	if len(before) == 0 {
		return nil, nil
	}
	type key struct {
		style int64
		line  string
	}
	now := make(map[key]string, len(rows))
	styleNames := map[int64]string{}
	for _, c := range rows {
		styleNames[c.styleID] = c.style
		if s := c.spot(); s != "" {
			now[key{c.styleID, c.line}] = s
		}
	}
	var moved []KeptSpot
	seen := map[string]bool{}
	for _, b := range before {
		if b.Spot == "" || now[key{b.StyleID, b.Line}] == b.Spot || seen[b.Spot] {
			continue
		}
		seen[b.Spot] = true
		moved = append(moved, b)
	}
	if len(moved) == 0 {
		return nil, nil
	}
	args := make([]any, len(moved))
	for i, m := range moved {
		args[i] = m.Spot
	}
	rs, err := db.Query(fmt.Sprintf(`SELECT delivery_node, COUNT(*) FROM orders
		WHERE delivery_node IN (%s) AND status NOT IN (%s) GROUP BY delivery_node`,
		strings.TrimSuffix(strings.Repeat("?,", len(moved)), ","), protocol.TerminalStatusSQLList()), args...)
	if err != nil {
		return nil, fmt.Errorf("keep_staged_node spot check: open orders: %w", err)
	}
	defer rs.Close()
	open := map[string]int{}
	for rs.Next() {
		var node string
		var n int
		if err := rs.Scan(&node, &n); err != nil {
			return nil, err
		}
		open[node] = n
	}
	if err := rs.Err(); err != nil {
		return nil, err
	}
	var errs []error
	for _, m := range moved {
		if n := open[m.Spot]; n > 0 {
			style := styleNames[m.StyleID]
			if style == "" {
				style = fmt.Sprintf("#%d", m.StyleID)
			}
			errs = append(errs, fmt.Errorf("%w: style %q line %s kept its spare at %s and %d open order(s) "+
				"still deliver there; let them finish or cancel them before moving or clearing the spot",
				domain.ErrKeepStagedSpot, style, m.Line, m.Spot, n))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return nil, err
	}
	return moved, nil
}
