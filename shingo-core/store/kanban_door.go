package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
)

// kanban_door.go — the one read behind a window's push into a kanban
// (plan.Planner.ChoosePush): the kanban's, its sources' and the type's overflow's
// Short for one carrier type, in one statement. It is the picture (the want), the
// residents, the open episodes' asks and the comings, each narrowed to the type by
// its own fragment, so a number here is the keeper's number. Pin:
// TestKanbanDoor_AgreesWithTheKeepersCounts.

type (
	// DoorLine is one group at a push's door (nodes.DoorLine).
	DoorLine = nodes.DoorLine
	// KanbanDoor is one read of a window's push (nodes.KanbanDoor).
	KanbanDoor = nodes.KanbanDoor
)

const (
	doorRoleKanban   = 0
	doorRoleSource   = 1
	doorRoleOverflow = 2
)

// kanbanDoorSQL answers no rows at all unless the source is a window of a live
// unloader whose outbound is the kanban and the kanban declares a level: that is the
// shape the door decides on Short, and every other push keeps the cap. The overflow
// has a row of its own, as well as its source's row if it is one. $1 kanban, $2 type,
// $3 the window's name, $4 the asking order (0: none yet), $5 the kanban kind. The
// asks' episode key is protocol.KanbanEpisodeKey spelled in SQL, per group of the door;
// TestKanbanDoor_AgreesWithTheKeepersCounts holds the two together.
var kanbanDoorSQL = `
WITH RECURSIVE door AS (
	SELECT $1::bigint AS node_id, ` + fmt.Sprint(doorRoleKanban) + ` AS role, 0 AS rank
	 WHERE EXISTS (SELECT 1 FROM kanban_levels kl WHERE kl.kanban_node_id = $1)
	   AND EXISTS (SELECT 1 FROM bin_loaders ul
	                JOIN bin_loader_homes uh ON uh.loader_id = ul.id
	                JOIN nodes uw ON uw.id = uh.position_node_id
	                JOIN nodes ug ON ug.name = ul.outbound_dest
	               WHERE ul.archived_at IS NULL AND ` + loaders.ActsAsUnloaderSQL("ul") + `
	                 AND ug.id = $1 AND uw.name = $3)
),
door_all AS (
	SELECT node_id, role, rank FROM door
	UNION ALL
	SELECT s.source_node_id, ` + fmt.Sprint(doorRoleSource) + `, s.rank
	  FROM kanban_sources s JOIN door ON door.node_id = s.kanban_node_id
	UNION ALL
	SELECT COALESCE(ol.overflow_node_id, ok.overflow_node_id), ` + fmt.Sprint(doorRoleOverflow) + `, 0
	  FROM door
	  JOIN kanbans ok ON ok.node_id = door.node_id
	  LEFT JOIN kanban_levels ol ON ol.kanban_node_id = ok.node_id AND ol.bin_type_id = $2
	 WHERE COALESCE(ol.overflow_node_id, ok.overflow_node_id) IS NOT NULL
),
scope AS (
	SELECT DISTINCT d.node_id FROM door_all d JOIN kanbans k ON k.node_id = d.node_id
),` + kanbanPictureCTEs + `,
kanban_tree(kanban, id) AS (
	SELECT DISTINCT d.node_id, d.node_id FROM door_all d
	UNION ALL
	SELECT t.kanban, n2.id FROM nodes n2 JOIN kanban_tree t ON n2.parent_id = t.id
),
door_residents AS (` + bins.KanbanResidentsOfTypeSQL("$2") + `),
door_coming AS (` + orders.KanbanComingOfTypeSQL("$2", "$4", "$5") + `),
door_asked AS (
	SELECT a.node_id, COUNT(o.id) FILTER (WHERE o.status NOT IN (` + protocol.TerminalStatusSQLList() + `)
	                                        AND o.parent_order_id IS NULL) AS asked
	  FROM (SELECT DISTINCT node_id FROM door_all) a
	  JOIN nodes an ON an.id = a.node_id
	  JOIN bin_types abt ON abt.id = $2
	  JOIN demand_origins d ON d.kind = $5 AND ` + helpers.DemandOpenSQL("d") + `
	   AND d.episode_key = 'mnt|' || an.name || '|' || abt.code
	  LEFT JOIN orders o ON o.origin_id = d.origin_id
	 GROUP BY a.node_id
)
SELECT ` + nodes.SelectCols + `, da.role, l.bin_type_id, bt.code, l.want, k.running_cover, COALESCE(r.running, 0),
       l.overflow_node_id, COALESCE(lo.name, ''),
       COALESCE(dr.stock, 0), COALESCE(dr.at_zero, 0), COALESCE(dc.stock, 0), COALESCE(dc.at_zero, 0),
       COALESCE(a.asked, 0), COALESCE(tb.want, 0)
  ` + nodes.FromClause + `
  JOIN door_all da ON da.node_id = n.id
  LEFT JOIN kanbans k ON k.node_id = n.id
  LEFT JOIN kanban_levels l ON l.kanban_node_id = k.node_id AND l.bin_type_id = $2
  LEFT JOIN bin_types bt ON bt.id = l.bin_type_id
  LEFT JOIN nodes lo ON lo.id = l.overflow_node_id
  LEFT JOIN running r ON r.kanban_node_id = l.kanban_node_id AND r.bin_type_id = l.bin_type_id
  LEFT JOIN kanban_layer_levels tb ON tb.kanban_node_id = l.kanban_node_id AND tb.bin_type_id = l.bin_type_id AND tb.layers = 0
  LEFT JOIN door_residents dr ON dr.kanban = n.id
  LEFT JOIN door_coming dc ON dc.kanban = n.id
  LEFT JOIN door_asked a ON a.node_id = n.id
 ORDER BY da.role, da.rank`

// ReadKanbanDoor reads what a push of one carrier type from a named source into a
// kanban is decided on: nil when the source is not a window of an unloader whose
// outbound is the kanban, or the kanban declares no level (no statement more than
// this one either way). self is the asking order, 0 when it has no row yet.
func (db *DB) ReadKanbanDoor(kanbanID, binTypeID int64, source string, self int64) (*KanbanDoor, error) {
	rows, err := db.DB.Query(kanbanDoorSQL, kanbanID, binTypeID, nodes.WindowNameOf(source), self, protocol.EpisodeKindKanban)
	if err != nil {
		return nil, fmt.Errorf("kanban door %d: %w", kanbanID, err)
	}
	defer rows.Close()
	var door *KanbanDoor
	for rows.Next() {
		var (
			role                       int
			typeID                     *int64
			code                       *string
			wantBase, wantCover, wantR *int
			levelOverflow              *int64
			levelOverflowName          string
			l                          DoorLine
		)
		n, err := nodes.ScanDoorLine(rows, &role, &typeID, &code, &wantBase, &wantCover, &wantR,
			&levelOverflow, &levelOverflowName, &l.Resident, &l.TopsResident, &l.Coming, &l.TopsComing,
			&l.Asked, &l.TopsWant)
		if err != nil {
			return nil, fmt.Errorf("kanban door %d: scan: %w", kanbanID, err)
		}
		l.Group = n
		if typeID != nil {
			lv := KanbanPictureRow{KanbanNodeID: n.ID, BinTypeID: *typeID, BinTypeCode: *code, LevelOverflowNodeID: levelOverflow,
				OverflowNode: levelOverflowName}
			lv.Base, lv.Cover, lv.Running = *wantBase, *wantCover, *wantR
			m := lv.KanbanLevel()
			l.Level = &m
		}
		if door == nil {
			door = &KanbanDoor{}
		}
		switch role {
		case doorRoleKanban:
			door.Kanban = l
		case doorRoleSource:
			door.Sources = append(door.Sources, l)
		case doorRoleOverflow:
			door.Overflow = &l
		}
	}
	return door, rows.Err()
}

// DoorReads is the read view a door resolves a slot in one group through, one read
// of the group's children and its types (KanbanAskReads), with the line it already
// holds so the cap asks no statement. Without a level the group declares none of the
// type, and a push of an empty asks it nothing more: a level on another type refuses
// a store with a payload only, and the door's push has none. Nil line: nothing is
// known and the view reads the group's levels itself.
func (db *DB) DoorReads(groupID int64, line *DoorLine) *KanbanAskReads {
	if line == nil {
		return &KanbanAskReads{GroupPassReads: db.GroupPassReads(groupID)}
	}
	var levels []KanbanLevel
	residents := map[string]int{}
	if line.Level != nil {
		levels = append(levels, *line.Level)
		residents[line.Level.BinTypeCode] = line.Resident
	}
	return db.KanbanAskReads(groupID, levels, residents)
}
