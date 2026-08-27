// Command schemasnapshot writes shingo-edge's committed schema snapshot.
//
// It creates an empty SQLite database, runs the FULL production migrate path
// against it, reads sqlite_master, and writes the result to
// store/schema/schema.snapshot.sql.
//
// No Docker and no external tools — SQLite is a file. Run via
// `make schema-snapshot` from the shingo-edge module root.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"shingoedge/internal/schemadump"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "schemasnapshot: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	dir, err := os.MkdirTemp("", "shingoedge-schema")
	if err != nil {
		return fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	fmt.Fprintf(os.Stderr, "schemasnapshot: running migrations...\n")
	path, err := schemadump.BuildFresh(dir)
	if err != nil {
		return err
	}

	dump, err := schemadump.Dump(path)
	if err != nil {
		// Unchecked, a failed dump still fell through and wrote whatever `dump`
		// held — committing an empty or truncated snapshot that the drift test
		// would then treat as the schema of record.
		return fmt.Errorf("dump schema: %w", err)
	}

	out := schemadump.SnapshotPath
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return fmt.Errorf("create %s: %w", filepath.Dir(out), err)
	}
	// LF endings, explicitly — the repo is eol=lf.
	if err := os.WriteFile(out, []byte(dump), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", out, err)
	}
	fmt.Fprintf(os.Stderr, "schemasnapshot: wrote %s (%d bytes)\n", out, len(dump))
	return nil
}
