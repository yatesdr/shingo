// Command seeddev seeds the local-dev demo plant (core Postgres + edge SQLite)
// from a declarative plant spec (brief Phase 4, D2). LOCAL DEV / SIMULATION ONLY.
//
// Flow: load core config + plant spec, validate the spec, then seed core
// (Postgres) first — edge process_nodes reference core node names by convention.
// Edge (SQLite) seeding is raw SQL (seeddev does NOT import the shingo-edge
// module — decided 2026-06-07) and runs against the already-migrated edge DB.
//
// Idempotent where an accessor supports existence-check; safe to re-run.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"strings"

	"shingocore/config"
	"shingocore/plantspec"
	"shingocore/store"
)

func main() {
	log.SetPrefix("[seeddev] ")
	log.SetFlags(0)

	configPath := flag.String("config", "", "core config YAML (required)")
	edgeDBPath := flag.String("edge-db", "", "edge SQLite path (required unless --core-only)")
	plantPath := flag.String("plant", "plants/demo.yaml", "plant spec file")
	coreOnly := flag.Bool("core-only", false, "seed core (Postgres) only")
	edgeOnly := flag.Bool("edge-only", false, "seed edge (SQLite) only")
	wipe := flag.Bool("wipe", false, "wipe operational data before re-seeding (F3-gated)")
	yes := flag.Bool("yes", false, "skip the interactive --wipe confirmation (Makefile path)")
	flag.Parse()

	if *configPath == "" {
		log.Fatal("--config is required")
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("load config: %v", err)
	}

	// Load + validate the plant spec BEFORE touching any database, so a bad spec
	// fails loudly with every problem at once rather than a partial seed.
	plant, err := plantspec.Load(*plantPath)
	if err != nil {
		log.Fatalf("%v", err)
	}
	if err := plant.Validate(); err != nil {
		log.Fatalf("%v", err)
	}
	log.Printf("plant %q validated: %d zones, %d stations, %d payloads, %d bins, %d claims",
		*plantPath, len(plant.Zones), len(plant.Stations), len(plant.Payloads), len(plant.Bins), len(plant.Claims))

	// --wipe safety (F3): destructive, so require sim mode + the env gate, then a
	// typed confirmation (mirrors core's --reset-db). --yes is the Makefile path.
	if *wipe {
		if !cfg.Sim.Enabled || os.Getenv("SHINGO_ALLOW_SIM") != "1" {
			log.Fatal("--wipe refused: requires sim.enabled=true in the config AND SHINGO_ALLOW_SIM=1")
		}
		if !*yes && !confirmWipe() {
			log.Fatal("aborted")
		}
		log.Printf("--wipe: TODO(T4.5) — operational-table truncation not yet implemented; seeding is idempotent so a fresh dev-reset is the current path")
	}

	// node-name → core bin id, filled by seedCore, used by seedEdge to bind
	// each produce/consume node's runtime active_bin_id to the bin at it.
	binIDByNode := make(map[string]int64)

	// Core (Postgres) first: edge references core node names.
	var coreDB *store.DB
	if !*edgeOnly {
		coreDB, err = store.Open(&cfg.Database)
		if err != nil {
			log.Fatalf("open core db: %v", err)
		}
		defer coreDB.Close()
		if err := seedCore(coreDB, plant, binIDByNode); err != nil {
			log.Fatalf("seed core: %v", err)
		}
		log.Printf("core seeded OK")
	}

	// Edge (SQLite) via raw SQL against the already-migrated edge DB.
	if !*coreOnly {
		if *edgeDBPath == "" {
			log.Fatal("--edge-db is required to seed edge (or pass --core-only)")
		}
		if err := seedEdge(*edgeDBPath, plant, binIDByNode); err != nil {
			log.Fatalf("seed edge: %v", err)
		}
		log.Printf("edge seeded OK")
	}

	// Cross-DB validation when both halves were seeded (catches a seeding bug
	// where an edge node name doesn't resolve to a core node).
	if coreDB != nil && !*coreOnly {
		if err := crossValidate(coreDB, *edgeDBPath); err != nil {
			log.Fatalf("cross-DB validation: %v", err)
		}
		log.Printf("cross-DB validation OK")
	}
	log.Printf("seed complete")
}

func confirmWipe() bool {
	fmt.Fprint(os.Stderr, "[seeddev] --wipe will delete operational data. Type 'yes' to confirm: ")
	var ans string
	_, _ = fmt.Scanln(&ans)
	return strings.TrimSpace(ans) == "yes"
}
