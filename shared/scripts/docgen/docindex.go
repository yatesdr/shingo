package main

// docindex.go — the root README's documentation index: every markdown doc
// under docs/, shingo-core/docs/ and shingo-edge/docs/, linked by its first
// heading, in one table per group.
//
// The groups are by directory, and the directory is the rule:
//
//	Shared          docs/*.md and any docs/ subdirectory not named below
//	Development     docs/dev-env/, docs/testing/, docs/fleet-simulator/
//	UI style guide  docs/ui-style-guide.md and its chapters, docs/ui-style-guide/
//	Shingo Core     shingo-core/docs/
//	Shingo Edge     shingo-edge/docs/
//
// A doc is listed by its first heading, or its file name when it has none.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

func init() {
	register(Generator{
		Name: "docs index",
		Emit: func(root string, args []string) (string, error) {
			if len(args) > 0 {
				return "", fmt.Errorf("takes no arguments, got %q", args)
			}
			return docidxEmit(root)
		},
	})
}

// docidxGroups is the display order. Each doc lands in the first group whose
// match accepts its root-relative slash path.
var docidxGroups = []struct {
	title string
	match func(p string) bool
}{
	{"Shared", func(p string) bool {
		return strings.HasPrefix(p, "docs/") && !docidxDev(p) && !docidxStyle(p)
	}},
	{"Development", docidxDev},
	{"UI style guide", docidxStyle},
	{"Shingo Core", func(p string) bool { return strings.HasPrefix(p, "shingo-core/docs/") }},
	{"Shingo Edge", func(p string) bool { return strings.HasPrefix(p, "shingo-edge/docs/") }},
}

func docidxDev(p string) bool {
	for _, d := range []string{"docs/dev-env/", "docs/testing/", "docs/fleet-simulator/"} {
		if strings.HasPrefix(p, d) {
			return true
		}
	}
	return false
}

func docidxStyle(p string) bool {
	return p == "docs/ui-style-guide.md" || strings.HasPrefix(p, "docs/ui-style-guide/")
}

// docidxATX is a markdown ATX heading line; a closing run of #s is dropped.
var docidxATX = regexp.MustCompile(`^#{1,6}\s+(.+?)(?:\s+#+)?\s*$`)

func docidxEmit(root string) (string, error) {
	var paths []string
	for _, dir := range []string{"docs", "shingo-core/docs", "shingo-edge/docs"} {
		err := filepath.WalkDir(filepath.Join(root, filepath.FromSlash(dir)), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
				return nil
			}
			rel, err := filepath.Rel(root, p)
			if err != nil {
				return err
			}
			paths = append(paths, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			return "", err
		}
	}
	sort.Strings(paths)

	var b strings.Builder
	for _, g := range docidxGroups {
		var rows []string
		for _, p := range paths {
			if !g.match(p) {
				continue
			}
			heading, err := docidxHeading(filepath.Join(root, filepath.FromSlash(p)))
			if err != nil {
				return "", err
			}
			if heading == "" {
				heading = filepath.Base(p)
			}
			rows = append(rows, fmt.Sprintf("| [%s](%s) | `%s` |", docidxCell(heading), p, p))
		}
		if len(rows) == 0 {
			continue
		}
		fmt.Fprintf(&b, "### %s\n\n| Document | File |\n|---|---|\n%s\n\n", g.title, strings.Join(rows, "\n"))
	}
	return b.String(), nil
}

// docidxHeading is a doc's first ATX heading (any level: a style guide
// chapter opens at ##) outside a fenced code block.
func docidxHeading(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	fenced := false
	for _, line := range strings.Split(strings.ReplaceAll(string(raw), "\r\n", "\n"), "\n") {
		if strings.HasPrefix(line, "```") {
			fenced = !fenced
			continue
		}
		if fenced {
			continue
		}
		if m := docidxATX.FindStringSubmatch(line); m != nil {
			return strings.TrimSpace(m[1]), nil
		}
	}
	return "", nil
}

// docidxCell keeps a heading from breaking the table or the link around it.
func docidxCell(s string) string {
	return strings.NewReplacer("|", `\|`, "[", `\[`, "]", `\]`).Replace(s)
}
