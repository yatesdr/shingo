package main

import (
	"strings"
	"testing"
)

func TestStyleIndex_ListsChaptersByFirstHeading(t *testing.T) {
	root := gosrcWriteTree(t, map[string]string{
		"docs/ui-style-guide/02-b.md":   "## Buttons — the rules\n\n### Sub\n",
		"docs/ui-style-guide/01-a.md":   "<!-- note -->\n## Alpha\n",
		"docs/ui-style-guide/notes.txt": "not a chapter",
	})
	got, err := styleindexList(root)
	if err != nil {
		t.Fatal(err)
	}
	want := "1. [`01-a.md`](ui-style-guide/01-a.md) — Alpha\n" +
		"1. [`02-b.md`](ui-style-guide/02-b.md) — Buttons — the rules\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	if _, err := styleindexList(gosrcWriteTree(t, map[string]string{"docs/ui-style-guide/01-x.md": "no heading\n"})); err == nil {
		t.Error("a chapter with no heading was listed")
	}
}

func TestStyleIndex_TheRealGuide(t *testing.T) {
	got, err := styleindexList(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "(ui-style-guide/04-design-tokens.md) — Design tokens\n") {
		t.Errorf("no Design tokens chapter:\n%s", got)
	}
}
