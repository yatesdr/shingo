package nodes

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// A LANE IS NEVER WITHOUT A MARK.
//
// A mark is a waiting spot: an RDS map point beside a block of lanes where a
// robot stands while Core decides whether it may enter. Every lane entry goes
// through the splice and the lane oracle (dispatch), and that needs a place to
// hold a robot outside the lane, so a lane with no mark has no lawful way in.
// Rather than refuse each entry at dispatch, the store refuses the state: a lane
// cannot be created, or moved, into a group with no wait points, and a group's
// last wait point cannot be removed while it has lanes. A database written
// before this rule is refused at boot (UnmarkedLanes).

// PropGroupWaitPoints is the node-property key, read on the NODE GROUP, naming
// the map points where robots with work anywhere in that group stand while Core
// decides which of its lanes they may enter. Comma-separated; blanks dropped.
// See dispatch.PropGroupWaitPoints for why the spots belong to the group.
const PropGroupWaitPoints = "group_wait_points"

// PropLaneGatePoint is the legacy per-lane key, read on the LANE node. It still
// marks a lane where a human set it, and is scheduled for deletion; a new lane
// is marked by its group.
const PropLaneGatePoint = "lane_gate_point"

// ParseWaitPoints splits a group's configured wait points, dropping blanks and
// surrounding space. The one parse: the writers validate with it and the
// dispatcher reads with it.
func ParseWaitPoints(raw string) []string {
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ErrLaneUnmarked refuses a lane whose group has no waiting spot.
var ErrLaneUnmarked = errors.New("a lane needs a waiting spot: its group has no wait points")

// laneTypeSQL is the LANE node type's id.
const laneTypeSQL = `(SELECT id FROM node_types WHERE code = 'LANE')`

// requireMarkedGroup refuses n when it is a LANE whose parent group has no wait
// points. A node that is not a lane passes untouched.
func requireMarkedGroup(q interface {
	QueryRow(string, ...any) *sql.Row
}, n *Node) error {
	if n.NodeTypeID == nil {
		return nil
	}
	var isLane bool
	var points string
	if err := q.QueryRow(`SELECT $1::bigint = `+laneTypeSQL+`,
		       COALESCE((SELECT value FROM node_properties WHERE node_id = $2 AND key = $3), '')`,
		*n.NodeTypeID, nullableParent(n), PropGroupWaitPoints).Scan(&isLane, &points); err != nil {
		return fmt.Errorf("check lane %q has a mark: %w", n.Name, err)
	}
	if !isLane || len(ParseWaitPoints(points)) > 0 {
		return nil
	}
	return fmt.Errorf("%w: lane %q (give its group %s first)", ErrLaneUnmarked, n.Name, PropGroupWaitPoints)
}

// requireMarkedParent refuses giving existing node nodeID the parent parentID
// (nil: no parent) when the node is a LANE and that parent has no wait points.
func requireMarkedParent(db *sql.DB, nodeID int64, parentID *int64) error {
	var name string
	var typeID sql.NullInt64
	if err := db.QueryRow(`SELECT name, node_type_id FROM nodes WHERE id = $1`, nodeID).Scan(&name, &typeID); err != nil {
		return fmt.Errorf("check node %d keeps a mark: %w", nodeID, err)
	}
	if !typeID.Valid {
		return nil
	}
	return requireMarkedGroup(db, &Node{Name: name, NodeTypeID: &typeID.Int64, ParentID: parentID})
}

func nullableParent(n *Node) any {
	if n.ParentID == nil {
		return nil
	}
	return *n.ParentID
}

// refuseUnmarkingAGroupWithLanes refuses a write that leaves group nodeID with
// no wait points while it has lanes. value is the points the write leaves
// ("" for a delete).
func refuseUnmarkingAGroupWithLanes(db *sql.DB, nodeID int64, value string) error {
	if len(ParseWaitPoints(value)) > 0 {
		return nil
	}
	var lanes int
	if err := db.QueryRow(`SELECT COUNT(*) FROM nodes WHERE parent_id = $1 AND node_type_id = `+laneTypeSQL,
		nodeID).Scan(&lanes); err != nil {
		return fmt.Errorf("check group %d has no lanes before unmarking it: %w", nodeID, err)
	}
	if lanes > 0 {
		return fmt.Errorf("%w: group %d has %d lane(s); give them another group or remove them first",
			ErrLaneUnmarked, nodeID, lanes)
	}
	return nil
}

// UnmarkedLanes names every lane with no waiting spot: no wait points on its
// group and no legacy lane_gate_point of its own. Boot refuses a database where
// this is not empty.
func UnmarkedLanes(db *sql.DB) ([]string, error) {
	rows, err := db.Query(`
		SELECT l.name || ' (group ' || COALESCE(g.name, '<none>') || ')'
		FROM nodes l
		LEFT JOIN nodes g ON g.id = l.parent_id
		WHERE l.node_type_id = `+laneTypeSQL+`
		  AND COALESCE(TRIM((SELECT value FROM node_properties WHERE node_id = g.id AND key = $1)), '') = ''
		  AND COALESCE(TRIM((SELECT value FROM node_properties WHERE node_id = l.id AND key = $2)), '') = ''
		ORDER BY l.name`, PropGroupWaitPoints, PropLaneGatePoint)
	if err != nil {
		return nil, fmt.Errorf("unmarked lanes: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, fmt.Errorf("unmarked lanes: scan: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GroupsWithWaitingSpots is the set of groups that have at least one wait point:
// the groups a lane can be added to.
func GroupsWithWaitingSpots(db *sql.DB) (map[int64]bool, error) {
	rows, err := db.Query(`SELECT node_id, value FROM node_properties WHERE key = $1`, PropGroupWaitPoints)
	if err != nil {
		return nil, fmt.Errorf("groups with waiting spots: %w", err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, fmt.Errorf("groups with waiting spots: scan: %w", err)
		}
		if len(ParseWaitPoints(v)) > 0 {
			out[id] = true
		}
	}
	return out, rows.Err()
}
