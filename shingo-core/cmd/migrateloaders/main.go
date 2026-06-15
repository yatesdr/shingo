// Command migrateloaders performs the one-time loader-refactor cutover migration:
// it derives Core's bin_loaders aggregate from the live Edge style_node_claims
// (joined with the home_location_loaders / transitional_loaders /
// loader_payload_thresholds flag tables), enforces the SLN_002 tripwire, writes
// the aggregate to Core Postgres, and seeds demand_registry from it.
//
// Like seeddev, this reads the Edge SQLite via RAW SQL (it does not import the
// shingo-edge module) and writes Core via the store. Run it once per plant to
// migrate its loaders into the Core aggregate. --dry-run derives + tripwire-checks only.
//
// Idempotent: WriteDerivedLoaders skips a (core_node, role) that already exists.
package main

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"log"

	_ "modernc.org/sqlite" // pure-Go SQLite driver, same as shingo-edge

	"shingocore/config"
	"shingocore/store"
	"shingocore/store/loaders"
)

func main() {
	log.SetPrefix("[migrate-loaders] ")
	log.SetFlags(0)

	configPath := flag.String("config", "", "core config YAML (required, unless --dry-run)")
	edgeDBPath := flag.String("edge-db", "", "edge SQLite path (required)")
	station := flag.String("station", "", "edge station id for demand_registry (required, unless --dry-run)")
	dryRun := flag.Bool("dry-run", false, "derive + tripwire-check only; write nothing")
	flag.Parse()

	if *edgeDBPath == "" {
		log.Fatal("--edge-db is required")
	}
	if !*dryRun && (*configPath == "" || *station == "") {
		log.Fatal("--config and --station are required (or use --dry-run)")
	}

	claims, err := readLegacyClaims(*edgeDBPath)
	if err != nil {
		log.Fatalf("read edge claims: %v", err)
	}
	log.Printf("read %d manual_swap loader claim(s) from edge", len(claims))

	// GroupIntoLoaders enforces CheckHomeTripwire — a home node with >1 payload
	// fails the whole migration loudly here rather than importing a self-spam config.
	derived, err := loaders.GroupIntoLoaders(claims)
	if err != nil {
		log.Fatalf("derive: %v", err)
	}
	log.Printf("derived %d loader(s)", len(derived))
	for _, d := range derived {
		log.Printf("  %s/%s %s — payloads=%d homes=%d", d.Loader.Name, d.Loader.Role, d.Loader.Layout, len(d.Payloads), len(d.Homes))
	}
	if *dryRun {
		log.Print("dry-run: nothing written")
		return
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}
	coreDB, err := store.Open(&cfg.Database)
	if err != nil {
		log.Fatalf("open core db: %v", err)
	}
	defer coreDB.Close()

	created, skipped, err := coreDB.WriteDerivedLoaders(derived)
	if err != nil {
		log.Fatalf("write loaders: %v", err)
	}
	log.Printf("wrote %d loader(s) (%d home(s) skipped — position node absent from core topology)", created, skipped)

	entries, err := coreDB.BuildDemandRegistryFromAggregate(*station)
	if err != nil {
		log.Fatalf("build demand registry: %v", err)
	}
	if _, err := coreDB.SyncDemandRegistry(*station, entries); err != nil {
		log.Fatalf("sync demand registry: %v", err)
	}
	log.Printf("synced %d demand_registry entr(ies) for station %s", len(entries), *station)
	log.Print("migration complete")
}

// readLegacyClaims reads the live Edge manual_swap loader bindings + flag-table
// state into []MigrationClaim via raw SQL (column lists track shingo-edge schema).
func readLegacyClaims(edgeDBPath string) ([]loaders.MigrationClaim, error) {
	db, err := sql.Open("sqlite", edgeDBPath+"?_busy_timeout=20000&_foreign_keys=on")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("ping edge sqlite %s (is it migrated?): %w", edgeDBPath, err)
	}

	// operator station per node (first wins) — the home-location grouping key.
	opStation := map[string]string{}
	if rows, qerr := db.Query(`SELECT pn.core_node_name, os.code FROM process_nodes pn JOIN operator_stations os ON os.id = pn.operator_station_id`); qerr == nil {
		for rows.Next() {
			var n, s string
			if rows.Scan(&n, &s) == nil && opStation[n] == "" {
				opStation[n] = s
			}
		}
		rows.Close()
	}

	home := flagSet(db, "home_location_loaders")
	transitional := flagSet(db, "transitional_loaders")

	thr := map[string]int{}
	if rows, qerr := db.Query(`SELECT core_node_name, payload_code, replenish_uop_threshold FROM loader_payload_thresholds`); qerr == nil {
		for rows.Next() {
			var n, p string
			var t int
			if rows.Scan(&n, &p, &t) == nil {
				thr[n+"\x00"+p] = t
			}
		}
		rows.Close()
	}

	rows, err := db.Query(`SELECT DISTINCT core_node_name, role, payload_code, allowed_payload_codes,
		reorder_point, inbound_source, outbound_destination, auto_push
		FROM style_node_claims WHERE swap_mode = 'manual_swap'`)
	if err != nil {
		return nil, fmt.Errorf("query claims: %w", err)
	}
	defer rows.Close()

	var out []loaders.MigrationClaim
	for rows.Next() {
		var node, role, payload, allowedJSON, inbound, outbound string
		var reorder, autoPush int
		if err := rows.Scan(&node, &role, &payload, &allowedJSON, &reorder, &inbound, &outbound, &autoPush); err != nil {
			return nil, err
		}
		payloads := parseAllowed(allowedJSON, payload)
		thrMap := map[string]int{}
		for _, p := range payloads {
			if v, ok := thr[node+"\x00"+p]; ok {
				thrMap[p] = v
			}
		}
		out = append(out, loaders.MigrationClaim{
			CoreNode: node, Role: role, Payload: payload, Payloads: payloads,
			ReorderPoint: reorder, InboundSource: inbound, OutboundDest: outbound,
			AutoPush: autoPush == 1, OperatorStation: opStation[node],
			HomeLocation: home[node], Transitional: transitional[node], Thresholds: thrMap,
		})
	}
	return out, rows.Err()
}

func flagSet(db *sql.DB, table string) map[string]bool {
	out := map[string]bool{}
	if rows, err := db.Query(`SELECT core_node_name FROM ` + table); err == nil { //nolint:gosec // fixed table names
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				out[n] = true
			}
		}
		rows.Close()
	}
	return out
}

// parseAllowed returns the claim's payload set: the JSON allowed_payload_codes
// list, or just the primary payload when the list is empty.
func parseAllowed(allowedJSON, primary string) []string {
	var allowed []string
	if allowedJSON != "" {
		_ = json.Unmarshal([]byte(allowedJSON), &allowed)
	}
	if len(allowed) == 0 {
		if primary != "" {
			return []string{primary}
		}
		return nil
	}
	return allowed
}
