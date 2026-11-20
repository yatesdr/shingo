package store

import (
	"database/sql"
	"fmt"
	"log"
	"maps"
	"slices"
	"strings"

	"shingocore/store/schema"
)

// v185 — the clean column model (the data-model attack, round 2; the build's
// END-STATE: the renames table, the A9 rulings, the move rules).
//
// A column holds a station uid if and only if it is named station_uid or ends
// _station_uid; Core's three actors (core-operator, core-direct, core-test)
// leave the station columns for requested_by; an RDS map name is rds_station;
// the outbox's addressee is dst_station; the columns and tables with no reader
// go. Every op is a row of v185Ops, so its down step can be generated from the
// same table.
//
// ONE TRANSACTION, in this order (the runner's, runOne):
//  1. SET LOCAL statement_timeout = 0: the moves can outrun the DSN's 30 s cap
//     at plant volume; the 3 s lock_timeout stays.
//  2. LOCK TABLE on every table it touches, ACCESS EXCLUSIVE, before anything
//     else: a reader holding one makes it fail on the lock (55P03) before a
//     single row moves, and the next boot tries again.
//  3. Renames, adds and the drops no move reads.
//  4. The data moves (v185Moves), the registry fold among them.
//  5. The drop a move read (edge_registry.station_id), then the CHECKs and the
//     total UNIQUE on edge_registry.station_uid.
//  6. store/schema/comments.sql, the rationale under the end-state names.
//
// IDEMPOTENT, because the runner re-runs a migration whose verify fails: each
// rename runs only while its old name is there, each add and drop is IF [NOT]
// EXISTS, each move's WHERE excludes the rows it has already moved, and each
// constraint is added only while absent.

// v185Actors are the three Core actors: the order senders that are not a
// station (www's manual, direct and test doors).
var v185Actors = []string{"core-operator", "core-direct", "core-test"}

// The move lists, exported so TestV185_MovesReadTheCensusLists can hold them
// equal to the step-0 census's (service/uid_under_actor_census_test.go). Each
// is what the census found writes a station uid where the clean model says a
// who, or the other way round.
var (
	// V185LedgerUIDOps are the bin_uop_ledger ops whose actor holds the
	// station uid: it moves to station_uid and actor becomes ''. edge_rollback
	// already holds the uid in station too, so only its actor clears.
	V185LedgerUIDOps = []string{"after_lift", "bin_uop_delta", "edge_rollback", "payload_bound_first_delta",
		"payload_mismatch_dropped", "payload_rebound_with_inventory", "stale_epoch_dropped"}
	// V185ExceptionUIDOps is, per bin_uop_exception kind whose actor holds the
	// uid, the ops it does so on (nil: any op). Keyed on (kind, op): a boundary
	// row holds a uid only on edge_rollback, and v93's backfilled
	// negative_crossing rows carry other ops and actors.
	V185ExceptionUIDOps = map[string][]string{
		"boundary":          {"edge_rollback"},
		"edge_rollback":     {"edge_rollback"},
		"negative_crossing": {"bin_uop_delta"},
		"payload_mismatch":  {"payload_mismatch_dropped"},
		"report_divergence": nil,
		"stale_epoch":       {"stale_epoch_dropped"},
	}
	// V185ExceptionDetailUID is each exception kind that holds the uid in a
	// detail key, and the key: it moves to station_uid and the key goes.
	V185ExceptionDetailUID = map[string]string{"hold_dropped": "station"}
	// V185LedgerActorStationOps are the bin_uop_ledger ops whose station can
	// hold a Core actor (an aware actor order's material station): the actor
	// is a who, so it moves to actor, where 'system' stood, and station_uid
	// becomes ''.
	V185LedgerActorStationOps = []string{"lifted"}
)

type v185Rename struct{ table, from, to string }

// v185Relabel renames an index (constraint false) or a constraint and the
// index under it (constraint true).
type v185Relabel struct {
	table, from, to string
	constraint      bool
}

type v185Add struct{ table, column, def string }

// v185Drop is a column with no reader. restore is its v184 definition, and
// after the statements down runs once it is back (its foreign key).
type v185Drop struct {
	table, column, restore string
	after                  []string
}

// v185DropTable is a table with no writer; restore recreates it in its v184
// shape, statement by statement.
type v185DropTable struct {
	table   string
	restore []string
}

// v185Move is one data move: up runs on v185 names after the renames, down on
// v184 names after the renames back.
type v185Move struct{ name, up, down string }

// v185Check is a constraint up adds last and down drops first.
type v185Check struct{ table, name, def string }

var v185Ops = struct {
	renames    []v185Rename
	relabels   []v185Relabel
	adds       []v185Add
	drops      []v185Drop
	dropTables []v185DropTable
	checks     []v185Check
}{
	renames: []v185Rename{
		{"orders", "station_id", "station_uid"},
		{"orders", "projects_to", "projects_to_station_uid"},
		{"mission_telemetry", "station_id", "station_uid"},
		{"cell_config", "station", "station_uid"},
		{"cell_config", "cell_id", "cell_label"},
		{"process_styles", "station_id", "station_uid"},
		{"bin_loaders", "edge_station_uid", "station_uid"},
		{"demand_registry", "station_id", "station_uid"},
		{"demand_origins", "station_id", "station_uid"},
		{"node_stations", "station_id", "station_uid"},
		{"edge_cells", "station", "station_uid"},
		{"edge_lineside_reports", "station", "station_uid"},
		{"inventory_delta_dedup", "station", "station_uid"},
		{"lineside_buckets", "station", "station_uid"},
		{"bin_uop_ledger", "station", "station_uid"},
		{"plant_claims_reports", "station_id", "station_uid"},
		{"kanbans", "station_id", "station_uid"},
		{"cell_part_events", "cell_id", "station_uid"},
		{"bin_uop_delta_daily", "actor", "station_uid"},
		{"outbox", "station_id", "dst_station"},
		{"mission_events", "robot_station", "rds_station"},
		{"robot_confidence_samples", "station", "rds_station"},
		{"robot_confidence_samples", "last_station", "last_rds_station"},
		{"robot_confidence_low", "station", "rds_station"},
		{"robot_confidence_low", "last_station", "last_rds_station"},
		{"robot_lidar_hours", "station", "rds_station"},
	},
	relabels: []v185Relabel{
		{"cell_config", "cell_config_station_idx", "cell_config_station_uid_idx", false},
		{"demand_registry", "demand_registry_station_id_core_node_name_payload_code_key",
			"demand_registry_station_uid_core_node_name_payload_code_key", true},
		{"edge_cells", "idx_edge_cells_station", "idx_edge_cells_station_uid", false},
		{"mission_telemetry", "idx_mission_telemetry_station", "idx_mission_telemetry_station_uid", false},
		{"node_stations", "idx_node_stations_station", "idx_node_stations_station_uid", false},
		{"cell_part_events", "idx_cell_part_events_cell_time", "idx_cell_part_events_station_uid_time", false},
	},
	adds: []v185Add{
		{"orders", "requested_by", "TEXT NOT NULL DEFAULT ''"},
		{"mission_telemetry", "requested_by", "TEXT NOT NULL DEFAULT ''"},
		{"bin_uop_exception", "station_uid", "TEXT NOT NULL DEFAULT ''"},
	},
	drops: []v185Drop{
		{table: "inbox", column: "station_id", restore: "text DEFAULT ''::text NOT NULL"},
		{table: "supply_refusals", column: "station_id", restore: "text DEFAULT ''::text NOT NULL"},
		{table: "order_intake_refusals", column: "station_id", restore: "text DEFAULT ''::text NOT NULL"},
		{table: "bin_loader_homes", column: "min_stock", restore: "integer DEFAULT 0 NOT NULL"},
		{table: "bin_loader_payloads", column: "min_stock", restore: "integer DEFAULT 0 NOT NULL"},
		{table: "demand_origins", column: "parent_origin_id", restore: "uuid"},
		{table: "demand_origins", column: "uop_delivered", restore: "integer DEFAULT 0 NOT NULL"},
		{table: "reservations", column: "expires_at", restore: "timestamp with time zone"},
		{table: "scene_diffs", column: "renames", restore: "jsonb"},
		{table: "scene_reflectors", column: "supersedes_id", restore: "bigint", after: []string{
			`ALTER TABLE ONLY public.scene_reflectors ADD CONSTRAINT scene_reflectors_supersedes_id_fkey
				FOREIGN KEY (supersedes_id) REFERENCES public.scene_reflectors(id)`}},
		{table: "carrier_families", column: "description", restore: "text DEFAULT ''::text NOT NULL"},
	},
	dropTables: []v185DropTable{
		{"cell_targets", []string{
			`CREATE TABLE public.cell_targets (
				cell_id text NOT NULL,
				payload_code text DEFAULT ''::text NOT NULL,
				target_cycle_ms bigint DEFAULT 0 NOT NULL,
				owner text DEFAULT ''::text NOT NULL,
				updated_at timestamp with time zone DEFAULT now() NOT NULL)`,
			`ALTER TABLE ONLY public.cell_targets ADD CONSTRAINT cell_targets_pkey PRIMARY KEY (cell_id, payload_code)`,
		}},
		{"downtime_event_dedup", []string{
			`CREATE TABLE public.downtime_event_dedup (
				station text NOT NULL,
				edge_event_id bigint NOT NULL,
				applied_at timestamp with time zone DEFAULT now() NOT NULL)`,
			`ALTER TABLE ONLY public.downtime_event_dedup ADD CONSTRAINT downtime_event_dedup_pkey PRIMARY KEY (station, edge_event_id)`,
		}},
		{"downtime_events", []string{
			`CREATE TABLE public.downtime_events (
				id bigint NOT NULL,
				station text NOT NULL,
				plc_name text NOT NULL,
				reason text DEFAULT ''::text NOT NULL,
				started_at timestamp with time zone NOT NULL,
				ended_at timestamp with time zone,
				duration_ms bigint DEFAULT 0 NOT NULL,
				edge_event_id bigint DEFAULT 0 NOT NULL)
			PARTITION BY RANGE (started_at)`,
			`CREATE SEQUENCE public.downtime_events_id_seq START WITH 1 INCREMENT BY 1 NO MINVALUE NO MAXVALUE CACHE 1`,
			`ALTER SEQUENCE public.downtime_events_id_seq OWNED BY public.downtime_events.id`,
			`ALTER TABLE ONLY public.downtime_events ALTER COLUMN id SET DEFAULT nextval('public.downtime_events_id_seq'::regclass)`,
			`CREATE INDEX idx_downtime_events_station_time ON ONLY public.downtime_events USING btree (station, started_at)`,
		}},
	},
	checks: []v185Check{
		{"orders", "orders_requested_by_known", "CHECK (requested_by IN (" + v185ActorList("''") + "))"},
		{"orders", "orders_station_uid_not_actor", "CHECK (station_uid NOT IN (" + v185ActorList("") + "))"},
		{"mission_telemetry", "mission_telemetry_requested_by_known", "CHECK (requested_by IN (" + v185ActorList("''") + "))"},
		{"mission_telemetry", "mission_telemetry_station_uid_not_actor", "CHECK (station_uid NOT IN (" + v185ActorList("") + "))"},
		{"edge_registry", "edge_registry_station_uid_key", "UNIQUE (station_uid)"},
		{"edge_registry", "edge_registry_station_uid_nonblank", "CHECK (station_uid <> '')"},
	},
}

// v185PartitionTokens are, per partitioned table whose renamed column its
// children's indexes are named after, that column's name before and now. The
// children are made at runtime (store/heartbeat), so their index names are
// found in the catalog, not in this table (v185RelabelChildren).
var v185PartitionTokens = []struct{ parent, before, now string }{
	{"cell_part_events", "cell_id", "station_uid"},
}

// v185Moves are the data moves, in order. Up's statements read v185 names,
// down's v184 names. Each up WHERE excludes a row already moved, so a re-run
// moves nothing twice; each down inverts exactly the rows the matching up
// produces, and the rows the clean model's writers write.
var v185Moves = []v185Move{
	{"Core actors leave orders.station_uid for requested_by",
		`UPDATE orders SET requested_by = station_uid, station_uid = '' WHERE station_uid IN (` + v185ActorList("") + `)`,
		`UPDATE orders SET station_id = requested_by WHERE requested_by <> ''`},
	{"Core actors leave mission_telemetry.station_uid for requested_by",
		`UPDATE mission_telemetry SET requested_by = station_uid, station_uid = '' WHERE station_uid IN (` + v185ActorList("") + `)`,
		`UPDATE mission_telemetry SET station_id = requested_by WHERE requested_by <> ''`},
	{"the ledger's uid moves out of actor",
		`UPDATE bin_uop_ledger SET station_uid = actor, actor = '' WHERE op IN (` + v185Quoted(v185LedgerActorOps()) + `)
			AND station_uid = '' AND actor <> ''`,
		`UPDATE bin_uop_ledger SET actor = station, station = '' WHERE op IN (` + v185Quoted(v185LedgerActorOps()) + `)
			AND actor = '' AND station <> ''`},
	{"edge_rollback's actor clears: its station already holds the uid",
		`UPDATE bin_uop_ledger SET actor = '' WHERE op = 'edge_rollback' AND actor <> '' AND actor = station_uid`,
		`UPDATE bin_uop_ledger SET actor = station WHERE op = 'edge_rollback' AND actor = '' AND station <> ''`},
	{"a Core actor in a lifted row's station is a who: it moves to actor",
		`UPDATE bin_uop_ledger SET actor = station_uid, station_uid = '' WHERE op IN (` + v185Quoted(V185LedgerActorStationOps) + `)
			AND station_uid IN (` + v185ActorList("") + `)`,
		`UPDATE bin_uop_ledger SET station = actor, actor = 'system' WHERE op IN (` + v185Quoted(V185LedgerActorStationOps) + `)
			AND station = '' AND actor IN (` + v185ActorList("") + `)`},
	{"the exceptions' uid moves out of actor",
		`UPDATE bin_uop_exception SET station_uid = actor, actor = '' WHERE (` + v185ExceptionUIDWhere() + `)
			AND station_uid = '' AND actor <> ''`,
		`UPDATE bin_uop_exception SET actor = station_uid WHERE (` + v185ExceptionUIDWhere() + `)
			AND actor = '' AND station_uid <> ''`},
	{"hold_dropped's station leaves detail for station_uid",
		`UPDATE bin_uop_exception SET station_uid = COALESCE(detail->>'station', ''), detail = detail - 'station'
			WHERE kind = 'hold_dropped' AND detail ? 'station'`,
		`UPDATE bin_uop_exception SET detail = COALESCE(detail, '{}'::jsonb) || jsonb_build_object('station', station_uid)
			WHERE kind = 'hold_dropped'`},
	{"audit_log floor_create: the JSON key follows the column", v185AuditKeyMove("station", "station_uid"), v185AuditKeyMove("station_uid", "station")},
}

// v185LedgerActorOps are V185LedgerUIDOps but edge_rollback, whose station
// already holds the uid (its own move).
func v185LedgerActorOps() []string {
	return slices.DeleteFunc(slices.Clone(V185LedgerUIDOps), func(op string) bool { return op == "edge_rollback" })
}

func v185ExceptionUIDWhere() string {
	var arms []string
	for _, kind := range slices.Sorted(maps.Keys(V185ExceptionUIDOps)) {
		if ops := V185ExceptionUIDOps[kind]; ops != nil {
			arms = append(arms, fmt.Sprintf("(kind = '%s' AND op IN (%s))", kind, v185Quoted(ops)))
		} else {
			arms = append(arms, fmt.Sprintf("kind = '%s'", kind))
		}
	}
	return strings.Join(arms, " OR ")
}

func v185Quoted(xs []string) string {
	q := make([]string, len(xs))
	for i, x := range xs {
		q[i] = "'" + x + "'"
	}
	return strings.Join(q, ", ")
}

// v185ActorList is the three actors quoted, after first when it is set.
func v185ActorList(first string) string {
	if first == "" {
		return v185Quoted(v185Actors)
	}
	return first + ", " + v185Quoted(v185Actors)
}

// v185Tables are the tables v185 touches, in a fixed order: the ones it
// locks before its first move. present filters out the tables down restores.
func v185Tables() []string {
	seen := map[string]bool{}
	var out []string
	add := func(t string) {
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	for _, r := range v185Ops.renames {
		add(r.table)
	}
	for _, r := range v185Ops.relabels {
		add(r.table)
	}
	for _, a := range v185Ops.adds {
		add(a.table)
	}
	for _, d := range v185Ops.drops {
		add(d.table)
	}
	for _, c := range v185Ops.checks {
		add(c.table)
	}
	add("audit_log") // its only change is a move
	return out
}

func v185Exec(tx *sql.Tx, step, q string) error {
	if _, err := tx.Exec(q); err != nil {
		return fmt.Errorf("v185 %s: %w", step, err)
	}
	return nil
}

func v185Lock(tx *sql.Tx, tables []string) error {
	return v185Exec(tx, "lock", "LOCK TABLE "+strings.Join(tables, ", ")+" IN ACCESS EXCLUSIVE MODE")
}

// v185Up is migration 185's apply function.
func v185Up(tx *sql.Tx) error {
	if err := v185Exec(tx, "statement_timeout", `SET LOCAL statement_timeout = 0`); err != nil {
		return err
	}
	var present []string
	for _, t := range v185Tables() {
		if schema.TableExists(tx, t) {
			present = append(present, t)
		}
	}
	for _, d := range v185Ops.dropTables {
		if schema.TableExists(tx, d.table) {
			present = append(present, d.table)
		}
	}
	if err := v185Lock(tx, present); err != nil {
		return err
	}
	// 3. Renames, adds, and the drops no move reads.
	for _, r := range v185Ops.renames {
		if schema.ColumnExists(tx, r.table, r.from) {
			if err := v185Exec(tx, "rename "+r.table+"."+r.from,
				fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.from, r.to)); err != nil {
				return err
			}
		}
	}
	for _, r := range v185Ops.relabels {
		if err := v185Relabel1(tx, r.table, r.from, r.to, r.constraint); err != nil {
			return err
		}
	}
	for _, p := range v185PartitionTokens {
		if err := v185RelabelChildren(tx, p.parent, p.now, p.before); err != nil {
			return err
		}
	}
	for _, a := range v185Ops.adds {
		if err := v185Exec(tx, "add "+a.table+"."+a.column,
			fmt.Sprintf(`ALTER TABLE %s ADD COLUMN IF NOT EXISTS %s %s`, a.table, a.column, a.def)); err != nil {
			return err
		}
	}
	for _, d := range v185Ops.drops {
		if err := v185Exec(tx, "drop "+d.table+"."+d.column,
			fmt.Sprintf(`ALTER TABLE %s DROP COLUMN IF EXISTS %s`, d.table, d.column)); err != nil {
			return err
		}
	}
	for _, d := range v185Ops.dropTables {
		if err := v185Exec(tx, "drop "+d.table, `DROP TABLE IF EXISTS `+d.table); err != nil {
			return err
		}
	}
	// 4. The moves, the registry fold last among them.
	if err := v185Exec(tx, "the JSON guard", v185JSONGuardOn); err != nil {
		return err
	}
	for _, m := range v185Moves {
		if err := v185Exec(tx, m.name, m.up); err != nil {
			return err
		}
	}
	if err := v185JSONGuardOff(tx); err != nil {
		return err
	}
	if err := v185FoldRegistry(tx); err != nil {
		return err
	}
	// 5. The drop the fold read, then the constraints.
	if err := v185Exec(tx, "drop edge_registry.station_id", `ALTER TABLE edge_registry DROP COLUMN IF EXISTS station_id`); err != nil {
		return err
	}
	if !schema.ConstraintExists(tx, "edge_registry", "edge_registry_station_uid_key") {
		// The partial unique index of the same name, which the constraint replaces.
		if err := v185Exec(tx, "drop the partial uid index", `DROP INDEX IF EXISTS edge_registry_station_uid_key`); err != nil {
			return err
		}
	}
	for _, c := range v185Ops.checks {
		if schema.ConstraintExists(tx, c.table, c.name) {
			continue
		}
		if err := v185Exec(tx, "constraint "+c.name,
			fmt.Sprintf(`ALTER TABLE %s ADD CONSTRAINT %s %s`, c.table, c.name, c.def)); err != nil {
			return err
		}
	}
	// 6. The rationale comments, under the end-state names, so a plant database
	// carries what a new one is born with.
	return v185Exec(tx, "comments", schema.Comments)
}

// v185Relabel1 renames index or constraint from to to on table, while from is
// there.
func v185Relabel1(tx *sql.Tx, table, from, to string, constraint bool) error {
	if constraint {
		if !schema.ConstraintExists(tx, table, from) {
			return nil
		}
		return v185Exec(tx, "rename constraint "+from, fmt.Sprintf(`ALTER TABLE %s RENAME CONSTRAINT %s TO %s`, table, from, to))
	}
	if !schema.IndexExists(tx, from) {
		return nil
	}
	return v185Exec(tx, "rename index "+from, fmt.Sprintf(`ALTER INDEX %s RENAME TO %s`, from, to))
}

// v185RelabelChildren renames each index of parent's partitions that carries
// the name Postgres gave it for its columns, with colNow in them read as
// colBefore, to the name Postgres gives it for its columns as they are now.
// Up calls it after the column rename (colNow station_uid, colBefore cell_id),
// down after the rename back: so a renamed child index is named as a partition
// created after the rename would be, truncation included, and down restores
// the name exactly. A child index named by hand is left alone.
func v185RelabelChildren(tx *sql.Tx, parent, colNow, colBefore string) error {
	rows, err := tx.Query(`SELECT ic.relname, c.relname,
		       (SELECT string_agg(a.attname, '_' ORDER BY k.ord)
		          FROM unnest(x.indkey) WITH ORDINALITY AS k(attnum, ord)
		          JOIN pg_attribute a ON a.attrelid = x.indrelid AND a.attnum = k.attnum)
		  FROM pg_inherits i
		  JOIN pg_class p ON p.oid = i.inhparent AND p.relname = $1
		  JOIN pg_class c ON c.oid = i.inhrelid
		  JOIN pg_index x ON x.indrelid = i.inhrelid
		  JOIN pg_class ic ON ic.oid = x.indexrelid
		 ORDER BY ic.relname`, parent)
	if err != nil {
		return fmt.Errorf("v185 list %s's partition indexes: %w", parent, err)
	}
	type childIndex struct{ name, table, cols string }
	var idx []childIndex
	for rows.Next() {
		var c childIndex
		if err := rows.Scan(&c.name, &c.table, &c.cols); err != nil {
			rows.Close()
			return err
		}
		idx = append(idx, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range idx {
		// The column list as it read before the rename: colNow, as a whole
		// column name between underscores, read as colBefore.
		before := strings.Trim(strings.ReplaceAll("_"+c.cols+"_", "_"+colNow+"_", "_"+colBefore+"_"), "_")
		if before == c.cols || c.name != pgObjectName(c.table, before, "idx") {
			continue
		}
		to := pgObjectName(c.table, c.cols, "idx")
		if err := v185Exec(tx, "rename partition index "+c.name, fmt.Sprintf(`ALTER INDEX %s RENAME TO %s`, c.name, to)); err != nil {
			return err
		}
	}
	return nil
}

// pgObjectName is the name Postgres makes for an object from name1, name2 and
// label (makeObjectName, src/backend/commands/indexcmds.c): name1_name2_label,
// the longer of name1 and name2 shortened a byte at a time until the whole
// fits in 63 bytes. A partition's index is named for its table and its
// columns joined with _, labelled idx. ASCII names only.
func pgObjectName(name1, name2, label string) string {
	avail := 63 - (1 + len(label) + 1)
	n1, n2 := len(name1), len(name2)
	for n1+n2 > avail {
		if n1 > n2 {
			n1--
		} else {
			n2--
		}
	}
	return name1[:n1] + "_" + name2[:n2] + "_" + label
}

// v185FoldRegistry gives every edge_registry row its uid: a blank station_uid
// takes the row's station_id, its routing selector, whose value is the uid. A
// row that cannot take one is deleted and logged (it is no good Edge's: it has
// no identity to keep). Two kinds: a blank-uid row whose station_id is another
// row's uid (the fold collision: the row that holds the uid is kept), and a
// row whose uid and station_id are both blank.
func v185FoldRegistry(tx *sql.Tx) error {
	if !schema.ColumnExists(tx, "edge_registry", "station_id") {
		return nil
	}
	rows, err := tx.Query(`SELECT r.id, r.station_id FROM edge_registry r
		 WHERE r.station_uid = ''
		   AND (r.station_id = '' OR EXISTS (SELECT 1 FROM edge_registry s WHERE s.id <> r.id AND s.station_uid = r.station_id))
		 ORDER BY r.id`)
	if err != nil {
		return fmt.Errorf("v185 find the registry fold's collisions: %w", err)
	}
	type gone struct {
		id        int64
		stationID string
	}
	var drop []gone
	for rows.Next() {
		var g gone
		if err := rows.Scan(&g.id, &g.stationID); err != nil {
			rows.Close()
			return err
		}
		drop = append(drop, g)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, g := range drop {
		why := "its station_id " + g.stationID + " is another row's station_uid"
		if g.stationID == "" {
			why = "its station_uid and station_id are both blank"
		}
		log.Printf("v185: edge_registry row id=%d station_id=%q deleted by the fold: %s", g.id, g.stationID, why)
		if _, err := tx.Exec(`DELETE FROM edge_registry WHERE id = $1`, g.id); err != nil {
			return fmt.Errorf("v185 delete registry row %d: %w", g.id, err)
		}
	}
	return v185Exec(tx, "fold the registry", `UPDATE edge_registry SET station_uid = station_id WHERE station_uid = ''`)
}

// v185Verify is v185's post-condition, and only the part a re-run of
// v185Up restores: every column rename, add and drop, and every dropped table.
// The runner re-runs a migration whose verify fails on every boot, and a
// re-run takes ACCESS EXCLUSIVE on every table v185 touches; one that cannot
// repair what failed would do that forever. A missing index or constraint
// name (a hand-dropped index, say) is the boot's drift report's to name, not
// this verify's (TestV185_AHandDroppedIndexIsDriftNotARerun). Catalog reads
// only.
func v185Verify(q schema.Querier) bool {
	ok := true
	for _, r := range v185Ops.renames {
		ok = ok && schema.ColumnAbsent(q, r.table, r.from) && schema.ColumnExists(q, r.table, r.to)
	}
	for _, a := range v185Ops.adds {
		ok = ok && schema.ColumnExists(q, a.table, a.column)
	}
	for _, d := range v185Ops.drops {
		ok = ok && schema.ColumnAbsent(q, d.table, d.column)
	}
	for _, d := range v185Ops.dropTables {
		ok = ok && schema.TableAbsent(q, d.table)
	}
	return ok && schema.ColumnAbsent(q, "edge_registry", "station_id")
}

// migrateDown185 puts the v184 shape back, generated from v185Ops and
// comments.sql: the comments up applied removed, the constraints dropped,
// every rename back, every move inverted, the added columns dropped, the dropped columns and tables restored as v184 declared
// them (every default, NOT NULL, key and sequence; edge_registry.station_id
// filled from station_uid), and schema_migrations' row for 185 deleted. One
// transaction, under SET LOCAL statement_timeout = 0 and every table's lock
// first, as up. It refuses unless 185 is the newest version recorded.
//
// What does not come back: the rows the registry fold deleted (logged by up),
// the values of the dropped columns (they come back as their defaults; no
// writer wrote them), audit_log.new_value's original spelling (jsonb
// re-serialises it), the uid the fold gave a blank row, a station_id that was
// not its row's uid (it comes back as the uid), and a partition index name
// set by hand.
func (db *DB) migrateDown185() error {
	var latest int
	if err := db.QueryRow(`SELECT COALESCE(MAX(version), 0) FROM schema_migrations`).Scan(&latest); err != nil {
		return fmt.Errorf("migrate down 185: read schema_migrations: %w", err)
	}
	if latest != 185 {
		return fmt.Errorf("migrate down 185: the newest version recorded is v%d; down 185 runs only on a database whose newest is v185", latest)
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := v185Down(tx); err != nil {
		return fmt.Errorf("migrate down 185: %w", err)
	}
	return tx.Commit()
}

func v185Down(tx *sql.Tx) error {
	if err := v185Exec(tx, "statement_timeout", `SET LOCAL statement_timeout = 0`); err != nil {
		return err
	}
	var present []string
	for _, t := range v185Tables() {
		if schema.TableExists(tx, t) {
			present = append(present, t)
		}
	}
	if err := v185Lock(tx, present); err != nil {
		return err
	}
	// The comments up applied go first, while every object has the name they
	// were written under: a v184 database carried none.
	if err := v185Exec(tx, "comments off", v185CommentsOff(schema.Comments)); err != nil {
		return err
	}
	for i := len(v185Ops.checks) - 1; i >= 0; i-- {
		c := v185Ops.checks[i]
		if err := v185Exec(tx, "drop constraint "+c.name,
			fmt.Sprintf(`ALTER TABLE %s DROP CONSTRAINT IF EXISTS %s`, c.table, c.name)); err != nil {
			return err
		}
	}
	if err := v185Exec(tx, "restore the partial uid index",
		`CREATE UNIQUE INDEX edge_registry_station_uid_key ON public.edge_registry USING btree (station_uid) WHERE (station_uid <> ''::text)`); err != nil {
		return err
	}
	for i := len(v185Ops.renames) - 1; i >= 0; i-- {
		r := v185Ops.renames[i]
		if err := v185Exec(tx, "rename back "+r.table+"."+r.to,
			fmt.Sprintf(`ALTER TABLE %s RENAME COLUMN %s TO %s`, r.table, r.to, r.from)); err != nil {
			return err
		}
	}
	for i := len(v185Ops.relabels) - 1; i >= 0; i-- {
		r := v185Ops.relabels[i]
		if err := v185Relabel1(tx, r.table, r.to, r.from, r.constraint); err != nil {
			return err
		}
	}
	for _, p := range v185PartitionTokens {
		if err := v185RelabelChildren(tx, p.parent, p.before, p.now); err != nil {
			return err
		}
	}
	if err := v185Exec(tx, "the JSON guard", v185JSONGuardOn); err != nil {
		return err
	}
	for i := len(v185Moves) - 1; i >= 0; i-- {
		m := v185Moves[i]
		if err := v185Exec(tx, "invert: "+m.name, m.down); err != nil {
			return err
		}
	}
	if err := v185JSONGuardOff(tx); err != nil {
		return err
	}
	for i := len(v185Ops.adds) - 1; i >= 0; i-- {
		a := v185Ops.adds[i]
		if err := v185Exec(tx, "drop "+a.table+"."+a.column,
			fmt.Sprintf(`ALTER TABLE %s DROP COLUMN %s`, a.table, a.column)); err != nil {
			return err
		}
	}
	for _, q := range []string{
		`ALTER TABLE edge_registry ADD COLUMN station_id text`,
		`UPDATE edge_registry SET station_id = station_uid`,
		`ALTER TABLE edge_registry ALTER COLUMN station_id SET NOT NULL`,
		`ALTER TABLE ONLY public.edge_registry ADD CONSTRAINT edge_registry_station_id_key UNIQUE (station_id)`,
	} {
		if err := v185Exec(tx, "restore edge_registry.station_id", q); err != nil {
			return err
		}
	}
	for _, d := range v185Ops.drops {
		if err := v185Exec(tx, "restore "+d.table+"."+d.column,
			fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, d.table, d.column, d.restore)); err != nil {
			return err
		}
		for _, q := range d.after {
			if err := v185Exec(tx, "restore "+d.table+"."+d.column, q); err != nil {
				return err
			}
		}
	}
	for _, d := range v185Ops.dropTables {
		for _, q := range d.restore {
			if err := v185Exec(tx, "restore "+d.table, q); err != nil {
				return err
			}
		}
	}
	return v185Exec(tx, "unrecord", `DELETE FROM schema_migrations WHERE version = 185`)
}

// v185CommentsOff is comments.sql inverted: each COMMENT ON <object> IS '...'
// becomes COMMENT ON <object> IS NULL.
func v185CommentsOff(comments string) string {
	var out []string
	for _, line := range strings.Split(comments, "\n") {
		if !strings.HasPrefix(line, "COMMENT ON ") {
			continue
		}
		if obj, _, ok := strings.Cut(line, " IS '"); ok {
			out = append(out, obj+" IS NULL;")
		}
	}
	return strings.Join(out, "\n")
}

// v185AuditKeyMove renames the from key of audit_log's floor_create rows to
// to. new_value is text a writer wrote as JSON; a row that does not parse as
// a JSON object is left as it was (v185JSONGuard counts it), so one bad row
// cannot fail the migration on every boot.
func v185AuditKeyMove(from, to string) string {
	return `UPDATE audit_log a SET new_value = ((p.j - '` + from + `') || jsonb_build_object('` + to + `', p.j -> '` + from + `'))::text
		  FROM (SELECT id, pg_temp.v185_jsonb(new_value) AS j FROM audit_log
		         WHERE entity_type = 'payload' AND action = 'floor_create') p
		 WHERE a.id = p.id AND jsonb_typeof(p.j) = 'object' AND p.j ? '` + from + `'`
}

// v185JSONGuardOn creates, for this transaction's session, the function the
// audit_log move reads new_value through: the text as jsonb, or NULL where
// the cast fails, in place of the cast's error.
const v185JSONGuardOn = `CREATE OR REPLACE FUNCTION pg_temp.v185_jsonb(t text) RETURNS jsonb LANGUAGE plpgsql AS $$
BEGIN
	RETURN t::jsonb;
EXCEPTION WHEN others THEN
	RETURN NULL;
END $$`

// v185JSONGuardOff counts the floor_create rows the move left as they were,
// logs the count once, and drops the function.
func v185JSONGuardOff(tx *sql.Tx) error {
	var left int
	if err := tx.QueryRow(`SELECT count(*) FROM audit_log WHERE entity_type = 'payload' AND action = 'floor_create'
		AND jsonb_typeof(pg_temp.v185_jsonb(new_value)) IS DISTINCT FROM 'object'`).Scan(&left); err != nil {
		return fmt.Errorf("v185 count the floor_create rows that are not JSON: %w", err)
	}
	if left > 0 {
		log.Printf("v185: %d audit_log floor_create rows whose new_value is not a JSON object were left as they were", left)
	}
	return v185Exec(tx, "drop the JSON guard", `DROP FUNCTION pg_temp.v185_jsonb(text)`)
}
