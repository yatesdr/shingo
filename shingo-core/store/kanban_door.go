package store

import (
	"fmt"

	"shingo/protocol"
	"shingocore/store/bins"
	"shingocore/store/internal/helpers"
	"shingocore/store/loaders"
	"shingocore/store/nodes"
	"shingocore/store/orders"
	"shingocore/store/reservations"
)

// kanban_door.go — the one read behind a push into a kanban (plan.Planner.ChoosePush): the
// verdict that the cart is of the kanban's loop, and each source's Short for its type, in one
// statement. No door decides that verdict again: the resolver, the gate and the divert take it
// from here (plan.DropNeed.Loop). Pin: TestKanbanPin_NoDoorReDerivesTheVerdict.

type (
	// DoorLine is one group at a push's door (nodes.DoorLine).
	DoorLine = nodes.DoorLine
	// KanbanDoor is one read of a window's push (nodes.KanbanDoor).
	KanbanDoor = nodes.KanbanDoor
)

const (
	doorRoleKanban = 0
	doorRoleSource = 1
)

// kanbanDoorSQL answers no rows at all unless the source is of the kanban's loop and the
// kanban declares a level. A cart of its loop is one lifted from a window of a live unloader
// whose outbound is the kanban, one of its supports, or one of its slots (a cancelled leg
// returns from a slot). Anything else is a stranger's and keeps the cap. $1 kanban, $2 type,
// $3 the source's name, $4 the asking order (0: none yet), $5 the kanban kind. The asks'
// episode key is protocol.KanbanEpisodeKey spelled in SQL, per group of the door;
// TestKanbanDoor_AgreesWithTheKeepersCounts holds the two together.
var kanbanDoorSQL = `
WITH RECURSIVE door AS (
	SELECT $1::bigint AS node_id, ` + fmt.Sprint(doorRoleKanban) + ` AS role, 0 AS rank
	 WHERE EXISTS (SELECT 1 FROM kanban_levels kl WHERE kl.kanban_node_id = $1)
	   AND (EXISTS (SELECT 1 FROM bin_loaders ul
	                 JOIN bin_loader_homes uh ON uh.loader_id = ul.id
	                 JOIN nodes uw ON uw.id = uh.position_node_id
	                 JOIN nodes ug ON ug.name = ul.outbound_dest
	                WHERE ul.archived_at IS NULL AND ` + loaders.ActsAsUnloaderSQL("ul") + `
	                  AND ug.id = $1 AND uw.name = $3)
	        OR EXISTS (SELECT 1 FROM kanban_supports ks JOIN nodes kp ON kp.id = ks.process_node_id
	                    WHERE ks.kanban_node_id = $1 AND kp.name = $3)
	        OR EXISTS (SELECT 1 FROM nodes ko WHERE ko.parent_id = $1 AND ko.name = $3))
),
door_all AS (
	SELECT node_id, role, rank FROM door
	UNION ALL
	SELECT s.source_node_id, ` + fmt.Sprint(doorRoleSource) + `, s.rank
	  FROM kanban_sources s JOIN door ON door.node_id = s.kanban_node_id
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
` + bins.EffectiveTypesCTE("door_types", "n.parent_id IN (SELECT node_id FROM door_all)") + `,
door_free AS (
	SELECT c.parent_id AS node_id, COUNT(*) AS free
	  FROM nodes c
	 WHERE c.parent_id IN (SELECT node_id FROM door_all)
	   AND c.enabled AND NOT c.is_synthetic AND c.claimed_by IS NULL
	   AND NOT EXISTS (SELECT 1 FROM bins fb WHERE fb.node_id = c.id)
	   AND NOT EXISTS (SELECT 1 FROM orders fo WHERE fo.delivery_node = c.name AND fo.id <> $4
	                    AND fo.status NOT IN (` + protocol.TerminalStatusSQLList() + `))
	   AND NOT ` + reservations.SlotSpokenForByStrangerSQL("fr", "c.id", "$4") + `
	   AND (NOT EXISTS (SELECT 1 FROM door_types ft WHERE ft.child = c.id)
	        OR EXISTS (SELECT 1 FROM door_types ft JOIN node_bin_types fbt ON fbt.node_id = ft.at
	                    WHERE ft.child = c.id AND fbt.bin_type_id = $2))
	 GROUP BY c.parent_id
),
door_asked AS (
	SELECT a.node_id, COUNT(o.id) FILTER (WHERE o.status NOT IN (` + protocol.TerminalStatusSQLList() + `)
	                                        AND o.parent_order_id IS NULL) AS asked
	  FROM (SELECT DISTINCT node_id FROM door_all) a
	  	  JOIN bin_types abt ON abt.id = $2
	  JOIN demand_origins d ON d.kind = $5 AND ` + helpers.DemandOpenSQL("d") + `
	   AND d.episode_key = 'mnt|' || a.node_id || '|' || abt.id
	  LEFT JOIN orders o ON o.origin_id = d.origin_id
	 GROUP BY a.node_id
)
SELECT ` + nodes.SelectCols + `, da.role, l.bin_type_id, bt.code, l.want, COALESCE(r.claimed, 0),
       COALESCE(dr.stock, 0), COALESCE(dr.at_zero, 0), COALESCE(dc.stock, 0), COALESCE(dc.at_zero, 0),
       COALESCE(a.asked, 0), COALESCE(tb.want, 0), COALESCE(df.free, 0),
       (SELECT tf.code FROM bin_types tt JOIN carrier_families tf ON tf.id = tt.family_id WHERE tt.id = $2)
  ` + nodes.FromClause + `
  JOIN door_all da ON da.node_id = n.id
  LEFT JOIN kanbans k ON k.node_id = n.id
  LEFT JOIN kanban_levels l ON l.kanban_node_id = k.node_id AND l.bin_type_id = $2
  LEFT JOIN bin_types bt ON bt.id = l.bin_type_id
  LEFT JOIN claimed r ON r.kanban_node_id = l.kanban_node_id AND r.bin_type_id = l.bin_type_id
  LEFT JOIN kanban_layer_levels tb ON tb.kanban_node_id = l.kanban_node_id AND tb.bin_type_id = l.bin_type_id AND tb.layers = 0
  LEFT JOIN door_residents dr ON dr.kanban = n.id
  LEFT JOIN door_coming dc ON dc.kanban = n.id
  LEFT JOIN door_asked a ON a.node_id = n.id
  LEFT JOIN door_free df ON df.node_id = n.id
 ORDER BY da.role, da.rank`

// ReadKanbanDoor reads what a push of one carrier type from a named source into a kanban is
// decided on: nil when the source is not of the kanban's loop, or the kanban declares no
// level (no statement more than this one either way). Non-nil says Loop. self is the asking order, 0 when it has no row yet.
func (db *DB) ReadKanbanDoor(kanbanID, binTypeID int64, source string, self int64) (*KanbanDoor, error) {
	rows, err := db.DB.Query(kanbanDoorSQL, kanbanID, binTypeID, nodes.WindowNameOf(source), self, protocol.EpisodeKindKanban)
	if err != nil {
		return nil, fmt.Errorf("kanban door %d: %w", kanbanID, err)
	}
	defer rows.Close()
	var door *KanbanDoor
	for rows.Next() {
		var (
			role                int
			typeID              *int64
			code                *string
			wantBase, wantCover *int
			l                   DoorLine
			family              string
		)
		n, err := nodes.ScanDoorLine(rows, &role, &typeID, &code, &wantBase, &wantCover,
			&l.Resident, &l.TopsResident, &l.Coming, &l.TopsComing,
			&l.Asked, &l.TopsWant, &l.FreeSlots, &family)
		if err != nil {
			return nil, fmt.Errorf("kanban door %d: scan: %w", kanbanID, err)
		}
		l.Group = n
		if typeID != nil {
			lv := KanbanPictureRow{KanbanNodeID: n.ID, BinTypeID: *typeID, BinTypeCode: *code}
			lv.Base, lv.Cover = *wantBase, *wantCover
			m := lv.KanbanLevel()
			l.Level = &m
		}
		if door == nil {
			door = &KanbanDoor{Loop: true, Family: family}
		}
		switch role {
		case doorRoleKanban:
			door.Kanban = l
		case doorRoleSource:
			door.Sources = append(door.Sources, l)
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
