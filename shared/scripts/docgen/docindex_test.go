package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func docidxWrite(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Each grouping rule and heading rule, pinned on a row that only exists if
// it fired: a subdirectory of docs/ that is not named goes to Shared, the
// named ones to the style guide and Development, a chapter's ## heading is
// its title, a # inside a code fence is not, a doc with no heading is listed
// by its file name, and a heading cannot break the table.
func TestDocidx_GroupsAndHeadings(t *testing.T) {
	root := t.TempDir()
	docidxWrite(t, root, map[string]string{
		"docs/a.md":                     "# Alpha | one\n",
		"docs/state/b.md":               "```\n# not this\n```\n\n# Bravo [x]\n",
		"docs/dev-env/sim.md":           "# The sim\n",
		"docs/testing/ledger.md":        "# Ledger\n",
		"docs/ui-style-guide.md":        "# Style guide\n",
		"docs/ui-style-guide/01-how.md": "## How to use it ##\n",
		"shingo-core/docs/c.md":         "no heading at all\n",
		"shingo-edge/docs/e.md":         "# Edge doc\n",
		"shingo-core/README.md":         "# not indexed\n",
		"docs/notes.txt":                "# not markdown\n",
	})
	got, err := docidxEmit(root)
	if err != nil {
		t.Fatal(err)
	}
	order := []string{
		"### Shared",
		"| [Alpha \\| one](docs/a.md) | `docs/a.md` |",
		"| [Bravo \\[x\\]](docs/state/b.md) | `docs/state/b.md` |",
		"### UI style guide",
		"| [Style guide](docs/ui-style-guide.md) |",
		"| [How to use it](docs/ui-style-guide/01-how.md) |",
		"### Shingo Core",
		"| [c.md](shingo-core/docs/c.md) |",
		"### Shingo Edge",
		"| [Edge doc](shingo-edge/docs/e.md) |",
		"### Development",
		"| [The sim](docs/dev-env/sim.md) |",
		"| [Ledger](docs/testing/ledger.md) |",
	}
	at := 0
	for _, w := range order {
		i := strings.Index(got[at:], w)
		if i < 0 {
			t.Fatalf("missing, or out of order: %s\n--- got:\n%s", w, got)
		}
		at += i + len(w)
	}
	for _, never := range []string{"not this", "not indexed", "notes.txt"} {
		if strings.Contains(got, never) {
			t.Errorf("%q reached the index:\n%s", never, got)
		}
	}
}

// On the real tree every markdown file under the three doc trees is a row.
func TestDocidx_RealTreeIsComplete(t *testing.T) {
	got, err := docidxEmit(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, dir := range []string{"docs", "shingo-core/docs", "shingo-edge/docs"} {
		_ = filepath.WalkDir(filepath.Join(repoRoot, dir), func(p string, d os.DirEntry, err error) error {
			if err == nil && !d.IsDir() && strings.HasSuffix(p, ".md") {
				rel, rerr := filepath.Rel(repoRoot, p)
				if rerr != nil {
					t.Fatal(rerr)
				}
				if !strings.Contains(got, "(`"+filepath.ToSlash(rel)+"`)") && !strings.Contains(got, "]("+filepath.ToSlash(rel)+")") {
					t.Errorf("%s is not in the index", filepath.ToSlash(rel))
				}
				n++
			}
			return nil
		})
	}
	if n < 10 {
		t.Fatalf("walked only %d docs; the walk is not reading the tree", n)
	}
}
