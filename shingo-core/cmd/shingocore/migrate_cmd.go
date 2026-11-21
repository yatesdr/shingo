package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strconv"

	"shingocore/config"
	"shingocore/store"
)

// migrateUsage is printed on stderr for a migrate command line this does not
// read.
const migrateUsage = `Usage: shingocore migrate [-config PATH] check
       shingocore migrate [-config PATH] down N

  check    say what this build's boot would do to the database, writing nothing:
           exit 0 and "database v<cur> -> v<target>" on stdout when Core would
           start on it; exit 1 and the refusal on stderr when it would refuse
           it; exit 2 and the error when the database cannot be read
  down N   run the down step of version N, the newest recorded; exit 0 on success

  -config PATH   config file path (default: shingocore.yaml); it must exist
`

// runMigrate is `shingocore migrate ...`: the installer's checks around a
// deploy (install-core.sh) and the rollback's first step. It returns the
// process's exit code. Exit 2 is a command line it does not read, a config
// file that is not there, a database it cannot read, or a panic: exit 1 is
// only ever a refusal, which install-core.sh reads as "this build will not
// start on the database" (main's own recover would make a panic exit 1).
func runMigrate(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "migrate: panic: %v\n%s", r, debug.Stack())
			code = 2
		}
	}()
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "shingocore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "migrate: %v\n", err)
		}
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	rest := fs.Args()
	var down int
	switch {
	case len(rest) == 1 && rest[0] == "check":
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
	// config.Load reads a missing file as the defaults, which name a database
	// on localhost: from the wrong directory, `migrate down` would step
	// whatever answers there. The file must be there; any other error reading
	// it is config.Load's to report.
	if _, err := os.Stat(*configPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "config %s is not there: pass -config with the file Core's service starts with "+
			"(systemctl cat shingo-core shows its --config)\n", *configPath)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 2
	}
	if down > 0 {
		if err := store.MigrateDown(&cfg.Database, down); err != nil {
			fmt.Fprintln(stderr, err)
			return 1
		}
		fmt.Fprintf(stdout, "migrate down %d: done\n", down)
		return 0
	}
	cur, target, err := store.CheckDatabase(&cfg.Database)
	var refusal *store.Refusal
	switch {
	case errors.As(err, &refusal):
		fmt.Fprintln(stderr, refusal.Error())
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "check the database: %v\n", err)
		return 2
	}
	fmt.Fprintf(stdout, "database v%d -> v%d\n", cur, target)
	return 0
}
