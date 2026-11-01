package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"shingo/protocol"
	"shingocore/domain"
	"shingocore/kanban"
	"shingocore/store/internal/helpers"
)

// kanban_picture.go — the one statement that says what a kanban is right now.

// KanbanRef names a node a kanban points at.
type KanbanRef struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// KanbanPictureRow is one (kanban, level) of KanbanPictureSQL. The embedded
// kanban.Level carries Base, Cover and Running, and composes Want().
type KanbanPictureRow struct {
	kanban.Level
	KanbanNodeID int64
	KanbanName   string
	BinTypeID    int64
	BinTypeCode  string
	// Slots is the kanban's enabled children.
	Slots int
	// Supports are the process nodes it serves, by name.
	Supports []KanbanRef
	// Sources are the groups its keeper may pull from, in rank order.
	Sources []KanbanRef
	// Floor is the type's floor size, Sized false with none entered.
	Floor domain.FloorSize
	Sized bool
	// OverflowNodeID and OverflowNode are where this type overflows:
	// COALESCE(level, kanban). LevelOverflowNodeID is the level row's own, nil
	// when the type takes the kanban's default.
	OverflowNodeID      *int64
	OverflowNode        string
	LevelOverflowNodeID *int64
	// TopsAndBottomsWant is the want of the type's row at 0 layers, 0 with none.
	TopsAndBottomsWant int
	StationID          string
	Strict             bool
	// Live says the kanban's node is an enabled group: a disabled one is counted
	// by nobody and keeps nothing.
	Live bool
	// RunningProcesses are the processes Running counts, by name: the card's
	// answer to which process is driving the want.
	RunningProcesses []string
	// NothingProcesses are the kanban's processes with a part that contributes
	// nothing to Running: no payload_bin_types row, or several. A process can be
	// here and also count for another part. The kanban's card names them.
	NothingProcesses []string
	// FallbackProcesses are the kanban's processes with an open changeover whose
	// target resolves to no type: they count on their active style's types, and
	// the kanban's card names them.
	FallbackProcesses []string
}

// KanbanPictureSQL is the kanban's one read: one row per (kanban, level), $1 NULL
// for all kanbans or an array of node ids. A kanban with no level has no row.
//
// RUNNING. A process runs its effective style: the target of its latest open
// changeover episode when that resolves to a type, else its active style
// (process_styles.is_active). Every levels reader takes it through
// DB.ListKanbanLevels or this read directly.
//
//   - open_changeover: the latest open episode per process. A changeover episode
//     carries its target's first produce claim as (process_id, core_node_name,
//     payload_code), which is how target_style finds the style. Two styles can
//     share that claim: the one that is not active is the target, and a choice
//     still open resolves to nothing rather than to a guess.
//   - part_carrier and style_part: a style's types are its produce claims' parts
//     through payload_bin_types, and a part counts only with exactly one row.
//   - effective: the style each process counts on. fell_back marks an open
//     episode whose target resolved to no type.
//   - connected: a process belongs to a kanban when its effective style's claims
//     name a supported node as core, paired or second paired node.
//   - running: DISTINCT processes per (kanban, type).
//
// Everything is one statement over the mirror; no read is per kanban or per level.
const KanbanPictureSQL = `
WITH scope AS (
	SELECT k.node_id FROM kanbans k
	 WHERE $1::bigint[] IS NULL OR k.node_id = ANY($1::bigint[])
),` + kanbanPictureCTEs + kanbanPictureSelect

// kanbanPictureWithSourcesSQL is the picture of the named kanbans and of their
// sources, in one statement. Rows as the picture's.
const kanbanPictureWithSourcesSQL = `
WITH scope AS (
	SELECT k.node_id FROM kanbans k
	 WHERE k.node_id = ANY($1::bigint[])
	    OR k.node_id IN (SELECT s.source_node_id FROM kanban_sources s WHERE s.kanban_node_id = ANY($1::bigint[]))
),` + kanbanPictureCTEs + kanbanPictureSelect

// kanbanPictureCTEs is everything the picture composes from its scope CTE. A door
// reading one type's want reads it through this (store.DB.ReadKanbanDoor).
const kanbanPictureCTEs = `
positions AS (
	SELECT s.kanban_node_id, pn.name AS node_name
	  FROM kanban_supports s
	  JOIN scope ON scope.node_id = s.kanban_node_id
	  JOIN nodes pn ON pn.id = s.process_node_id
),
active AS (
	SELECT process_id, style_id FROM process_styles WHERE is_active
),
open_changeover AS (
	SELECT DISTINCT ON (process_id) process_id, core_node_name, payload_code
	  FROM demand_origins
	 WHERE kind = '` + protocol.EpisodeKindChangeover + `' AND closed_at IS NULL AND process_id <> ''
	 ORDER BY process_id, opened_at DESC NULLS LAST
),
target_candidate AS (
	SELECT DISTINCT oc.process_id, sc.style_id, a.style_id IS NOT NULL AS is_active
	  FROM open_changeover oc
	  JOIN style_claims sc ON sc.process_id = oc.process_id
	                      AND sc.core_node_name = oc.core_node_name
	                      AND sc.payload_code = oc.payload_code
	                      AND sc.role = '` + string(protocol.ClaimRoleProduce) + `'
	  LEFT JOIN active a ON a.process_id = sc.process_id AND a.style_id = sc.style_id
),
target_style AS (
	SELECT process_id,
	       CASE WHEN COUNT(*) FILTER (WHERE NOT is_active) = 1 THEN MAX(style_id) FILTER (WHERE NOT is_active)
	            WHEN COUNT(*) = 1 THEN MAX(style_id)
	       END AS style_id
	  FROM target_candidate
	 GROUP BY process_id
),
part_carrier AS (
	SELECT p.code, COUNT(pbt.bin_type_id) AS carriers, MIN(pbt.bin_type_id) AS bin_type_id
	  FROM payloads p
	  LEFT JOIN payload_bin_types pbt ON pbt.payload_id = p.id
	 GROUP BY p.code
),
style_part AS (
	SELECT DISTINCT sc.process_id, sc.style_id, sc.payload_code,
	       CASE WHEN pc.carriers = 1 THEN pc.bin_type_id END AS bin_type_id
	  FROM style_claims sc
	  LEFT JOIN part_carrier pc ON pc.code = sc.payload_code
	 WHERE sc.role = '` + string(protocol.ClaimRoleProduce) + `' AND sc.payload_code <> ''
),
effective AS (
	SELECT pr.process_id,
	       CASE WHEN resolves.style_id IS NOT NULL THEN resolves.style_id ELSE a.style_id END AS style_id,
	       oc.process_id IS NOT NULL AND resolves.style_id IS NULL AS fell_back
	  FROM (SELECT process_id FROM active UNION SELECT process_id FROM open_changeover) pr
	  LEFT JOIN active a ON a.process_id = pr.process_id
	  LEFT JOIN open_changeover oc ON oc.process_id = pr.process_id
	  LEFT JOIN LATERAL (
		SELECT ts.style_id FROM target_style ts
		 WHERE ts.process_id = pr.process_id
		   AND EXISTS (SELECT 1 FROM style_part sp
		                WHERE sp.process_id = ts.process_id AND sp.style_id = ts.style_id
		                  AND sp.bin_type_id IS NOT NULL)
	  ) resolves ON true
),
connected AS (
	SELECT DISTINCT pos.kanban_node_id, e.process_id, e.style_id
	  FROM effective e
	  JOIN style_claims sc ON sc.process_id = e.process_id AND sc.style_id = e.style_id
	  JOIN positions pos ON pos.node_name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
),
running AS (
	SELECT c.kanban_node_id, sp.bin_type_id, COUNT(DISTINCT c.process_id) AS running,
	       json_agg(DISTINCT c.process_id ORDER BY c.process_id) AS processes
	  FROM connected c
	  JOIN style_part sp ON sp.process_id = c.process_id AND sp.style_id = c.style_id
	 WHERE sp.bin_type_id IS NOT NULL
	 GROUP BY c.kanban_node_id, sp.bin_type_id
),
nothing AS (
	SELECT c.kanban_node_id, json_agg(DISTINCT c.process_id ORDER BY c.process_id) AS processes
	  FROM connected c
	  JOIN style_part sp ON sp.process_id = c.process_id AND sp.style_id = c.style_id
	 WHERE sp.bin_type_id IS NULL
	 GROUP BY c.kanban_node_id
),
fallen AS (
	SELECT pos.kanban_node_id, json_agg(DISTINCT e.process_id ORDER BY e.process_id) AS processes
	  FROM effective e
	  JOIN style_claims sc ON sc.process_id = e.process_id
	  JOIN positions pos ON pos.node_name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
	 WHERE e.fell_back
	 GROUP BY pos.kanban_node_id
),
slots AS (
	SELECT c.parent_id AS kanban_node_id, COUNT(*) AS slots
	  FROM nodes c
	  JOIN scope ON scope.node_id = c.parent_id
	 WHERE c.enabled
	 GROUP BY c.parent_id
),
supports AS (
	SELECT s.kanban_node_id, json_agg(json_build_object('id', pn.id, 'name', pn.name) ORDER BY pn.name) AS refs
	  FROM kanban_supports s
	  JOIN scope ON scope.node_id = s.kanban_node_id
	  JOIN nodes pn ON pn.id = s.process_node_id
	 GROUP BY s.kanban_node_id
),
sources AS (
	SELECT s.kanban_node_id, json_agg(json_build_object('id', n.id, 'name', n.name) ORDER BY s.rank) AS refs
	  FROM kanban_sources s
	  JOIN scope ON scope.node_id = s.kanban_node_id
	  JOIN nodes n ON n.id = s.source_node_id
	 GROUP BY s.kanban_node_id
)`

const kanbanPictureSelect = `
SELECT g.id, g.name, l.bin_type_id, bt.code, l.want, k.running_cover, COALESCE(r.running, 0),
       COALESCE(sl.slots, 0), COALESCE(su.refs, '[]'), COALESCE(so.refs, '[]'),
       COALESCE(l.overflow_node_id, k.overflow_node_id), COALESCE(ov.name, ''), l.overflow_node_id,
       COALESCE(tb.want, 0), COALESCE(k.station_id, ''), k.strict, g.enabled AND g.is_synthetic,
       COALESCE(r.processes, '[]'), COALESCE(nt.processes, '[]'), COALESCE(fa.processes, '[]'),
       COALESCE(bt.width_in, 0), COALESCE(bt.length_in, 0)
  FROM scope
  JOIN kanbans k ON k.node_id = scope.node_id
  JOIN nodes g ON g.id = k.node_id
  JOIN kanban_levels l ON l.kanban_node_id = k.node_id
  JOIN bin_types bt ON bt.id = l.bin_type_id
  LEFT JOIN nodes ov ON ov.id = COALESCE(l.overflow_node_id, k.overflow_node_id)
  LEFT JOIN kanban_layer_levels tb ON tb.kanban_node_id = k.node_id AND tb.bin_type_id = l.bin_type_id AND tb.layers = 0
  LEFT JOIN running r ON r.kanban_node_id = k.node_id AND r.bin_type_id = l.bin_type_id
  LEFT JOIN slots sl ON sl.kanban_node_id = k.node_id
  LEFT JOIN supports su ON su.kanban_node_id = k.node_id
  LEFT JOIN sources so ON so.kanban_node_id = k.node_id
  LEFT JOIN nothing nt ON nt.kanban_node_id = k.node_id
  LEFT JOIN fallen fa ON fa.kanban_node_id = k.node_id
 ORDER BY g.name, bt.code`

// ListKanbanPicture reads the picture: every kanban for nil ids, else those named.
func (db *DB) ListKanbanPicture(ids []int64) ([]KanbanPictureRow, error) {
	return listKanbanPicture(db.DB, ids)
}

// ListKanbanPictureWithSources reads the picture of the named kanbans and of their
// sources in one statement: a snapshot build's one levels read, so an unloader's
// walk can ask a kanban's sources with no second one.
func (db *DB) ListKanbanPictureWithSources(ids []int64) ([]KanbanPictureRow, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	return scanKanbanPicture(db.DB.Query(kanbanPictureWithSourcesSQL, ids))
}

// KanbanLevel is the row as a levels reader takes it: Want the effective want,
// Base the declared number, the overflow the row's own.
func (r KanbanPictureRow) KanbanLevel() KanbanLevel {
	lv := KanbanLevel{GroupNodeID: r.KanbanNodeID, BinTypeID: r.BinTypeID, BinTypeCode: r.BinTypeCode,
		Want: r.Want(), Base: r.Base, OverflowNodeID: r.LevelOverflowNodeID}
	if r.LevelOverflowNodeID != nil {
		lv.OverflowNode = r.OverflowNode
	}
	return lv
}

// LevelsOfPicture is the rows as levels, in the picture's order (by code within a
// kanban).
func LevelsOfPicture(rows []KanbanPictureRow) []KanbanLevel {
	var out []KanbanLevel
	for _, r := range rows {
		out = append(out, r.KanbanLevel())
	}
	return out
}

func listKanbanPicture(q helpers.RowsQuerier, ids []int64) ([]KanbanPictureRow, error) {
	var arg any
	if ids != nil {
		arg = ids
	}
	return scanKanbanPicture(q.Query(KanbanPictureSQL, arg))
}

func scanKanbanPicture(rows *sql.Rows, err error) ([]KanbanPictureRow, error) {
	if err != nil {
		return nil, fmt.Errorf("kanban picture: %w", err)
	}
	defer rows.Close()
	var out []KanbanPictureRow
	for rows.Next() {
		var (
			r                                         KanbanPictureRow
			supports, sources, running, nothing, fell []byte
			width, length                             float64
		)
		if err := rows.Scan(&r.KanbanNodeID, &r.KanbanName, &r.BinTypeID, &r.BinTypeCode,
			&r.Base, &r.Cover, &r.Running, &r.Slots, &supports, &sources,
			&r.OverflowNodeID, &r.OverflowNode, &r.LevelOverflowNodeID,
			&r.TopsAndBottomsWant, &r.StationID, &r.Strict, &r.Live, &running, &nothing, &fell,
			&width, &length); err != nil {
			return nil, fmt.Errorf("kanban picture: scan: %w", err)
		}
		r.Floor, r.Sized = domain.BinType{WidthIn: width, LengthIn: length}.FloorSize()
		for _, f := range []struct {
			raw []byte
			to  any
		}{{supports, &r.Supports}, {sources, &r.Sources}, {running, &r.RunningProcesses},
			{nothing, &r.NothingProcesses}, {fell, &r.FallbackProcesses}} {
			if err := json.Unmarshal(f.raw, f.to); err != nil {
				return nil, fmt.Errorf("kanban picture: decode: %w", err)
			}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
