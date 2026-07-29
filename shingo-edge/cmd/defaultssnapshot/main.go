// Command defaultssnapshot writes the committed rendering of shingo-edge's
// shipped defaults.
//
// The defaults are the schema of how this system behaves when nobody has said
// otherwise, and until now there was no file you could open to see them.
// Committing the rendering makes retuning one a DIFF a reviewer can question
// rather than a one-character edit nobody sees.
//
// Run via `make defaults-snapshot`. No Docker, no database — it renders a
// zero-value Config.
package main

import (
	"fmt"
	"os"
	"path/filepath"

	"shingoedge/config"
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
