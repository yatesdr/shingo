package bins

import (
	"database/sql"
	"fmt"

	"shingocore/domain"
	"shingocore/store/reservations"
)

// kanban_reads.go — the two empties reads the keeper's tick and a snapshot build
// make for many kanbans at once, each composed of the fragments the single reads
// are: the census beside CountEmptyOfTypeInGroup, and the forecast beside the
// finder. Neither has a WHERE of its own.

// ResidentCount is one bucket of ListKanbanResidents: how many empties of a type
// stand in a kanban with this many totes on (nil: no number), and whether they are
// the keeper's stock (Stock: tops & bottoms is not).
type ResidentCount struct {
	Kanban int64
	Code   string
	Totes  *int
	Stock  bool
	N      int
}

// ListKanbanResidents reads every empty standing in the named kanbans (nil: all),
// in one descendants walk: what CountEmptyOfTypeInGroup counts per (kanban, type)
// at no height said is the buckets with Stock, and what the heights census
// (store.DB.CountEmptyBinsOfTypeInGroupByTotes) counts is the buckets with a
// number. The same WHERE: emptyCarrierWhere with the kanban as the group a
// pulled-directly stage 2's windows stand in, and no destination, as the count
// passes one. EmptyOfTypeInGroupWhere's group arm is the walk's join: its other
// half is a destination's cleared home, and the count has no destination. Pin:
// TestKanbanCounts_TheBatchedReadsAgreeWithTheSingleReads.
func ListKanbanResidents(db *sql.DB, kanbanIDs []int64) ([]ResidentCount, error) {
	var arg any
	if kanbanIDs != nil {
		arg = kanbanIDs
	}
	rows, err := db.Query(`WITH RECURSIVE kanban_tree(kanban, id) AS (
			SELECT k.node_id, c.id FROM kanbans k JOIN nodes c ON c.parent_id = k.node_id
			 WHERE $1::bigint[] IS NULL OR k.node_id = ANY($1::bigint[])
			UNION ALL
			SELECT t.kanban, n2.id FROM nodes n2 JOIN kanban_tree t ON n2.parent_id = t.id
		)
		SELECT kt.kanban, bt.code, b.totes_on_cart, `+noHeightSaidSQL()+`, COUNT(*) `+BinFromClause+`
		JOIN kanban_tree kt ON kt.id = b.node_id`+emptyCarrierWhere("NULL", "kt.kanban")+`
		GROUP BY kt.kanban, bt.code, b.totes_on_cart, `+noHeightSaidSQL(), arg)
	if err != nil {
		return nil, fmt.Errorf("list kanban residents: %w", err)
	}
	defer rows.Close()
	var out []ResidentCount
	for rows.Next() {
		var r ResidentCount
		if err := rows.Scan(&r.Kanban, &r.Code, &r.Totes, &r.Stock, &r.N); err != nil {
			return nil, fmt.Errorf("scan kanban resident: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SourceableTypes is the keeper's forecast for one short kanban: which of codes
// a plant-wide find would answer with something, in one statement, the finder's
// own WHERE, fence, sources and dig arm for an ask that holds nothing
// (FindEmptyOfTypes with no destination, no height said, reservations.Anyone). A
// carrier it would find buried counts: a dig reaches it. Pins:
// TestKanbanForecast_AgreesWithTheFinder,
// TestMaintainer_MintsNoAskForATypeNothingOutsideCanSource.
func SourceableTypes(db *sql.DB, codes []string, fence EmptyFence) (map[string]bool, error) {
	if len(codes) == 0 {
		return nil, nil
	}
	a := &emptyQueryArgs{}
	destP := a.add(int64(0))
	where := EmptyCarrierWhereFor(destP) + OfTypesArm(a.add(codes)) + ExcludeNodeArm(destP)
	cte, arms, _ := fence.fenceArms(a)
	where += arms + NotForeignDugArm(a.add(string(reservations.ModeDig)),
		a.add(reservations.Anyone.OrderID), a.add(reservations.Anyone.LaneOwner)) + HeightArm(domain.Height{})
	rows, err := db.Query(cte+`SELECT bt.code `+BinFromClause+where+`
		GROUP BY bt.code`, a.vals...)
	if err != nil {
		return nil, fmt.Errorf("sourceable types %v: %w", codes, err)
	}
	defer rows.Close()
	out := make(map[string]bool, len(codes))
	for rows.Next() {
		var code string
		if err := rows.Scan(&code); err != nil {
			return nil, fmt.Errorf("scan sourceable type: %w", err)
		}
		out[code] = true
	}
	return out, rows.Err()
}

// EffectiveTypesOfChildren is GetEffectiveBinTypes for every child of a group, in
// one statement: a child with no entry takes anything. Its mode decides as the
// single read's does: "all" nothing, "specific" its own rows, anything else the
// nearest of itself and its ancestors with rows. Pin:
// TestEffectiveTypesOfChildren_AgreesWithTheSingleRead.
func EffectiveTypesOfChildren(db *sql.DB, groupNodeID int64) (map[int64][]*BinType, error) {
	rows, err := db.Query(`WITH RECURSIVE up(child, id, parent_id, depth) AS (
			SELECT n.id, n.id, n.parent_id, 0 FROM nodes n WHERE n.parent_id = $1
			UNION ALL
			SELECT u.child, a.id, a.parent_id, u.depth + 1 FROM nodes a JOIN up u ON a.id = u.parent_id
		),
		source AS (
			SELECT DISTINCT ON (u.child) u.child, u.id AS at
			  FROM up u
			  LEFT JOIN node_properties m ON m.node_id = u.child AND m.key = 'bin_type_mode'
			 WHERE COALESCE(m.value, '') <> 'all'
			   AND (u.depth = 0 OR COALESCE(m.value, '') <> 'specific')
			   AND EXISTS (SELECT 1 FROM node_bin_types t WHERE t.node_id = u.id)
			 ORDER BY u.child, u.depth
		)
		SELECT s.child, bt.id, bt.code, bt.description, bt.width_in, bt.height_in, bt.length_in,
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
