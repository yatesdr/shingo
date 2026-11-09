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
// WHO CUTS IT. The auto-release workflow (.github/workflows/auto-release.yml),
// on every push to main: -plan decides, the gate jobs run, -ci cuts, and the
// same run builds and publishes. `make release` cuts one by hand, with the
// cert gate run here.
//
// MODES.
//
//	[-version vX.Y.Z] [-title T] [-ci]  cut the release (Release.Run); without
//	                                    -version, the least version the changes
//	                                    allow; -ci skips only the local cert gate
//	-plan                               print what a cut would be, change nothing (Plan.Line first)
//	-check-changelog                    the guard scripts/check-changelog.sh runs (CheckChangelog)
//	-notes vX.Y.Z                       print that version's CHANGELOG.md section body (Notes)
//
// THE RELEASE REFUSES, cheapest first, and writes nothing when it does: a
// malformed VERSION; HEAD not on main; any change in the worktree, untracked
// included; HEAD not origin/main after a fetch; a tag that exists; a red
// changelog check, or an empty `## Unreleased`; a VERSION below the least the
// changes since the last tag allow (Required: MINOR for a migration, a
// change to protocol/'s source or a config key added or removed, PATCH
// otherwise); then, without -ci, a red `bash scripts/gate.sh full --unscoped`, which it runs,
// and a worktree, HEAD or origin/main that moved while the gate ran. Only then
// does it commit, tag and push, atomically and never forced: a push that is
// not a fast-forward is refused by origin and is reported as a refusal.
//
// Usage, from shared/ (the root Makefile's `make release` runs the first):
//
//	go run ./scripts/release -root .. [-version v0.1.1] [-title "..."]
//	go run ./scripts/release -root .. -ci
//	go run ./scripts/release -root .. -plan
//	go run ./scripts/release -root .. -check-changelog
//	go run ./scripts/release -root .. -notes v0.1.1
//
// Exit 0 = done (or clean; -plan always, when the tree can be read), 1 = a
// refusal or a finding, 2 = usage, or a tree that cannot be read.
package main

import (
	"flag"
	"fmt"
	"os"
	"time"
)

func main() {
	root := flag.String("root", "..", "the repo root")
	version := flag.String("version", "", "cut this release, vMAJOR.MINOR.PATCH; empty or absent: the least the changes allow")
	title := flag.String("title", "", "the release's title, for its CHANGELOG.md heading")
	ci := flag.Bool("ci", false, "cut without running the local cert gate (the workflow's gate jobs ran on this sha)")
	plan := flag.Bool("plan", false, "print what a cut would be and change nothing")
	check := flag.Bool("check-changelog", false, "hold `## Unreleased` to the release format")
	notes := flag.String("notes", "", "print this version's CHANGELOG.md section body")
	flag.Parse()

	set := map[string]bool{}
	flag.Visit(func(f *flag.Flag) { set[f.Name] = true })
	modes := 0
	for _, m := range []string{"plan", "check-changelog", "notes"} {
		if set[m] {
			modes++
		}
	}
	cutFlags := set["version"] || set["title"] || set["ci"]
	if modes > 1 || (modes == 1 && cutFlags) || flag.NArg() > 0 {
		fmt.Fprintln(os.Stderr, "usage: release [-root DIR] ([-version vX.Y.Z] [-title T] [-ci] | -plan | -check-changelog | -notes vX.Y.Z)")
		os.Exit(2)
	}

	switch {
	case *plan:
		p, err := PlanRelease(*root)
		if err != nil {
			fmt.Fprintf(os.Stderr, "release -plan: cannot read the tree: %v\n", err)
			os.Exit(2)
		}
		fmt.Println(p.Line())
		if p.Last != "" {
			fmt.Printf("since %s: %s\n", p.Last, p.Bump)
		}
		for _, prob := range p.Problems {
			fmt.Printf("changelog: %s\n", prob)
		}
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
		rel := &Release{Root: *root, Version: *version, Title: *title, CI: *ci, Out: os.Stdout, Gate: RunGate, Now: time.Now}
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
