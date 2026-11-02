package store

import (
	"database/sql"
	"fmt"
	"log"

	"shingo/protocol"
	"shingocore/store/internal/helpers"
	"shingocore/store/schema"
)

// kanban_family_migration.go — v172: the carrier family and a kanban's pool.
//
// LISTS, NEVER DECIDES. The backfill groups types by what they already say, a footprint
// entered on both sides, and codes the family by that footprint, so no name is chosen by
// anybody. A plant that disagrees regroups them on the Bin Types page, and the census names
// what looks wrong. No pool row is written: a pool is what a plant bought, and the save writes
// it at the minimum and says so on the card.
//
// RE-RUNNABLE. Every step reads what it finds and the verify is the post-condition.

// FamilyCensusSQL is every list v172 has for a person to read, as (list, what, detail).
// Run it at a plant before a deploy; the migration logs the same rows.
var FamilyCensusSQL = `
WITH RECURSIVE scope AS (SELECT k.node_id FROM kanbans k),` + kanbanPictureCTEs + `,
fp AS (
	SELECT bt.id, bt.code, bt.family_id, LEAST(bt.width_in, bt.length_in) AS s, GREATEST(bt.width_in, bt.length_in) AS l,
	       bt.height_in, bt.required_robot_group, bt.bare_of
	  FROM bin_types bt
),
sized AS (SELECT * FROM fp WHERE s > 0 AND bare_of IS NULL),
slot_admits AS (
	SELECT c.parent_id AS kanban_node_id, c.id AS slot_id, c.name AS slot, bt.id AS bin_type_id, bt.family_id
	  FROM nodes c
	  JOIN scope ON scope.node_id = c.parent_id
	  JOIN LATERAL (
		SELECT t.bin_type_id FROM node_bin_types t WHERE t.node_id = c.id
		UNION ALL
		SELECT t.bin_type_id FROM node_bin_types t WHERE t.node_id = c.parent_id
		   AND NOT EXISTS (SELECT 1 FROM node_bin_types own WHERE own.node_id = c.id)
	  ) typed ON TRUE
	  JOIN bin_types bt ON bt.id = typed.bin_type_id
	 WHERE c.enabled
),
room AS (
	SELECT c.parent_id AS kanban_node_id, f.family_id, COUNT(*) AS room
	  FROM nodes c
	  JOIN scope ON scope.node_id = c.parent_id
	  JOIN (SELECT DISTINCT kanban_node_id, family_id FROM mix_family) f ON f.kanban_node_id = c.parent_id
	 WHERE c.enabled AND (EXISTS (SELECT 1 FROM slot_admits a WHERE a.slot_id = c.id AND a.family_id = f.family_id)
	       OR NOT EXISTS (SELECT 1 FROM slot_admits a WHERE a.slot_id = c.id))
	 GROUP BY c.parent_id, f.family_id
)
SELECT 'family_near_footprint', a.code || ' / ' || b.code, a.s || 'x' || a.l || ' and ' || b.s || 'x' || b.l || ' are under an inch apart'
  FROM sized a JOIN sized b ON a.id < b.id AND a.family_id <> b.family_id
   AND ABS(a.s - b.s) < 1 AND ABS(a.l - b.l) < 1
UNION ALL
SELECT 'family_footprint_disagrees', f.code, string_agg(DISTINCT a.code || '=' || a.s || 'x' || a.l, ', ')
  FROM sized a JOIN carrier_families f ON f.id = a.family_id
 GROUP BY f.id, f.code HAVING COUNT(DISTINCT (a.s, a.l)) > 1
UNION ALL
SELECT 'family_robot_group_disagrees', f.code, string_agg(DISTINCT a.code || '=' || a.required_robot_group, ', ')
  FROM fp a JOIN carrier_families f ON f.id = a.family_id WHERE a.bare_of IS NULL
 GROUP BY f.id, f.code HAVING COUNT(DISTINCT a.required_robot_group) > 1
UNION ALL
SELECT 'marker_family_differs', m.code, 'carrier ' || c.code
  FROM bin_types m JOIN bin_types c ON c.id = m.bare_of WHERE m.family_id <> c.family_id
UNION ALL
SELECT 'slot_partial_family', sa.slot, f.code || ': admits ' || COUNT(DISTINCT sa.bin_type_id) || ' of the ' ||
       (SELECT COUNT(*) FROM kanban_levels l JOIN bin_types lt ON lt.id = l.bin_type_id
         WHERE l.kanban_node_id = sa.kanban_node_id AND lt.family_id = sa.family_id) || ' it levels'
  FROM slot_admits sa
  JOIN kanban_levels l ON l.kanban_node_id = sa.kanban_node_id AND l.bin_type_id = sa.bin_type_id
  JOIN carrier_families f ON f.id = sa.family_id
 GROUP BY sa.slot, sa.kanban_node_id, sa.family_id, f.code
HAVING COUNT(DISTINCT sa.bin_type_id) < (SELECT COUNT(*) FROM kanban_levels l2 JOIN bin_types lt2 ON lt2.id = l2.bin_type_id
         WHERE l2.kanban_node_id = sa.kanban_node_id AND lt2.family_id = sa.family_id)
UNION ALL
SELECT 'pool_over_room', g.name, f.code || ': pool ' || kf.pool || ', room ' || COALESCE(r.room, 0)
  FROM kanban_families kf
  JOIN nodes g ON g.id = kf.kanban_node_id
  JOIN carrier_families f ON f.id = kf.family_id
  LEFT JOIN room r ON r.kanban_node_id = kf.kanban_node_id AND r.family_id = kf.family_id
 WHERE COALESCE(r.room, 0) < kf.pool
UNION ALL
SELECT 'pool_under_minimum', g.name, f.code || ': pool ' || kf.pool || ', minimum ' || (COALESCE(mf.mix, 0) + COALESCE(lf.positions, 0))
  FROM kanban_families kf
  JOIN nodes g ON g.id = kf.kanban_node_id
  JOIN carrier_families f ON f.id = kf.family_id
  LEFT JOIN mix_family mf ON mf.kanban_node_id = kf.kanban_node_id AND mf.family_id = kf.family_id
  LEFT JOIN line_family lf ON lf.kanban_node_id = kf.kanban_node_id AND lf.family_id = kf.family_id
 WHERE kf.pool < COALESCE(mf.mix, 0) + COALESCE(lf.positions, 0)
UNION ALL
SELECT DISTINCT 'claim_position_unsupported', g.name, sc.process_id || '/' || sc.style_id || ': ' || extra.node
  FROM style_claims sc
  JOIN positions pos ON pos.node_name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
  JOIN nodes g ON g.id = pos.kanban_node_id
  CROSS JOIN LATERAL (VALUES (sc.core_node_name), (sc.paired_core_node), (sc.second_paired_core_node)) AS extra(node)
 WHERE sc.role = '` + string(protocol.ClaimRoleProduce) + `' AND COALESCE(extra.node, '') <> ''
   AND NOT EXISTS (SELECT 1 FROM positions p2 WHERE p2.kanban_node_id = pos.kanban_node_id AND p2.node_name = extra.node)
UNION ALL
SELECT DISTINCT 'claimed_type_not_levelled', g.name, bt.code
  FROM style_part sp
  JOIN bin_types bt ON bt.id = sp.bin_type_id
  JOIN style_claims sc ON sc.process_id = sp.process_id AND sc.style_id = sp.style_id AND sc.payload_code = sp.payload_code
  JOIN positions pos ON pos.node_name IN (sc.core_node_name, sc.paired_core_node, sc.second_paired_core_node)
  JOIN nodes g ON g.id = pos.kanban_node_id
 WHERE NOT EXISTS (SELECT 1 FROM kanban_levels l WHERE l.kanban_node_id = pos.kanban_node_id AND l.bin_type_id = bt.id)
UNION ALL
SELECT 'unloader_outbound_unresolved', l.name, l.outbound_dest
  FROM bin_loaders l LEFT JOIN nodes n ON n.name = l.outbound_dest
 WHERE l.archived_at IS NULL AND COALESCE(l.outbound_dest, '') <> '' AND n.id IS NULL
UNION ALL
SELECT 'episode_key_unresolved', d.episode_key, d.origin_id::text
  FROM demand_origins d
 WHERE d.kind = 'kanban' AND ` + helpers.DemandOpenSQL("d") + ` AND d.episode_key !~ '^mnt\|[0-9]+\|[0-9]+$'`

// v172Families adds the family and the pool, and moves open kanban episodes' keys to ids.
func v172Families(tx *sql.Tx) error {
	// The step that drops running_cover: each kanban's typed cover is listed before it, beside
	// the minimum computed from the claims, so re-entering it as a pool is a decision.
	const dropsAt = 14
	steps := []string{
		`CREATE TABLE IF NOT EXISTS carrier_families (
			id          BIGSERIAL PRIMARY KEY,
			code        TEXT NOT NULL UNIQUE,
			description TEXT NOT NULL DEFAULT '',
			created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`,
		`ALTER TABLE bin_types ADD COLUMN IF NOT EXISTS family_id BIGINT REFERENCES carrier_families(id) ON DELETE RESTRICT`,
		// 1. One family per distinct normalised footprint among sized, non-bare types, coded
		// by the footprint, spelled as domain.Footprint.String spells it.
		`INSERT INTO carrier_families (code)
		 SELECT DISTINCT footprintCode.code FROM bin_types bt
		  CROSS JOIN LATERAL (SELECT trim_scale(LEAST(bt.width_in, bt.length_in)::numeric)::text || 'x' ||
		                             trim_scale(GREATEST(bt.width_in, bt.length_in)::numeric)::text AS code) footprintCode
		  WHERE bt.family_id IS NULL AND bt.bare_of IS NULL AND bt.width_in > 0 AND bt.length_in > 0
		 ON CONFLICT (code) DO NOTHING`,
		`UPDATE bin_types bt SET family_id = f.id
		   FROM carrier_families f
		  WHERE bt.family_id IS NULL AND bt.bare_of IS NULL AND bt.width_in > 0 AND bt.length_in > 0
		    AND f.code = trim_scale(LEAST(bt.width_in, bt.length_in)::numeric)::text || 'x' ||
		                 trim_scale(GREATEST(bt.width_in, bt.length_in)::numeric)::text`,
		// 2. A family of ONE for every remaining type, coded by the type's code. A code a
		// footprint family already holds takes the type's id with it rather than joining.
		`INSERT INTO carrier_families (code)
		 SELECT CASE WHEN EXISTS (SELECT 1 FROM carrier_families c WHERE c.code = bt.code)
		             THEN bt.code || '-' || bt.id ELSE bt.code END
		   FROM bin_types bt WHERE bt.family_id IS NULL AND bt.bare_of IS NULL
		 ON CONFLICT (code) DO NOTHING`,
		`UPDATE bin_types bt SET family_id = f.id
		   FROM carrier_families f
		  WHERE bt.family_id IS NULL AND bt.bare_of IS NULL
		    AND f.code = CASE WHEN EXISTS (SELECT 1 FROM carrier_families c WHERE c.code = bt.code || '-' || bt.id)
		                      THEN bt.code || '-' || bt.id ELSE bt.code END`,
		// 3. Every bare marker takes its carrier's.
		`UPDATE bin_types m SET family_id = c.family_id
		   FROM bin_types c WHERE c.id = m.bare_of AND m.family_id IS DISTINCT FROM c.family_id`,
		// 4. No NULL arm anywhere, and whoever inserts a type names a family or takes one.
		`ALTER TABLE bin_types ALTER COLUMN family_id SET NOT NULL`,
		`CREATE INDEX IF NOT EXISTS idx_bin_types_family ON bin_types(family_id)`,
		`CREATE OR REPLACE FUNCTION bin_types_family_of_one() RETURNS trigger AS $body$
		 DECLARE
		   family_code TEXT := NEW.code;
		 BEGIN
		   IF NEW.family_id IS NOT NULL THEN
		     RETURN NEW;
		   END IF;
		   IF NEW.bare_of IS NOT NULL THEN
		     SELECT family_id INTO NEW.family_id FROM bin_types WHERE id = NEW.bare_of;
		     RETURN NEW;
		   END IF;
		   -- A code already held is an insert about to conflict: it makes no family.
		   SELECT family_id INTO NEW.family_id FROM bin_types WHERE code = NEW.code;
		   IF NEW.family_id IS NOT NULL THEN
		     RETURN NEW;
		   END IF;
		   IF EXISTS (SELECT 1 FROM carrier_families WHERE code = family_code) THEN
		     family_code := NEW.code || '-' || NEW.id;
		   END IF;
		   -- Two first inserts of one code race here, and only one of them will stand.
		   INSERT INTO carrier_families (code) VALUES (family_code)
		     ON CONFLICT (code) DO UPDATE SET updated_at = NOW() RETURNING id INTO NEW.family_id;
		   RETURN NEW;
		 END
		 $body$ LANGUAGE plpgsql`,
		`DROP TRIGGER IF EXISTS bin_types_family_of_one ON bin_types`,
		`CREATE TRIGGER bin_types_family_of_one BEFORE INSERT ON bin_types
		   FOR EACH ROW EXECUTE FUNCTION bin_types_family_of_one()`,
		`CREATE TABLE IF NOT EXISTS kanban_families (
			kanban_node_id BIGINT NOT NULL REFERENCES kanbans(node_id) ON DELETE CASCADE,
			family_id      BIGINT NOT NULL REFERENCES carrier_families(id) ON DELETE RESTRICT,
			pool           INTEGER NOT NULL CHECK (pool >= 0),
			created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
			PRIMARY KEY (kanban_node_id, family_id)
		)`,
		// Cover comes from the claims and a kanban has no overflow. Each was listed above.
		`ALTER TABLE kanbans DROP COLUMN IF EXISTS running_cover`,
		`ALTER TABLE kanbans DROP COLUMN IF EXISTS overflow_node_id`,
		`ALTER TABLE kanban_levels DROP COLUMN IF EXISTS overflow_node_id`,
		// The open episodes' keys. Ids from now on (protocol.KanbanEpisodeKey). A key that
		// resolves to nothing stays as it is and the census names it.
		`UPDATE demand_origins d SET episode_key = 'mnt|' || n.id || '|' || bt.id
		   FROM nodes n, bin_types bt
		  WHERE d.kind = 'kanban' AND ` + helpers.DemandOpenSQL("d") + ` AND d.episode_key !~ '^mnt\|[0-9]+\|[0-9]+$'
		    AND n.name = split_part(d.episode_key, '|', 2) AND bt.code = split_part(d.episode_key, '|', 3)`,
	}
	for i, s := range steps {
		if i == dropsAt {
			if err := logRetiredSettings(tx); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(s); err != nil {
			return fmt.Errorf("v172 families: %w", err)
		}
	}
	return logFamilyCensus(tx)
}

// logRetiredSettings lists what the drops take: a typed cover and an overflow.
func logRetiredSettings(tx *sql.Tx) error {
	if !schema.ColumnExists(tx, "kanbans", "running_cover") {
		return nil
	}
	rows, err := tx.Query(`SELECT g.name, k.running_cover, COALESCE(o.name, '')
		  FROM kanbans k JOIN nodes g ON g.id = k.node_id LEFT JOIN nodes o ON o.id = k.overflow_node_id ORDER BY g.name`)
	if err != nil {
		return fmt.Errorf("v172 retired settings: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name, overflow string
		var cover int
		if err := rows.Scan(&name, &cover, &overflow); err != nil {
			return fmt.Errorf("v172 retired settings: scan: %w", err)
		}
		log.Printf("migrations: v172 families: retired: %s: running_cover %d, overflow %q", name, cover, overflow)
	}
	return rows.Err()
}

func logFamilyCensus(tx *sql.Tx) error {
	rows, err := tx.Query(FamilyCensusSQL)
	if err != nil {
		return fmt.Errorf("v172 census: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var list, what, detail string
		if err := rows.Scan(&list, &what, &detail); err != nil {
			return fmt.Errorf("v172 census: scan: %w", err)
		}
		log.Printf("migrations: v172 families: %s: %s: %s", list, what, detail)
	}
	return rows.Err()
}

// v172Done is the post-condition.
func v172Done(q schema.Querier) bool {
	return schema.TableExists(q, "carrier_families") && schema.TableExists(q, "kanban_families") &&
		schema.ColumnExists(q, "bin_types", "family_id") && !schema.ColumnNullable(q, "bin_types", "family_id") &&
		schema.IndexExists(q, "idx_bin_types_family") &&
		schema.ColumnAbsent(q, "kanbans", "running_cover") && schema.ColumnAbsent(q, "kanbans", "overflow_node_id") &&
		schema.ColumnAbsent(q, "kanban_levels", "overflow_node_id")
}
