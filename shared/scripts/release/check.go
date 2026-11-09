package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// CheckResult is what the changelog check found.
type CheckResult struct {
	Tag      string           // the last release tag; "" when there is none
	Shallow  bool             // a shallow clone, which may simply not have the tags
	Paths    []string         // every path changed since Tag (ChangedPaths)
	Source   []string         // the source ones
	Added    map[string][]int // migrations added since Tag, by binary
	Body     []string         // the Unreleased section's lines, blank ends trimmed
	Problems []string
}

// CheckChangelog holds `## Unreleased` to the release format. Once a source
// path (IsSource) differs from the last tag, the section must be non-empty
// and open with a Deploy: line, and that line must name, per binary, exactly
// the migrations added since the tag (AddedMigrations) — or say
// "no migration" when none was. A non-empty section is held to the same
// whatever changed, and the section is the file's first (unreleased). With no
// v* tag there is nothing to compare with, and the
// result says so and passes. The error is for a tree that cannot be read;
// what the check objects to is in Problems.
func CheckChangelog(dir string) (*CheckResult, error) {
	root, err := TopLevel(dir)
	if err != nil {
		return nil, err
	}
	tag, err := LastTag(root)
	if err != nil {
		return nil, err
	}
	return checkSince(root, tag)
}

// checkSince is CheckChangelog against tag ("" for none) in the top-level root.
func checkSince(root, tag string) (*CheckResult, error) {
	r := &CheckResult{Tag: tag}
	text, readErr := os.ReadFile(filepath.Join(root, ChangelogPath))
	lines := splitLines(string(text))
	u, missing := unreleased(lines)
	if readErr == nil && missing == nil {
		r.Body = u.body(lines)
	}
	if tag == "" {
		if s, err := gitLine(root, "rev-parse", "--is-shallow-repository"); err == nil && s == "true" {
			r.Shallow = true
		}
		return r, nil
	}
	if readErr != nil {
		return nil, readErr
	}
	var err error
	if r.Paths, err = ChangedPaths(root, tag); err != nil {
		return nil, err
	}
	for _, p := range r.Paths {
		if IsSource(p) {
			r.Source = append(r.Source, p)
		}
	}
	if r.Added, err = AddedMigrations(root, tag); err != nil {
		return nil, err
	}
	if missing != nil {
		r.Problems = append(r.Problems, missing.Error())
		return r, nil
	}
	if len(r.Body) == 0 {
		if len(r.Source) > 0 {
			r.Problems = append(r.Problems, fmt.Sprintf(
				"%d source files changed since %s (%s) and `## Unreleased` is empty: open it with a Deploy: line, then a bullet per change",
				len(r.Source), tag, sample(r.Source)))
		}
		return r, nil
	}
	para := deployParagraph(r.Body)
	if para == "" {
		r.Problems = append(r.Problems, fmt.Sprintf("`## Unreleased` opens with %q, not a Deploy: line", clip(r.Body[0])))
		return r, nil
	}
	d, err := ParseDeploy(para)
	if err != nil {
		r.Problems = append(r.Problems, err.Error())
		return r, nil
	}
	r.Problems = append(r.Problems, compareDeploy(d, r.Added, tag)...)
	return r, nil
}

// compareDeploy is what the Deploy line gets wrong against the migrations
// added since tag.
func compareDeploy(d Deploy, added map[string][]int, tag string) []string {
	var problems []string
	total := 0
	for _, vs := range added {
		total += len(vs)
	}
	if total == 0 {
		named := 0
		for _, vs := range d.Named {
			named += len(vs)
		}
		if named > 0 {
			problems = append(problems, fmt.Sprintf("the Deploy line names %s, and no migration was added since %s", describe(d.Named), tag))
		} else if !d.None {
			problems = append(problems, fmt.Sprintf("no migration was added since %s, and the Deploy line does not say `no migration`", tag))
		}
		return problems
	}
	for _, reg := range Registries {
		var missing, extra []int
		for _, v := range added[reg.Binary] {
			if !slices.Contains(d.Named[reg.Binary], v) {
				missing = append(missing, v)
			}
		}
		for _, v := range d.Named[reg.Binary] {
			if !slices.Contains(added[reg.Binary], v) {
				extra = append(extra, v)
			}
		}
		if len(missing) > 0 {
			problems = append(problems, fmt.Sprintf("the Deploy line does not name %s %s, added since %s", reg.Binary, FormatVersions(missing), tag))
		}
		if len(extra) > 0 {
			problems = append(problems, fmt.Sprintf("the Deploy line names %s %s, which no migration added since %s declares", reg.Binary, FormatVersions(extra), tag))
		}
	}
	return problems
}

// describe writes per-binary versions: "Core v8–v175, Edge v2–v44".
func describe(byBinary map[string][]int) string {
	var parts []string
	for _, reg := range Registries {
		if vs := byBinary[reg.Binary]; len(vs) > 0 {
			parts = append(parts, reg.Binary+" "+FormatVersions(vs))
		}
	}
	if len(parts) == 0 {
		return "no migration"
	}
	return strings.Join(parts, ", ")
}

// sample names the first few of a list of paths.
func sample(paths []string) string {
	const n = 3
	if len(paths) <= n {
		return strings.Join(paths, ", ")
	}
	return strings.Join(paths[:n], ", ") + fmt.Sprintf(" and %d more", len(paths)-n)
}

func clip(s string) string {
	if r := []rune(s); len(r) > 60 {
		return string(r[:60]) + "…"
	}
	return s
}

// Print writes the guard's lines and reports whether the check passed.
func (r *CheckResult) Print(w io.Writer) bool {
	switch {
	case len(r.Problems) > 0:
		for _, p := range r.Problems {
			fmt.Fprintf(w, "FAIL changelog  %s  %s\n", ChangelogPath, p)
		}
		fmt.Fprintf(w, "     (the format is in %s's header; the rules are CheckChangelog in shared/scripts/release)\n", ChangelogPath)
		return false
	case r.Tag == "" && r.Shallow:
		fmt.Fprintln(w, "ok   changelog (no v* tag in this shallow clone: nothing to hold Unreleased to; check out with fetch-depth: 0)")
	case r.Tag == "":
		fmt.Fprintln(w, "ok   changelog (no v* tag yet: nothing to hold Unreleased to)")
	case len(r.Source) == 0 && len(r.Body) == 0:
		fmt.Fprintf(w, "ok   changelog (no source change since %s; Unreleased empty)\n", r.Tag)
	default:
		fmt.Fprintf(w, "ok   changelog (since %s: %d source files; Deploy names %s)\n", r.Tag, len(r.Source), describe(r.Added))
	}
	return true
}
