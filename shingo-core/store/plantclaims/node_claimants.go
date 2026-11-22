package plantclaims

import (
	"database/sql"
	"fmt"
)

// node_claimants.go — a physical node as more than one station names it (owner
// ruling 2026-11-22: flagged, never refused). Groups and other synthetic nodes
// are shareable and are in neither read.

// claimedNodesSQL is the physical nodes each station claims, one row a claim
// as (node, station, claim): a claim's own position and its paired ones, under
// any style, active or not, as ClaimingStationsSQL counts them.
const claimedNodesSQL = `SELECT n.name AS node, ps.station_uid AS station, true AS claim
	  FROM style_claims sc
	  JOIN process_styles ps ON ps.process_id = sc.process_id AND ps.style_id = sc.style_id
	 CROSS JOIN LATERAL (VALUES (sc.core_node_name), (sc.paired_core_node), (sc.second_paired_core_node)) v(name)
	  JOIN nodes n ON n.name = v.name AND NOT n.is_synthetic
	 WHERE ps.station_uid IS NOT NULL`

// stationLabelSQL is a station as a person reads it: the display name, else
// the uid.
const stationLabelSQL = `COALESCE(NULLIF(er.display_name, ''), c.station)`

// SharedNode is one physical node several stations name, at least one of them
// in a claim, with those stations as a person reads them.
type SharedNode struct {
	Node     string
	Stations []string
}

// SharedNodes lists the physical nodes station and another station both name,
// by node, where at least one side is a claim; the other may be a claim or a
// position (the two sources of ClaimingStationsSQL). A place two stations hold
// only as positions is not listed. One statement.
func SharedNodes(db *sql.DB, station string) ([]SharedNode, error) {
	rows, err := db.Query(`WITH named AS (`+claimedNodesSQL+`
		UNION ALL
		SELECT n.name, ps.station_uid, false
		  FROM process_positions pp
		  JOIN process_styles ps ON ps.process_id = pp.process_id
		  JOIN nodes n ON n.name = pp.core_node_name AND NOT n.is_synthetic
		 WHERE ps.station_uid IS NOT NULL
		), shared AS (
		SELECT node FROM named GROUP BY node
		HAVING count(DISTINCT station) > 1 AND bool_or(claim) AND bool_or(station = $1)
		)
		SELECT c.node, `+stationLabelSQL+`
		  FROM (SELECT DISTINCT node, station FROM named WHERE node IN (SELECT node FROM shared)) c
		  LEFT JOIN edge_registry er ON er.station_uid = c.station
		 ORDER BY 1, 2`, station)
	if err != nil {
		return nil, fmt.Errorf("plantclaims shared nodes %s: %w", station, err)
	}
	defer rows.Close()
	var out []SharedNode
	for rows.Next() {
		var node, who string
		if err := rows.Scan(&node, &who); err != nil {
			return nil, fmt.Errorf("plantclaims shared nodes %s: scan: %w", station, err)
		}
		if len(out) == 0 || out[len(out)-1].Node != node {
			out = append(out, SharedNode{Node: node})
		}
		last := &out[len(out)-1]
		last.Stations = append(last.Stations, who)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("plantclaims shared nodes %s: %w", station, err)
	}
	return out, nil
}
