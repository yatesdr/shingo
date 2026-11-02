package store

import (
	"database/sql"
	"fmt"
	"log"
	"strings"

	"shingocore/store/nodes"
	"shingocore/store/schema"
)

// kanban_migration.go — v169: one row per kanban, its settings as columns, its
// sets keyed on that row, and the census of what the pivot could not carry.

// KanbanMigrationCensusSQL names every group the kanban migration (v169) cannot
// carry across as it stands, against the PRE-migration shape: read-only, to run
// against a plant before the deploy. One row per finding: (list, group_name,
// detail). v169 runs it too and logs each row, so a plant's log holds what its
// own migration saw.
//
// IT LISTS, IT DOES NOT DECIDE. The lists:
//
//	levels_no_station    levels and no station: its keeper is skipped until one is set
//	levels_flag_off      levels and maintain_enabled not on: a cap with no keeper today; the flag goes, and with a station its keeper starts
//	overflow_unresolved  a group that gets a kanbans row names an overflow that does not resolve, or itself: NULL after the pivot
//	kanban_no_supports   levels and no supports: holds a level and serves nobody by name
//	supports_no_levels   supports and no level: a kanbans row exists for the foreign key
//	layers_no_levels     layer wants and no level: a kanbans row exists for the foreign key
//	strict_no_levels     strict on and no level: a kanbans row exists so the fence stays up
//	plain_overflow_kept  no kanbans row, names an overflow: stays a property, the doors honour it there
//	plain_station_unread no kanbans row, names a station: the row stays and nothing reads it
//
// The overflow names resolve as nodes.GetByDotName resolves them: split at the
// first dot, a name with none matched whole.
const KanbanMigrationCensusSQL = `
WITH prop AS (
	SELECT p.node_id,
	       MAX(p.value) FILTER (WHERE p.key = 'maintenance_station') AS station,
	       MAX(p.value) FILTER (WHERE p.key = 'maintain_enabled') AS enabled,
	       MAX(p.value) FILTER (WHERE p.key = 'strict_sourcing') AS strict,
	       MAX(p.value) FILTER (WHERE p.key = 'overflow_destination') AS overflow
	  FROM node_properties p
	 WHERE p.key IN ('maintenance_station', 'maintain_enabled', 'strict_sourcing', 'overflow_destination')
	 GROUP BY p.node_id
),
shape AS (
	SELECT g.id, g.name,
	       EXISTS (SELECT 1 FROM node_maintain_levels x WHERE x.group_node_id = g.id) AS levels,
	       EXISTS (SELECT 1 FROM node_maintain_supports x WHERE x.group_node_id = g.id) AS supports,
	       EXISTS (SELECT 1 FROM node_maintain_layer_levels x WHERE x.group_node_id = g.id) AS layers,
	       COALESCE(p.strict, '') = 'on' AS strict,
	       COALESCE(p.enabled, '') AS enabled,
	       TRIM(COALESCE(p.station, '')) AS station,
	       TRIM(COALESCE(p.overflow, '')) AS overflow
	  FROM nodes g
	  LEFT JOIN prop p ON p.node_id = g.id
),
resolved AS (
	SELECT s.*, s.levels OR s.supports OR s.layers OR s.strict AS has_row,
	       (SELECT d.id FROM nodes d
	         WHERE CASE WHEN strpos(s.overflow, '.') = 0 THEN d.name = s.overflow
	                    ELSE d.name = substr(s.overflow, strpos(s.overflow, '.') + 1)
	                         AND d.parent_id IN (SELECT id FROM nodes
	                              WHERE name = left(s.overflow, strpos(s.overflow, '.') - 1))
	               END
	       ) AS overflow_id
	  FROM shape s
)
SELECT list, name AS group_name, detail FROM (
	SELECT 'levels_no_station' AS list, name, 'declares a level and names no station' AS detail
	  FROM resolved WHERE levels AND station = ''
	UNION ALL
	SELECT 'levels_flag_off', name, 'declares a level with maintain_enabled=' || CASE WHEN enabled = '' THEN '(absent)' ELSE enabled END
	  FROM resolved WHERE levels AND enabled <> 'on'
	UNION ALL
	SELECT 'overflow_unresolved', name,
	       CASE WHEN overflow_id = id THEN 'overflow names itself: ' ELSE 'overflow does not resolve: ' END || overflow
	  FROM resolved WHERE has_row AND overflow <> '' AND (overflow_id IS NULL OR overflow_id = id)
	UNION ALL
	SELECT 'kanban_no_supports', name, 'declares a level and supports no process node'
	  FROM resolved WHERE levels AND NOT supports
	UNION ALL
	SELECT 'supports_no_levels', name, 'supports process nodes and declares no level'
	  FROM resolved WHERE supports AND NOT levels
	UNION ALL
	SELECT 'layers_no_levels', name, 'has layer wants and declares no level'
	  FROM resolved WHERE layers AND NOT levels
	UNION ALL
	SELECT 'strict_no_levels', name, 'strict_sourcing is on and it declares no level'
	  FROM resolved WHERE strict AND NOT levels
	UNION ALL
	SELECT 'plain_overflow_kept', name, 'overflow: ' || overflow
	  FROM resolved WHERE NOT has_row AND overflow <> ''
	UNION ALL
	SELECT 'plain_station_unread', name, 'station: ' || station
	  FROM resolved WHERE NOT has_row AND station <> ''
) findings
ORDER BY list, name`

// v169Kanbans gives a kanban its row. kanbans holds the settings that were node
// properties (station, strict, overflow as a node id, and the running cover that
// is new); the three set tables take their final names and hang off it;
// kanban_sources is new and empty; and the two reads every descendants walk and
// strict fence start from get their indexes.
//
// THE PIVOT, name to id in Go, covers every group that needs a row: levels, or
// the sets and fence that need one (supports, layer wants, strict on; the census
// names these). Their station, strict and overflow properties go, and so does
// every maintain_enabled row: a group with levels IS a kanban, so the flag has no
// reader. A plain group's overflow_destination stays a property, and the doors
// keep honouring it there (nodes.OverflowTargetOf). Its other leftover rows stay
// too, and nothing reads them.
//
// ALTER, not copy, so the rows and their timestamps stay. The constraints take
// the table's new name with it. RE-RUNNABLE, as the self-heal needs: each step
// finds its own work done. A v90 or v161 self-healed against a drifted column
// re-creates its empty old-named table, and this step drops it.
//
// ROLLBACK is the previous binary plus the reverse DDL in ageKanbanSchema
// (kanban_migration_docker_test.go) and re-inserting the properties from kanbans.
func v169Kanbans(tx *sql.Tx) error {
	if schema.TableExists(tx, "node_maintain_levels") && !schema.TableExists(tx, "kanban_levels") {
		if err := logKanbanCensus(tx); err != nil {
			return err
		}
	}
	if err := v169RenameTables(tx); err != nil {
		return err
	}
	if err := v169Exec(tx,
		`CREATE TABLE IF NOT EXISTS kanbans (
			node_id          BIGINT PRIMARY KEY REFERENCES nodes(id) ON DELETE CASCADE,
			station_id       TEXT,
			strict           BOOLEAN NOT NULL DEFAULT false,
			running_cover    INTEGER NOT NULL DEFAULT 0 CHECK (running_cover >= 0),
			overflow_node_id BIGINT REFERENCES nodes(id) ON DELETE SET NULL,
			created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			CHECK (overflow_node_id <> node_id)
		)`,
		`ALTER TABLE kanban_levels ADD COLUMN IF NOT EXISTS overflow_node_id BIGINT REFERENCES nodes(id) ON DELETE SET NULL`,
		`CREATE INDEX IF NOT EXISTS idx_nodes_parent ON nodes(parent_id)`,
		`CREATE INDEX IF NOT EXISTS idx_node_properties_key ON node_properties(key)`,
	); err != nil {
		return err
	}
	if err := v169PivotProperties(tx); err != nil {
		return err
	}
	return v169Exec(tx,
		`ALTER TABLE kanban_levels DROP CONSTRAINT IF EXISTS kanban_levels_kanban_node_id_fkey`,
		`ALTER TABLE kanban_levels ADD CONSTRAINT kanban_levels_kanban_node_id_fkey
			FOREIGN KEY (kanban_node_id) REFERENCES kanbans(node_id) ON DELETE CASCADE`,
		`ALTER TABLE kanban_supports DROP CONSTRAINT IF EXISTS kanban_supports_kanban_node_id_fkey`,
		`ALTER TABLE kanban_supports ADD CONSTRAINT kanban_supports_kanban_node_id_fkey
			FOREIGN KEY (kanban_node_id) REFERENCES kanbans(node_id) ON DELETE CASCADE`,
		`ALTER TABLE kanban_layer_levels DROP CONSTRAINT IF EXISTS kanban_layer_levels_kanban_node_id_fkey`,
		`ALTER TABLE kanban_layer_levels ADD CONSTRAINT kanban_layer_levels_kanban_node_id_fkey
			FOREIGN KEY (kanban_node_id) REFERENCES kanbans(node_id) ON DELETE CASCADE`,
		`CREATE TABLE IF NOT EXISTS kanban_sources (
			kanban_node_id BIGINT NOT NULL REFERENCES kanbans(node_id) ON DELETE CASCADE,
			rank           INTEGER NOT NULL CHECK (rank >= 1),
			source_node_id BIGINT NOT NULL REFERENCES nodes(id) ON DELETE CASCADE,
			PRIMARY KEY (kanban_node_id, rank),
			UNIQUE (kanban_node_id, source_node_id),
			CHECK (source_node_id <> kanban_node_id)
		)`,
	)
}

func v169Exec(tx *sql.Tx, stmts ...string) error {
	for _, s := range stmts {
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v169 kanbans: %w", err)
		}
	}
	return nil
}

// logKanbanCensus writes the census to the log, as it stands before the pivot.
func logKanbanCensus(tx *sql.Tx) error {
	rows, err := tx.Query(KanbanMigrationCensusSQL)
	if err != nil {
		return fmt.Errorf("v169 census: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var list, group, detail string
		if err := rows.Scan(&list, &group, &detail); err != nil {
			return fmt.Errorf("v169 census: scan: %w", err)
		}
		log.Printf("migrations: v169 kanbans: %s: %s: %s", list, group, detail)
	}
	return rows.Err()
}

// v169Renames is old table name to new, the three set tables. group_node_id
// becomes kanban_node_id in each, the one spelling with kanban_sources. keep is
// the constraints that mean the same after the rename; the group_node_id foreign
// key is not among them, its replacement points at kanbans.
var v169Renames = []struct {
	from, to string
	keep     []string
}{
	{"node_maintain_levels", "kanban_levels", []string{"pkey", "want_check", "bin_type_id_fkey"}},
	{"node_maintain_supports", "kanban_supports", []string{"pkey", "process_node_id_fkey"}},
	{"node_maintain_layer_levels", "kanban_layer_levels", []string{"pkey", "layers_check", "want_check", "bin_type_id_fkey"}},
}

func v169RenameTables(tx *sql.Tx) error {
	for _, r := range v169Renames {
		if schema.TableExists(tx, r.to) {
			if err := v169Exec(tx, `DROP TABLE IF EXISTS `+r.from); err != nil {
				return err
			}
			continue
		}
		stmts := []string{
			`ALTER TABLE ` + r.from + ` DROP CONSTRAINT ` + r.from + `_group_node_id_fkey`,
			`ALTER TABLE ` + r.from + ` RENAME TO ` + r.to,
			`ALTER TABLE ` + r.to + ` RENAME COLUMN group_node_id TO kanban_node_id`,
		}
		for _, suffix := range r.keep {
			stmts = append(stmts, `ALTER TABLE `+r.to+` RENAME CONSTRAINT `+r.from+`_`+suffix+` TO `+r.to+`_`+suffix)
		}
		if err := v169Exec(tx, stmts...); err != nil {
			return err
		}
	}
	return nil
}

// v169Group is one group the pivot gives a row, with the three property values
// it moves.
type v169Group struct {
	id                     int64
	name                   string
	station, strict, spill string
}

func v169GroupsToPivot(tx *sql.Tx) ([]v169Group, error) {
	rows, err := tx.Query(`
		SELECT g.id, g.name,
		       COALESCE((SELECT value FROM node_properties WHERE node_id = g.id AND key = 'maintenance_station'), ''),
		       COALESCE((SELECT value FROM node_properties WHERE node_id = g.id AND key = 'strict_sourcing'), ''),
		       COALESCE((SELECT value FROM node_properties WHERE node_id = g.id AND key = 'overflow_destination'), '')
		  FROM nodes g
		 WHERE g.id IN (SELECT kanban_node_id FROM kanban_levels
		                UNION SELECT kanban_node_id FROM kanban_supports
		                UNION SELECT kanban_node_id FROM kanban_layer_levels
		                UNION SELECT node_id FROM node_properties WHERE key = 'strict_sourcing' AND value = 'on')
		 ORDER BY g.id`)
	if err != nil {
		return nil, fmt.Errorf("v169 read groups: %w", err)
	}
	defer rows.Close()
	var groups []v169Group
	for rows.Next() {
		var g v169Group
		if err := rows.Scan(&g.id, &g.name, &g.station, &g.strict, &g.spill); err != nil {
			return nil, fmt.Errorf("v169 scan group: %w", err)
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// v169PivotProperties writes a kanbans row for every group that needs one, from
// its properties, and deletes what it moved. An overflow that does not resolve,
// or names the group itself, is NULL and logged: the door parks on it today and
// parks on NULL after, so nothing about the push changes.
func v169PivotProperties(tx *sql.Tx) error {
	groups, err := v169GroupsToPivot(tx)
	if err != nil {
		return err
	}
	for _, g := range groups {
		var overflow any
		if name := strings.TrimSpace(g.spill); name != "" {
			id, ok, err := nodes.ResolveDotNameID(tx, name)
			if err != nil {
				return fmt.Errorf("v169 resolve overflow of %s: %w", g.name, err)
			}
			if ok && id != g.id {
				overflow = id
			} else {
				log.Printf("migrations: v169 kanbans: %s: overflow %q is NULL after the pivot", g.name, name)
			}
		}
		if _, err := tx.Exec(`
			INSERT INTO kanbans (node_id, station_id, strict, overflow_node_id)
			VALUES ($1, NULLIF(TRIM($2::text), ''), $3, $4)
			ON CONFLICT (node_id) DO NOTHING`, g.id, g.station, g.strict == "on", overflow); err != nil {
			return fmt.Errorf("v169 kanbans row for %s: %w", g.name, err)
		}
		if _, err := tx.Exec(`DELETE FROM node_properties WHERE node_id = $1
			AND key IN ('maintenance_station', 'strict_sourcing', 'overflow_destination')`, g.id); err != nil {
			return fmt.Errorf("v169 delete properties of %s: %w", g.name, err)
		}
	}
	return v169Exec(tx, `DELETE FROM node_properties WHERE key = 'maintain_enabled'`)
}

// v169Done is the post-condition: the tables are the new shape and no old name or
// retired key is left. A maintain_enabled written since through the generic
// property endpoint re-runs the step, which only deletes it again.
func v169Done(q schema.Querier) bool {
	for _, r := range v169Renames {
		if !schema.TableExists(q, r.to) || !schema.TableAbsent(q, r.from) {
			return false
		}
	}
	return schema.TableExists(q, "kanbans") &&
		schema.TableExists(q, "kanban_sources") &&
		schema.IndexExists(q, "idx_nodes_parent") &&
		schema.IndexExists(q, "idx_node_properties_key") &&
		schema.NodePropertyKeyAbsent(q, "maintain_enabled")
}

// kanbanSetsRenamed is what the verifies of v90, v161 and v164 add. Those three
// created tables v169 renames; without it each would fail its post-condition on
// every boot once v169 ran and re-create the old table, the shape
// TestMigrate_NoMigrationOscillatesOnEveryBoot exists to catch.
func kanbanSetsRenamed(q schema.Querier) bool {
	return schema.TableExists(q, "kanban_levels")
}
