package main

import (
	"flag"
	"fmt"
	"io"
	"strconv"

	"shingocore/config"
	"shingocore/store"
)

// migrateUsage is printed on stderr for a migrate command line this does not
// read.
const migrateUsage = `Usage: shingocore migrate [-config PATH] down N

  down N   run the down step of version N, the newest recorded; exit 0 on success

  -config PATH   config file path (default: shingocore.yaml)
`

// runMigrate is `shingocore migrate ...`: the rollback's first step. It returns
// the process's exit code; exit 2 is a command line it does not read.
func runMigrate(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "shingocore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	rest := fs.Args()
	var down int
	switch {
	case len(rest) == 2 && rest[0] == "down":
		n, err := strconv.Atoi(rest[1])
		if err != nil || n <= 0 {
			fmt.Fprint(stderr, migrateUsage)
			return 2
		}
		down = n
	default:
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 2
	}
	if err := store.MigrateDown(&cfg.Database, down); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "migrate down %d: done\n", down)
	return 0
}
