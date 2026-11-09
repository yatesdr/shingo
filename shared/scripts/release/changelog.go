package main

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// ChangelogPath is the changelog's place under the repo root.
const ChangelogPath = "CHANGELOG.md"

// Unreleased is the title of the section the next release carries.
const Unreleased = "Unreleased"

// section is one `## ` heading and the lines under it, up to the next one.
type section struct {
	title string // the heading's text after "## "
	head  int    // index of the heading line
	end   int    // one past the section's last line
}

// splitLines splits text into lines, dropping each "\n" and a "\r" before it.
func splitLines(text string) []string {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	for i, l := range lines {
		lines[i] = strings.TrimSuffix(l, "\r")
	}
	return lines
}

// sections finds the level-2 headings, skipping any inside a fenced code block.
func sections(lines []string) []section {
	var out []section
	fenced := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			fenced = !fenced
			continue
		}
		if fenced || !strings.HasPrefix(l, "## ") {
			continue
		}
		if n := len(out); n > 0 {
			out[n-1].end = i
		}
		out = append(out, section{title: strings.TrimSpace(l[3:]), head: i, end: len(lines)})
	}
	return out
}

// body is the section's lines with the blank ones at either end trimmed.
func (s section) body(lines []string) []string {
	b := lines[s.head+1 : s.end]
	for len(b) > 0 && strings.TrimSpace(b[0]) == "" {
		b = b[1:]
	}
	for len(b) > 0 && strings.TrimSpace(b[len(b)-1]) == "" {
		b = b[:len(b)-1]
	}
	return b
}

// unreleased finds the one `## Unreleased` section, which is the first: a
// section above it (a dated heading in the old merge-log style) would be in no
// release.
func unreleased(lines []string) (section, error) {
	all := sections(lines)
	var found []section
	for _, s := range all {
		if s.title == Unreleased {
			found = append(found, s)
		}
	}
	switch {
	case len(found) == 0:
		return section{}, errors.New("no `## Unreleased` section")
	case len(found) > 1:
		return section{}, fmt.Errorf("%d `## Unreleased` sections; there is one", len(found))
	case all[0].title != Unreleased:
		return section{}, fmt.Errorf("`## %s` is above `## Unreleased`, so no release carries it: move its bullets into `## Unreleased`", all[0].title)
	}
	return found[0], nil
}

// DeployPrefix opens every section's first line.
const DeployPrefix = "Deploy:"

// deployParagraph is the body's Deploy paragraph, joined onto one line: its
// first line and the lines that continue it, up to a blank line, a bullet or
// a heading. "" when the body does not open with one.
func deployParagraph(body []string) string {
	if len(body) == 0 || !strings.HasPrefix(body[0], DeployPrefix) {
		return ""
	}
	parts := []string{strings.TrimSpace(body[0])}
	for _, l := range body[1:] {
		t := strings.TrimSpace(l)
		if t == "" || strings.HasPrefix(t, "- ") || strings.HasPrefix(t, "* ") || strings.HasPrefix(t, "#") {
			break
		}
		parts = append(parts, t)
	}
	return strings.Join(parts, " ")
}

// Deploy is what a Deploy paragraph says about migrations.
type Deploy struct {
	Named map[string][]int // the migration versions named, by binary
	None  bool             // it says "no migration"
}

var (
	// deployItemRe matches a binary's name, or a migration version or range
	// after it: v176, v8–v175, v8-175, v8..v175. A dotted version (v0.1.0) is
	// matched only so it can be skipped: it is a release, not a migration.
	deployItemRe  = regexp.MustCompile(`\b(Core|Edge)\b|\bv([0-9]+)((?:\.[0-9]+)*)(?:\s*(?:–|—|-|\.\.)\s*v?([0-9]+))?`)
	noMigrationRe = regexp.MustCompile(`(?i)\bno migrations?\b`)
)

// maxRange bounds one range, so a typo cannot ask for a billion versions.
const maxRange = 10000

// ParseDeploy reads the migrations a Deploy paragraph names. Every vN, or
// range vN–vM, belongs to the binary named last before it ("Core v176; Edge
// v45–v46"); a version before any binary's name is an error, because the
// paragraph has not said whose it is.
func ParseDeploy(text string) (Deploy, error) {
	d := Deploy{Named: map[string][]int{}, None: noMigrationRe.MatchString(text)}
	binary := ""
	for _, m := range deployItemRe.FindAllStringSubmatch(text, -1) {
		if m[1] != "" {
			binary = m[1]
			continue
		}
		if m[3] != "" {
			continue // a release version
		}
		lo, _ := strconv.Atoi(m[2])
		hi := lo
		if m[4] != "" {
			hi, _ = strconv.Atoi(m[4])
		}
		if binary == "" {
			return d, fmt.Errorf("the Deploy line names %s before saying whose it is (Core or Edge)", strings.TrimSpace(m[0]))
		}
		if hi < lo || hi-lo > maxRange {
			return d, fmt.Errorf("the Deploy line's range %s does not read as low–high", strings.TrimSpace(m[0]))
		}
		for v := lo; v <= hi; v++ {
			d.Named[binary] = append(d.Named[binary], v)
		}
	}
	for b, vs := range d.Named {
		slices.Sort(vs)
		d.Named[b] = slices.Compact(vs)
	}
	return d, nil
}

// Notes is the body of version's section: the lines under the heading whose
// first word is version, blank lines at either end trimmed.
func Notes(text, version string) (string, error) {
	lines := splitLines(text)
	for _, s := range sections(lines) {
		if f := strings.Fields(s.title); len(f) == 0 || f[0] != version {
			continue
		}
		b := s.body(lines)
		if len(b) == 0 {
			return "", fmt.Errorf("the %s section of %s is empty", version, ChangelogPath)
		}
		return strings.Join(b, "\n") + "\n", nil
	}
	return "", fmt.Errorf("%s has no `## %s` section", ChangelogPath, version)
}

// ReleaseHeading is a release section's heading text: version, date and,
// when there is one, title, joined by em dashes.
func ReleaseHeading(version, date, title string) string {
	h := version + " — " + date
	if title != "" {
		h += " — " + title
	}
	return h
}

// Cut renames `## Unreleased` to `## heading` and puts a fresh, empty
// `## Unreleased` above it.
func Cut(text, heading string) (string, error) {
	lines := splitLines(text)
	u, err := unreleased(lines)
	if err != nil {
		return "", err
	}
	out := make([]string, 0, len(lines)+2)
	out = append(out, lines[:u.head]...)
	out = append(out, "## "+Unreleased, "", "## "+heading)
	out = append(out, lines[u.head+1:]...)
	return strings.Join(out, "\n") + "\n", nil
}
