// Command release cuts a shingo release, and holds CHANGELOG.md to the shape
// a release needs.
//
// WHY. Core and the Edge ship as one version, v0.MINOR.PATCH, and what a
// person deploying one must do (the migrations it applies, which binary goes
// first) used to sit in a dated merge log nobody could map to a build. Here
// the version and that sentence are one artifact: a release renames the
// changelog's `## Unreleased` to the version's heading and tags that commit,
// and the GitHub release's notes are that section (-notes).
//
// THREE MODES.
//
//	-version vX.Y.Z [-title T]  cut the release (Release.Run)
//	-check-changelog            the guard scripts/check-changelog.sh runs (CheckChangelog)
//	-notes vX.Y.Z               print that version's CHANGELOG.md section body (Notes)
//
// THE RELEASE REFUSES, cheapest first, and writes nothing when it does: a
// malformed VERSION; HEAD not on main; any change in the worktree, untracked
// included; HEAD not origin/main after a fetch; a tag that exists; a red
// changelog check, or an empty `## Unreleased`; a VERSION below the least the
// changes since the last tag allow (Required: MINOR for a migration, a
// protocol/ change or a config key added or removed, PATCH otherwise); then a
// red `bash scripts/gate.sh full --unscoped`, which it runs; then a worktree,
// HEAD or origin/main that moved while the gate ran. Only then does it
// commit, tag and push.
//
// Usage, from shared/ (the root Makefile's `make release` runs the first):
//
//	go run ./scripts/release -root .. -version v0.1.0 [-title "..."]
//	go run ./scripts/release -root .. -check-changelog
//	go run ./scripts/release -root .. -notes v0.1.0
//
// Exit 0 = done (or clean), 1 = a refusal or a finding, 2 = usage, or a tree
// that cannot be read.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	root := flag.String("root", "..", "the repo root")
	version := flag.String("version", "", "cut this release: vMAJOR.MINOR.PATCH")
	title := flag.String("title", "", "the release's title, for its CHANGELOG.md heading")
	check := flag.Bool("check-changelog", false, "hold `## Unreleased` to the release format")
	notes := flag.String("notes", "", "print this version's CHANGELOG.md section body")
	flag.Parse()

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	modes := 0
	for _, m := range []string{"version", "check-changelog", "notes"} {
		if set[m] {
			modes++
		}
	}
	if modes != 1 || flag.NArg() > 0 || (set["title"] && !set["version"]) {
		fmt.Fprintln(os.Stderr, "usage: release [-root DIR] (-version vX.Y.Z [-title T] | -check-changelog | -notes vX.Y.Z)")
		os.Exit(2)
	}

	switch {
	case *check:
		r, err := CheckChangelog(*root)
		if err != nil {
			fmt.Fprintf(os.Stdout, "FAIL changelog  cannot read the tree: %v\n", err)
			os.Exit(2)
		}
		if !r.Print(os.Stdout) {
			os.Exit(1)
		}
	case set["notes"]:
		text, err := os.ReadFile(*root + "/" + ChangelogPath)
		if err == nil {
			var body string
			if body, err = Notes(string(text), *notes); err == nil {
				fmt.Print(body)
				return
			}
		}
		fmt.Fprintf(os.Stderr, "release -notes: %v\n", err)
		os.Exit(1)
	default:
		rel := &Release{Root: *root, Version: *version, Title: *title, Out: os.Stdout, Gate: RunGate, Now: time.Now}
		if err := rel.Run(); err != nil {
			if isRefusal(err) {
				fmt.Fprintf(os.Stderr, "release: refused: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "release: %v\n", err)
			}
			os.Exit(1)
		}
	}
}
