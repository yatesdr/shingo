// Command schemabaseline writes the committed baseline,
// store/schema/baseline.sql: the Edge's whole schema and its schema_migrations
// rows, which store.migrate applies to a file with no tables.
//
// It builds the boot path of store.RetiredChainRelease from git and boots an
// empty file with it (the retired chain, by the code the plants ran), runs this
// tree's migrations above it, and writes sqlite_master out
// (schemadump.GenerateBaseline). The file is never edited by hand;
// TestBaselineIsCurrent regenerates it and compares.
//
// Run via `make schema-baseline` from the shingo-edge module root, then
// `make schema-snapshot` (the snapshot is a fresh boot of a build that embeds
// the new baseline). Requires the release's tag in this clone; no Docker.
package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"shingoedge/internal/schemadump"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "schemabaseline: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	root, err := os.Getwd()
	if err != nil {
		return err
	}
	work, err := os.MkdirTemp("", "schemabaseline-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work)

	fmt.Fprintf(os.Stderr, "schemabaseline: building the release and booting it...\n")
	baseline, err := schemadump.GenerateBaseline(ctx, root, work)
	if err != nil {
		return err
	}
	out := filepath.FromSlash(schemadump.BaselinePath)
	// LF endings, explicitly: the repo is eol=lf.
	if err := os.WriteFile(out, []byte(baseline), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "schemabaseline: wrote %s (%d bytes)\n", out, len(baseline))
	return nil
}
