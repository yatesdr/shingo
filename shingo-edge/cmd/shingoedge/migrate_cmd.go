package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"shingoedge/backup"
	"shingoedge/config"
	"shingoedge/store"
)

// migrateUsage is printed on stderr for a migrate command line this does not
// read.
const migrateUsage = `Usage: shingoedge migrate [-config PATH] check

  check    say what this build's start would do to the database, writing
           nothing: exit 0 and "database v<cur> -> v<target>" on stdout when
           the Edge would start on it; exit 1 and the refusal on stderr when it
           would refuse it, or a restore staged for that start; exit 2 and the
           error when either cannot be read

  -config PATH   config file path (default: shingoedge.yaml); it must exist
                 A relative database_path is read from the working directory,
                 as the Edge reads it.
`

// runMigrate is shingoedge migrate check, the installer's question before
// it stops the service (install-edge.sh). It returns the process's exit code.
// Exit 2 is a command line it does not read, a config file that is not there,
// a file it cannot read, or a panic: exit 1 is only ever a refusal, which
// install-edge.sh reads as "this build will not start on the database".
func runMigrate(args []string, stdout, stderr io.Writer) (code int) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "migrate: panic: %v\n%s", r, debug.Stack())
			code = 2
		}
	}()
	fs := flag.NewFlagSet("migrate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	configPath := fs.String("config", "shingoedge.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "migrate: %v\n", err)
		}
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	if rest := fs.Args(); len(rest) != 1 || rest[0] != "check" {
		fmt.Fprint(stderr, migrateUsage)
		return 2
	}
	// config.Load reads a missing file as the defaults, which name a database
	// in the working directory: the file must be there.
	if _, err := os.Stat(*configPath); errors.Is(err, os.ErrNotExist) {
		fmt.Fprintf(stderr, "config %s is not there: pass -config with the file the Edge's service starts with "+
			"(systemctl cat shingo-edge shows its --config)\n", *configPath)
		return 2
	}
	cfg, err := config.Load(*configPath)
	if err != nil {
		fmt.Fprintf(stderr, "load config: %v\n", err)
		return 2
	}
	answer := func(what string, err error) int {
		var refusal *store.Refusal
		if errors.As(err, &refusal) {
			fmt.Fprintf(stderr, "%s%v\n", what, err)
			return 1
		}
		fmt.Fprintf(stderr, "%scannot be read: %v\n", what, err)
		return 2
	}
	// A staged restore replaces the database at the next start, so it is asked
	// too.
	if staged, err := backup.CheckPendingRestore(*configPath); err != nil {
		return answer("the staged restore: ", err)
	} else if staged {
		fmt.Fprintln(stdout, "a staged restore passes: the next start applies it")
	}
	cur, target, err := store.CheckDatabase(cfg.DatabasePath)
	if err != nil {
		return answer(cfg.DatabasePath+": ", err)
	}
	fmt.Fprintf(stdout, "database v%d -> v%d\n", cur, target)
	return 0
}
