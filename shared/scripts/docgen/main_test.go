package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// repoRoot is the repo root as seen from this package's directory.
const repoRoot = "../../.."

// TestGeneratedBlocksAreCurrent is the drift test: every generated block in
// the repo's docs is exactly what its generator emits from the tree now, and
// every generator feeds at least one block. A source of truth that moved
// without `make docs-gen` fails here, naming the doc and the block.
func TestGeneratedBlocksAreCurrent(t *testing.T) {
	stale, unused, err := Run(repoRoot, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range stale {
		t.Errorf("%s:%d: the block generated from %q is stale; run make docs-gen", s.File, s.Line, s.Source)
	}
	for _, u := range unused {
		t.Errorf("generator %q feeds no block: delete it or give it a block", u)
	}
}

// TestRun_RewritesOnlyTheBlock pins the block contract on a scratch tree:
// the body between the markers is replaced, framed by one blank line each
// side; the prose around it is untouched; a second run changes nothing.
func TestRun_RewritesOnlyTheBlock(t *testing.T) {
	saved := generators
	t.Cleanup(func() { generators = saved })
	generators = map[string]Generator{}
	register(Generator{Name: "src/a.go Thing", Emit: func(_ string, args []string) (string, error) {
		return "| col |\n|---|\n| " + strings.Join(args, ",") + " |\n", nil
	}})

	root := t.TempDir()
	doc := "# Title\n\nHand prose.\n\n<!-- generated: src/a.go Thing x y -->\nold row\n<!-- /generated -->\n\nMore prose.\n"
	if err := os.WriteFile(filepath.Join(root, "d.md"), []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	stale, unused, err := Run(root, true)
	if err != nil || len(unused) != 0 || len(stale) != 1 {
		t.Fatalf("first run: stale=%v unused=%v err=%v", stale, unused, err)
	}
	got, err := os.ReadFile(filepath.Join(root, "d.md"))
	if err != nil {
		t.Fatal(err)
	}
	want := "# Title\n\nHand prose.\n\n<!-- generated: src/a.go Thing x y -->\n\n| col |\n|---|\n| x,y |\n\n<!-- /generated -->\n\nMore prose.\n"
	if string(got) != want {
		t.Fatalf("rewritten doc:\n%s\nwant:\n%s", got, want)
	}
	if stale, _, err := Run(root, false); err != nil || len(stale) != 0 {
		t.Fatalf("second run is not idempotent: stale=%v err=%v", stale, err)
	}
}

// TestParseBlocks_Refuses pins the malformed shapes: each is an error, never
// a silently skipped block.
func TestParseBlocks_Refuses(t *testing.T) {
	saved := generators
	t.Cleanup(func() { generators = saved })
	generators = map[string]Generator{}
	register(Generator{Name: "src/a.go", Emit: func(string, []string) (string, error) { return "", nil }})

	cases := map[string]string{
		"unknown generator": "<!-- generated: src/b.go -->\n<!-- /generated -->",
		"never closed":      "<!-- generated: src/a.go -->\nrow",
		"nested":            "<!-- generated: src/a.go -->\n<!-- generated: src/a.go -->\n<!-- /generated -->",
		"stray close":       "prose\n<!-- /generated -->",
	}
	for name, text := range cases {
		if _, err := parseBlocks("d.md", strings.Split(text, "\n")); err == nil {
			t.Errorf("%s: parsed without error", name)
		}
	}
}
