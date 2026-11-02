package bins

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
	"shingocore/store/internal/nodetree"
	"shingocore/store/reservations"
)

// kanban_reads.go — the two empties reads the keeper's tick and a snapshot build
// make for many kanbans at once, each composed of the fragments the single reads
// are: the census beside CountEmptyOfTypeInGroup, and the forecast beside the
// finder. Neither has a WHERE of its own.

// KanbanResidentsOfTypeSQL is the residents of one carrier type, as a fragment a door's one read
// composes: per group of the walk the caller's
// statement names (kanban_tree(kanban, id)), the stock of the type standing there
// and the carts at 0. typeExpr is the expression carrying the type's id. The finder's WHERE.
func KanbanResidentsOfTypeSQL(typeExpr string) string {
	return `SELECT kt.kanban AS kanban,
			COUNT(*) FILTER (WHERE ` + noHeightSaidSQL() + `) AS stock,
			COUNT(*) FILTER (WHERE NOT (` + noHeightSaidSQL() + `)) AS at_zero ` + BinFromClause + `
		JOIN kanban_tree kt ON kt.id = b.node_id` + emptyCarrierWhere("NULL", "kt.kanban") + `
		  AND bt.id = ` + typeExpr + `
		GROUP BY kt.kanban`
}

// SourceableCounts is the keeper's forecast for one short kanban: how many empties of each of
// codes a plant-wide find would answer with, in one statement, the finder's own WHERE, fence,
// sources and dig arm for an ask that holds nothing (FindEmptyOfTypes with no destination, no
// height said, reservations.Anyone). A carrier it would find buried counts: a dig reaches it.
// A kanban giving only what stands above its pool counts no more than that. Pins:
// TestKanbanForecast_AgreesWithTheFinder,
// TestKanbanKeeper_MintsNoAskForATypeNothingOutsideCanSource.
func SourceableCounts(db *sql.DB, codes []string, fence EmptyFence) (map[string]int, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	a := &emptyQueryArgs{}
	destP := a.add(int64(0))
	where := EmptyCarrierWhereFor(destP) + OfTypesArm(a.add(codes)) + ExcludeNodeArm(destP)
	cte, arms, _ := fence.fenceArms(a)
	where += arms + NotForeignDugArm(a.add(string(reservations.ModeDig)),
		a.add(reservations.Anyone.OrderID), a.add(reservations.Anyone.LaneOwner)) + HeightArm(domain.Height{})
	above := `NULL::bigint AS above`
	join := ``
	if cte != "" {
		above = `MAX(ap.above) AS above`
		join = ` LEFT JOIN nodes an ON an.id = b.node_id
		 LEFT JOIN above_pool ap ON ap.kanban = an.parent_id AND ap.family_id = bt.family_id`
	}
	rows, err := db.Query(cte+`SELECT bt.code, COALESCE(n.parent_id, 0), COUNT(*), `+above+` `+BinFromClause+join+where+`
		GROUP BY bt.code, n.parent_id`, a.vals...)
	if err != nil {
		return nil, fmt.Errorf("sourceable counts %v: %w", codes, err)
	}
	defer rows.Close()
	out := make(map[string]int, len(codes))
	for rows.Next() {
		var (
			code   string
			parent int64
			n      int
			limit  sql.NullInt64
		)
		if err := rows.Scan(&code, &parent, &n, &limit); err != nil {
			return nil, fmt.Errorf("scan sourceable count: %w", err)
		}
		if limit.Valid && int(limit.Int64) < n {
			n = int(limit.Int64)
		}
		out[code] += n
	}
	return out, rows.Err()
}

// EffectiveTypesCTE names a recursive statement's CTE (child, at): for each node the where
// clause picks (alias n), the node whose rows are its list. A child with no row takes anything.
func EffectiveTypesCTE(name, where string) string {
	return name + `_up(child, id, parent_id, depth) AS (
			SELECT n.id, n.id, n.parent_id, 0 FROM nodes n WHERE ` + where + `
			UNION ALL
			SELECT u.child, a.id, a.parent_id, u.depth + 1 FROM nodes a JOIN ` + name + `_up u ON a.id = u.parent_id
		),
		` + name + ` AS (
			SELECT DISTINCT ON (u.child) u.child, u.id AS at
			  FROM ` + name + `_up u
			  LEFT JOIN node_properties m ON m.node_id = u.child AND m.key = 'bin_type_mode'
			 WHERE COALESCE(m.value, '') <> 'all'
			   AND (u.depth = 0 OR COALESCE(m.value, '') <> 'specific')
			   AND EXISTS (SELECT 1 FROM node_bin_types t WHERE t.node_id = u.id)
			 ORDER BY u.child, u.depth
		)`
}

// EffectiveTypesOfChildren is GetEffectiveBinTypes for every child of a group, in
// one statement: a child with no entry takes anything. Its mode decides as the
// single read's does: "all" nothing, "specific" its own rows, anything else the
// nearest of itself and its ancestors with rows. Pin:
// TestEffectiveTypesOfChildren_AgreesWithTheSingleRead.
func EffectiveTypesOfChildren(db *sql.DB, groupNodeID int64) (map[int64][]*BinType, error) {
	rows, err := db.Query(`WITH RECURSIVE `+EffectiveTypesCTE("source", "n.parent_id = $1")+`
		SELECT s.child, bt.id, bt.code, bt.description, bt.width_in, bt.height_in, bt.length_in, bt.family_id,
		       (SELECT f.code FROM carrier_families f WHERE f.id = bt.family_id),
		       bt.required_robot_group, bt.bare, bt.bare_of, bt.totes_per_layer, bt.created_at, bt.updated_at
		  FROM source s
		  JOIN node_bin_types t ON t.node_id = s.at
		  JOIN bin_types bt ON bt.id = t.bin_type_id
		 ORDER BY s.child, bt.code`, groupNodeID)
	if err != nil {
		return nil, fmt.Errorf("effective types of the children of %d: %w", groupNodeID, err)
	}
	defer rows.Close()
	out := map[int64][]*BinType{}
	for rows.Next() {
		var child int64
		bt, err := ScanBinType(childScan{rows, &child})
		if err != nil {
			return nil, fmt.Errorf("scan effective type: %w", err)
		}
		out[child] = append(out[child], bt)
	}
	return out, rows.Err()
}

// childScan is a row whose first column is the child, ahead of a bin type's.
type childScan struct {
	rows  *sql.Rows
	child *int64
}

func (c childScan) Scan(dest ...any) error {
	return c.rows.Scan(append([]any{c.child}, dest...)...)
}

// ListKanbanSurplus is the population the surplus arm moves from: the empties of the
// named types standing in a kanban that its count calls residents, oldest arrival first:
// the last lift off any node, else the row's birth. updated_at is no arrival, any UPDATE moves
// it.
//
// THE COUNT'S OWN WHERE, SO THE ARM CANNOT MOVE WHAT THE KEEPER DOES NOT COUNT:
// emptyCarrierWhere refuses a claimed, locked, reserved, staged, loaded, bare or
// quality-held cart, and a cart on a disabled node. HeightArm's no-height-said arm
// leaves tops & bottoms out, which is no stock of a level. A cart that met any of them
// is not read, so no plan is made for it: there is no refusal to handle. Pin:
// TestKanbanSurplus_NeverMovesWhatTheCountRefuses.
func ListKanbanSurplus(db *sql.DB, kanbanID int64, codes []string) ([]*Bin, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	rows, err := db.Query(nodetree.DescendantsOf(1)+" "+BinJoinQuery+emptyCarrierWhere("NULL", "$1")+`
		  AND b.node_id IN (SELECT id FROM descendants)`+OfTypesArm(2)+HeightArm(domain.Height{})+`
		ORDER BY COALESCE(b.last_lift_at, b.created_at), b.id`, kanbanID, codes)
	if err != nil {
		return nil, fmt.Errorf("list kanban surplus %d: %w", kanbanID, err)
	}
	defer rows.Close()
	return scanBins(rows)
}

// EmptyCarrierWhereForKanban is emptyCarrierWhere with no destination and groupExpr the
// kanban a pulled-directly stage 2's windows stand in: the WHERE of "this cart is an empty
// standing in the kanban" (store.DB.ListKanbanLoop).
func EmptyCarrierWhereForKanban(groupExpr string) string {
	return emptyCarrierWhere("NULL", groupExpr)
}

// NoHeightSaidSQL says the cart at b, bt is at no height said: anything but tops & bottoms.
func NoHeightSaidSQL() string { return noHeightSaidSQL() }
