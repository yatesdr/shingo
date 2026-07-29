// Command schemasnapshot writes the committed schema snapshot.
//
// It boots an empty Postgres, runs the FULL production migrate path against
// it, dumps the result with pg_dump --schema-only, and writes it to
// store/schema/schema.snapshot.sql.
//
// The point of the committed file is that every future shape change appears as
// a readable diff in review, instead of requiring a reviewer to simulate fifty
// migrations in their head. TestSchemaSnapshotIsCurrent keeps it honest.
//
// Run via `make schema-snapshot` from the shingo-core module root. Requires
// Docker; pg_dump runs inside the container, so no Postgres client tools are
// needed on the host.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"shingocore/internal/schemadump"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "schemasnapshot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	fmt.Fprintf(os.Stderr, "schemasnapshot: starting postgres...\n")
	inst, err := schemadump.Start(ctx)
	if err != nil {
		return err
	}
	defer func() {
		// Best-effort: a leaked container costs a reap, a lost snapshot costs
		// a rerun. Neither should mask a real error from the work above.
		_ = inst.Close(context.Background())
	}()

	fmt.Fprintf(os.Stderr, "schemasnapshot: running migrations...\n")
	dbName, err := inst.BuildFresh(ctx)
	if err != nil {
		return err
	}

	fmt.Fprintf(os.Stderr, "schemasnapshot: dumping schema...\n")
	dump, err := inst.Dump(ctx, dbName)
	if err != nil {
		return err
	}

	out := schemadump.SnapshotPath
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	// LF endings, explicitly — the repo is eol=lf and a CRLF snapshot would
	// diff against itself on the next machine.
	if err := os.WriteFile(out, []byte(dump), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "schemasnapshot: wrote %s (%d bytes)\n", out, len(dump))
	return nil
}
