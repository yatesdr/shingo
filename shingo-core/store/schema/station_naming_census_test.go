package schema

import (
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// station_naming_census_test.go — the station naming rule over the committed
// snapshot, with every column that breaks it today named with what it becomes.
//
// THE RULE (the clean column model's): a column holds a station uid if and
// only if it is named station_uid or ends _station_uid; no column is named
// station_id or bare station; and station appears in a column name otherwise
// only in stationNamesAllowed. Which columns hold a uid is not in a name, so the
// end state says it: endStateUIDColumns.
//
// It is red on any tree that breaks the rule. knownNamingViolations lists every
// way today's snapshot breaks it, each with the name the column gets (or that
// it goes), and the test passes only when the snapshot's violations are
// exactly those: a migration that fixes one without striking it, or a column
// that breaks the rule anew, fails. The list empties as the migration lands; an
// empty list is the rule itself.
//
// knownStationIndexNames does the same for index and constraint names, whose
// names follow their columns: each one that covers a column on that list, or
// whose name breaks the column rule (indexNameBreaksRule), is listed until the
// migration renames it. A name that carries no column (a _pkey) is listed only
// while it covers a listed column, so it leaves the list with that column.
//
// An end-state uid column that today holds its uid under another name (a
// column that does not say station: cell_part_events.cell_id,
// bin_uop_delta_daily.actor, orders.projects_to) is found from the list's own
// rename: which column holds a uid is the end-state table's word, not a
// name's. That is the one place the list is trusted rather than checked.
//
// PARTITIONS: the snapshot is dumped from a freshly migrated database, which
// has no partition children (they are created per range at runtime, by
// store/heartbeat and store/robotconfidence); a child would be a CREATE TABLE …
// PARTITION OF, or an ALTER … ATTACH PARTITION, and is skipped, its parent
// carrying the columns. The parents are read like any table.

// endStateUIDColumns is every table.column that holds a station uid at the
// end state (the round-2 end-state table with its decided names).
var endStateUIDColumns = []string{
	"bin_loaders.station_uid",
	"bin_uop_delta_daily.station_uid",
	"bin_uop_exception.station_uid",
	"bin_uop_ledger.station_uid",
	"cell_config.station_uid",
	"cell_part_events.station_uid",
	"demand_origins.station_uid",
	"demand_registry.station_uid",
	"edge_cells.station_uid",
	"edge_lineside_reports.station_uid",
	"edge_registry.station_uid",
	"inventory_delta_dedup.station_uid",
	"kanbans.station_uid",
	"lineside_buckets.station_uid",
	"mission_telemetry.station_uid",
	"node_stations.station_uid",
	"orders.projects_to_station_uid",
	"orders.station_uid",
	"plant_claims_reports.station_uid",
	"process_styles.station_uid",
}

// stationNamesAllowed are the column names that say station and hold no uid.
var stationNamesAllowed = []string{"dst_station", "last_rds_station", "rds_station", "station_unaware", "stations_json"}

// Ends of a known violation.
const (
	nameDropped      = "(dropped)"
	nameTableDropped = "(table dropped)"
	nameAdded        = "(added)"
)

// knownNamingViolations is every column that breaks the rule in today's
// snapshot, as table.column, with the column it becomes ("table.column",
// nameDropped or nameTableDropped), and every end-state uid column the
// snapshot lacks that no rename makes (nameAdded).
var knownNamingViolations = map[string]string{
	"bin_loaders.edge_station_uid":          "bin_loaders.station_uid", // one station role: the bare name
	"bin_uop_delta_daily.actor":             "bin_uop_delta_daily.station_uid",
	"bin_uop_exception.station_uid":         nameAdded, // the uid out of actor, and hold_dropped's detail station
	"bin_uop_ledger.station":                "bin_uop_ledger.station_uid",
	"cell_config.station":                   "cell_config.station_uid",
	"cell_part_events.cell_id":              "cell_part_events.station_uid",
	"demand_origins.station_id":             "demand_origins.station_uid",
	"demand_registry.station_id":            "demand_registry.station_uid",
	"downtime_event_dedup.station":          nameTableDropped,
	"downtime_events.station":               nameTableDropped,
	"edge_cells.station":                    "edge_cells.station_uid",
	"edge_lineside_reports.station":         "edge_lineside_reports.station_uid",
	"edge_registry.station_id":              nameDropped, // folded into station_uid first
	"inbox.station_id":                      nameDropped,
	"inventory_delta_dedup.station":         "inventory_delta_dedup.station_uid",
	"kanbans.station_id":                    "kanbans.station_uid",
	"lineside_buckets.station":              "lineside_buckets.station_uid",
	"mission_events.robot_station":          "mission_events.rds_station",
	"mission_telemetry.station_id":          "mission_telemetry.station_uid",
	"node_stations.station_id":              "node_stations.station_uid",
	"order_intake_refusals.station_id":      nameDropped, // the reader search: GetIntakeRefusal scans it into a field nothing reads
	"orders.projects_to":                    "orders.projects_to_station_uid",
	"orders.station_id":                     "orders.station_uid",
	"outbox.station_id":                     "outbox.dst_station",
	"plant_claims_reports.station_id":       "plant_claims_reports.station_uid",
	"process_styles.station_id":             "process_styles.station_uid",
	"robot_confidence_low.last_station":     "robot_confidence_low.last_rds_station",
	"robot_confidence_low.station":          "robot_confidence_low.rds_station",
	"robot_confidence_samples.last_station": "robot_confidence_samples.last_rds_station",
	"robot_confidence_samples.station":      "robot_confidence_samples.rds_station",
	"robot_lidar_hours.station":             "robot_lidar_hours.rds_station",
	"supply_refusals.station_id":            nameDropped,
}

// knownStationIndexNames is every index or constraint that covers a column
// in knownNamingViolations, or whose name breaks the column rule, as
// "table.name" with the columns it covers.
var knownStationIndexNames = map[string]string{
	"bin_uop_delta_daily.bin_uop_delta_daily_pkey":                               "day, bin_id, epoch_seq, payload_code, reason, actor",
	"cell_config.cell_config_station_idx":                                        "station",
	"cell_part_events.idx_cell_part_events_cell_time":                            "cell_id, recorded_at",
	"cell_part_events.uq_cell_part_events_tick":                                  "cell_id, edge_snapshot_id, recorded_at",
	"demand_registry.demand_registry_station_id_core_node_name_payload_code_key": "station_id, core_node_name, payload_code",
	"downtime_event_dedup.downtime_event_dedup_pkey":                             "station, edge_event_id",
	"downtime_events.idx_downtime_events_station_time":                           "station, started_at",
	"edge_cells.edge_cells_pkey":                                                 "station, cell_label",
	"edge_cells.idx_edge_cells_station":                                          "station",
	"edge_lineside_reports.edge_lineside_reports_pkey":                           "station, core_node_name, payload_code",
	"edge_registry.edge_registry_station_id_key":                                 "station_id",
	"inventory_delta_dedup.inventory_delta_dedup_pkey":                           "station, scope_kind, scope_key, epoch",
	"mission_telemetry.idx_mission_telemetry_station":                            "station_id",
	"node_stations.idx_node_stations_station":                                    "station_id",
	"node_stations.node_stations_pkey":                                           "node_id, station_id",
}

func TestStationNaming_TheSnapshotFollowsTheRule(t *testing.T) {
	t.Parallel()
	snap := readSnapshotShape(t)
	got := stationNamingViolations(snap)
	want := slices.Sorted(maps.Keys(knownNamingViolations))
	for _, v := range got {
		if _, ok := knownNamingViolations[v.column]; !ok {
			t.Errorf("%s %s. Name it by the rule (a station uid in station_uid or <role>_station_uid; an RDS map "+
				"name in rds_station; otherwise no station in the name), or, until a migration renames it, "+
				"list it in knownNamingViolations with the name it is due to get.", v.column, v.why)
		}
	}
	gotCols := map[string]bool{}
	for _, v := range got {
		gotCols[v.column] = true
	}
	for _, col := range want {
		if !gotCols[col] {
			t.Errorf("knownNamingViolations names %s (-> %s), and the snapshot no longer breaks the rule there: "+
				"strike it from the list.", col, knownNamingViolations[col])
		}
	}
	// What each violation becomes must itself follow the rule.
	declared := map[string]bool{}
	for _, c := range endStateUIDColumns {
		declared[c] = true
	}
	for _, col := range want {
		to := knownNamingViolations[col]
		_, name, _ := strings.Cut(to, ".")
		table, _, _ := strings.Cut(col, ".")
		switch {
		case to == nameAdded:
			if snap.columns[col] || !declared[col] {
				t.Errorf("%s is listed as added: it must be an end-state uid column the snapshot lacks", col)
			}
		case strings.HasPrefix(to, "("):
		case !strings.HasPrefix(to, table+"."):
			t.Errorf("%s -> %s: a rename stays on its table", col, to)
		case declared[to], slices.Contains(stationNamesAllowed, name):
		case strings.Contains(name, "station"):
			t.Errorf("%s -> %s: the new name breaks the rule too", col, to)
		}
	}
	t.Logf("%d tables, %d columns read; %d partition children skipped %v", snap.tables, len(snap.columns),
		len(snap.partitions), snap.partitions)
}

func TestStationNaming_IndexNamesFollowTheirColumns(t *testing.T) {
	t.Parallel()
	snap := readSnapshotShape(t)
	got := map[string]string{}
	for _, ix := range snap.indexes {
		covered := false
		for _, c := range ix.columns {
			if _, ok := knownNamingViolations[ix.table+"."+c]; ok {
				covered = true
			}
		}
		if covered || indexNameBreaksRule(ix.name, ix.table) {
			got[ix.table+"."+ix.name] = strings.Join(ix.columns, ", ")
		}
	}
	for _, k := range slices.Sorted(maps.Keys(got)) {
		if want, ok := knownStationIndexNames[k]; !ok || want != got[k] {
			t.Errorf("%s (%s) covers a column due a new name, or its name carries one, and knownStationIndexNames "+
				"names it %q: an index or constraint name follows its columns in the migration that renames them; "+
				"list it here until then.", k, got[k], want)
		}
	}
	for _, k := range slices.Sorted(maps.Keys(knownStationIndexNames)) {
		if _, ok := got[k]; !ok {
			t.Errorf("knownStationIndexNames names %s, which no longer covers a listed column or carries one in "+
				"its name: strike it.", k)
		}
	}
}

// indexNameBreaksRule says an index or constraint name, past its table's
// name, says station other than as station_uid or an allowed column name, or
// names cell_id or projects_to, whose columns are renamed: the column rule,
// applied to the names that follow columns.
func indexNameBreaksRule(name, table string) bool {
	rest := strings.ReplaceAll(name, table, "")
	// Longest first, so rds_station does not eat last_rds_station's tail.
	for _, ok := range []string{"projects_to_station_uid", "last_rds_station", "station_unaware", "stations_json",
		"dst_station", "rds_station", "station_uid"} {
		rest = strings.ReplaceAll(rest, ok, "")
	}
	return strings.Contains(rest, "station") || strings.Contains(rest, "cell_id") || strings.Contains(rest, "projects_to")
}

type namingViolation struct{ column, why string }

// stationNamingViolations is every way the snapshot breaks the rule: a
// column named against it, a column ending station_uid that holds no uid at
// the end state, and an end-state uid column the snapshot lacks.
func stationNamingViolations(snap snapshotShape) []namingViolation {
	declared := map[string]bool{}
	for _, c := range endStateUIDColumns {
		declared[c] = true
	}
	var out []namingViolation
	for _, col := range slices.Sorted(maps.Keys(snap.columns)) {
		_, name, _ := strings.Cut(col, ".")
		uidName := name == "station_uid" || strings.HasSuffix(name, "_station_uid")
		switch {
		case name == "station_id" || name == "station":
			out = append(out, namingViolation{col, "is named " + name})
		case uidName && !declared[col]:
			out = append(out, namingViolation{col, "is named for a station uid and endStateUIDColumns does not name it"})
		case !uidName && strings.Contains(name, "station") && !slices.Contains(stationNamesAllowed, name):
			out = append(out, namingViolation{col, "says station and is not one of " + strings.Join(stationNamesAllowed, ", ")})
		}
	}
	// An end-state uid column the snapshot lacks is held today by the column
	// the list says is renamed to it, which is then the violation (holding a
	// uid under another name) whatever its name says; with none, the missing
	// column itself is. Which column holds the uid is the end-state table's
	// word, not a name's, so this is the one place the list is trusted.
	held := map[string]string{}
	for col, to := range knownNamingViolations {
		if snap.columns[col] && declared[to] {
			held[to] = col
		}
	}
	seen := map[string]bool{}
	for _, v := range out {
		seen[v.column] = true
	}
	for _, c := range endStateUIDColumns {
		switch {
		case snap.columns[c]:
		case held[c] != "" && !seen[held[c]]:
			out = append(out, namingViolation{held[c], "holds the station uid that the end state names " + c})
		case held[c] == "":
			out = append(out, namingViolation{c, "holds a station uid at the end state and is not in the snapshot"})
		}
	}
	return out
}

// snapshotShape is the snapshot's columns ("table.column"), indexes and
// constraints, and the partition children it skipped.
type snapshotShape struct {
	tables     int
	columns    map[string]bool
	indexes    []snapshotIndex
	partitions []string
}

type snapshotIndex struct {
	table, name string
	columns     []string
}

var (
	createTableRe = regexp.MustCompile(`(?s)^CREATE TABLE public\.(\w+) \(\n(.*?)\n\)`)
	columnNameRe  = regexp.MustCompile(`^"?([a-z_][a-z0-9_]*)"?$`)
	partitionOfRe = regexp.MustCompile(`(?s)^CREATE TABLE public\.(\w+) PARTITION OF `)
	attachRe      = regexp.MustCompile(`ATTACH PARTITION public\.(\w+)`)
	createIndexRe = regexp.MustCompile(`^CREATE (?:UNIQUE )?INDEX (\w+) ON (?:ONLY )?public\.(\w+) USING \w+ \(([^)]*)\)`)
	addConRe      = regexp.MustCompile(`(?s)^ALTER TABLE (?:ONLY )?public\.(\w+)\s+ADD CONSTRAINT (\w+) (?:(?:PRIMARY KEY|UNIQUE|FOREIGN KEY) \(([^)]*)\))?`)
	inlineConRe   = regexp.MustCompile(`^CONSTRAINT (\w+) `)
)

// readSnapshotShape parses schema.snapshot.sql statement by statement.
func readSnapshotShape(t *testing.T) snapshotShape {
	t.Helper()
	const path = "schema.snapshot.sql"
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v (this census is the only reader of the naming rule; repoint it if the snapshot moved)", path, err)
	}
	shape := snapshotShape{columns: map[string]bool{}}
	attached := map[string]bool{}
	for _, m := range attachRe.FindAllStringSubmatch(string(b), -1) {
		attached[m[1]] = true
	}
	for _, stmt := range snapshotStatements(string(b)) {
		if m := partitionOfRe.FindStringSubmatch(stmt); m != nil {
			shape.partitions = append(shape.partitions, m[1])
			continue
		}
		if m := createTableRe.FindStringSubmatch(stmt); m != nil {
			if attached[m[1]] {
				shape.partitions = append(shape.partitions, m[1])
				continue
			}
			shape.tables++
			for _, line := range strings.Split(m[2], "\n") {
				line = strings.TrimSuffix(strings.TrimSpace(line), ",")
				if line == "" {
					continue
				}
				if c := inlineConRe.FindStringSubmatch(line); c != nil {
					shape.indexes = append(shape.indexes, snapshotIndex{table: m[1], name: c[1]})
					continue
				}
				c := columnNameRe.FindStringSubmatch(strings.Fields(line)[0])
				if c == nil {
					t.Fatalf("%s: table %s has a line the census cannot read as a column: %q", path, m[1], line)
				}
				shape.columns[m[1]+"."+c[1]] = true
			}
			continue
		}
		if m := createIndexRe.FindStringSubmatch(stmt); m != nil {
			shape.indexes = append(shape.indexes, snapshotIndex{table: m[2], name: m[1], columns: indexColumns(m[3])})
			continue
		}
		if m := addConRe.FindStringSubmatch(stmt); m != nil {
			shape.indexes = append(shape.indexes, snapshotIndex{table: m[1], name: m[2], columns: indexColumns(m[3])})
		}
	}
	if shape.tables < 50 || len(shape.columns) < 500 {
		t.Fatalf("parsed %d tables and %d columns from %s; the parser is broken, not the schema", shape.tables, len(shape.columns), path)
	}
	return shape
}

// indexColumns is an index's or key's column list, each column's first word.
func indexColumns(list string) []string {
	var out []string
	for _, c := range strings.Split(list, ",") {
		if f := strings.Fields(c); len(f) > 0 {
			out = append(out, strings.Trim(f[0], `"`))
		}
	}
	return out
}

// snapshotStatements splits the dump on lines that end a statement, the way
// pg_dump writes them; a function body ($$ … $$) is one statement.
func snapshotStatements(dump string) []string {
	var out, cur []string
	inBody := false
	for _, line := range strings.Split(dump, "\n") {
		if strings.Count(line, "$$")%2 == 1 {
			inBody = !inBody
		}
		cur = append(cur, line)
		if !inBody && strings.HasSuffix(strings.TrimSpace(line), ";") {
			out = append(out, strings.TrimSpace(strings.Join(cur, "\n")))
			cur = nil
		}
	}
	return out
}

func (v namingViolation) String() string { return fmt.Sprintf("%s %s", v.column, v.why) }
