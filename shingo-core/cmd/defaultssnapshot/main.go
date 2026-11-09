// Command defaultssnapshot writes the committed rendering of shingo-core's
// shipped defaults (config/defaults.snapshot.txt).
//
// Committing the rendering makes retuning a default a DIFF a reviewer can
// question rather than a one-character edit nobody sees, and it is what the
// configuration key table in docs/configuration.md is generated from.
//
// Run via `make defaults-snapshot`. No Docker, no database — it renders
// config.Defaults().
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"shingocore/config"
)

func main() {
	out := config.DefaultsSnapshotPath
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "defaultssnapshot: create %s: %v\n", filepath.Dir(out), err)
		os.Exit(1)
	}
	// LF endings, explicitly — the repo is eol=lf.
	if err := os.WriteFile(out, []byte(config.RenderDefaults()), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "defaultssnapshot: write %s: %v\n", out, err)
		os.Exit(1)
	}
	fmt.Fprintf(os.Stderr, "defaultssnapshot: wrote %s\n", out)
}
